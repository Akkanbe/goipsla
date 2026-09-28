// The schedule state machine is methods of Manager filed apart from
// manager.go: it reads and writes the operation set, the lock and the
// schedule fields of operation that manager.go declares, and manager.go
// calls back into it (Start → initLocked). One device, one namespace.
//
//declscope:namespace manager

package manager

import (
	"context"
	"time"

	"goipsla/internal/config"
	"goipsla/internal/sched"
)

// The schedule state machine follows rttMonCtrlOperState (see
// docs/scheduling.md):
//
//	start now            → active at once; first attempt after the dispersion offset
//	start after / at     → pending; active at the start time
//	start pending        → pending until a reload changes the start
//	life reaches 0       → inactive (statistics frozen)
//	recurring            → active again every day at the start time (new life)
//	ageout reaches 0     → deleted (only counts while not active)
//	restart              → new statistics life, life and Seq reset (active only)
//	reset                → every operation back to its initial transition
//
// Timed transitions are entries of m.events, a second Scheduler keyed by
// eventKey, so that thousands of operations with a finite life do not need
// a timer each. All transitions run with m.mu held.

type eventKind int

const (
	evStart eventKind = iota
	evLifeEnd
	evAgeout
	numEventKinds
)

func eventKey(id int, k eventKind) int { return id*int(numEventKinds) + int(k) }

// oneShot is the frequency of events that fire once: long enough never to
// fire twice, and the handler removes the entry anyway.
const oneShot = 100 * 365 * 24 * time.Hour

// day is the period of recurring schedules. It is 24 hours of elapsed time,
// so a daylight saving change shifts the local start time by an hour.
const day = 24 * time.Hour

// storeAdd starts a new statistics life of c at start.
func (m *Manager) storeAdd(c *config.Operation, start time.Time) {
	m.lives[c.ID]++
	if m.store != nil {
		m.store.Add(c, start)
	}
}

// initLocked puts o into the initial state of its schedule at now: the
// transition made at startup, for operations added by a reload and by
// reset. It also starts the statistics row of o.
func (m *Manager) initLocked(o *operation, now time.Time) {
	c := o.config()
	if m.react != nil {
		m.react.Configure(c)
	}
	if o.runner == nil { // skipped type
		o.setState(stateInactive)
		m.storeAdd(c, now)
		o.freshLife = true
		return
	}
	s := c.Schedule
	switch {
	case s == nil || s.Start == config.StartNow || s.Start == "":
		// Disperse the operations over one period so that they do not all
		// send at the same instant (Cisco group schedule). The life starts
		// at the first attempt.
		m.activateLocked(o, now.Add(sched.Offset(c.ID, c.Frequency)), true)
	case s.Start == config.StartAfter || s.Start == config.StartAt:
		start := now.Add(s.After)
		if s.Start == config.StartAt {
			start = s.At
		}
		if !start.After(now) && !s.Recurring {
			// A start time in the past starts at once.
			m.activateLocked(o, now, true)
			return
		}
		o.setState(statePending)
		o.nextStart = start
		m.logger.Info("operation pending until its start time", append(opAttrs(o), "start", start)...)
		m.storeAdd(c, start)
		o.freshLife = true
		freq := oneShot
		if s.Recurring {
			freq = day
		}
		m.events.Add(sched.Entry{
			ID:        eventKey(o.id, evStart),
			Frequency: freq,
			First:     start,
			Run:       func(_ context.Context, at time.Time) { m.onStart(o, at) },
		})
		m.startAgeoutLocked(o, now)
	default: // pending
		o.setState(statePending)
		m.logger.Info("operation pending (start-time pending); a reload that changes start-time starts it", opAttrs(o)...)
		m.storeAdd(c, now)
		o.freshLife = true
		m.startAgeoutLocked(o, now)
	}
}

// activateLocked makes o active with a new life starting at first, the time
// of its first attempt. newRow tells that the statistics row must start a
// life at first (initial activation); otherwise the life prepared by
// initLocked is used if it is still fresh.
func (m *Manager) activateLocked(o *operation, first time.Time, newRow bool) {
	c := o.config()
	if newRow || !o.freshLife {
		m.storeAdd(c, first)
	}
	o.freshLife = false
	o.lifeStart = first
	o.gen.Add(1) // results of attempts of the previous life are dropped
	o.seq.Store(0)
	o.forgetStarts()
	if m.react != nil {
		// A new life: occurred false and empty windows, no notification.
		m.react.Reset(o.id)
	}
	o.setState(stateActive)
	o.nextStart = time.Time{}
	m.stopAgeoutLocked(o)
	m.armLifeLocked(o, first)
	end := o.lifeEnd
	m.sched.Add(sched.Entry{
		ID:        o.id,
		Frequency: c.Frequency,
		First:     first,
		// The scheduler passes the lattice time; results carry the time
		// the attempt actually started instead (see attempt). A lattice
		// point at or after the end of the life belongs to no life: the
		// life-end event and the attempt may fire at the same instant.
		Run: func(ctx context.Context, at time.Time) {
			if !end.IsZero() && !at.Before(end) {
				return
			}
			m.attempt(ctx, o)
		},
		Busy: func(time.Time) { m.busy(o) },
	})
}

// armLifeLocked sets the end of o's life to start + life (nothing for a
// forever life).
func (m *Manager) armLifeLocked(o *operation, start time.Time) {
	s := o.config().Schedule
	if s == nil || s.Forever {
		o.lifeEnd = time.Time{}
		m.events.Remove(eventKey(o.id, evLifeEnd))
		return
	}
	o.lifeEnd = start.Add(s.Life)
	m.events.Add(sched.Entry{
		ID:        eventKey(o.id, evLifeEnd),
		Frequency: oneShot,
		First:     o.lifeEnd,
		Run:       func(_ context.Context, at time.Time) { m.onLifeEnd(o, at) },
	})
}

// startAgeoutLocked starts counting o's ageout from now (non-active states).
func (m *Manager) startAgeoutLocked(o *operation, now time.Time) {
	s := o.config().Schedule
	if s == nil || s.Ageout <= 0 {
		return
	}
	o.ageoutAt = now.Add(s.Ageout)
	m.events.Add(sched.Entry{
		ID:        eventKey(o.id, evAgeout),
		Frequency: oneShot,
		First:     o.ageoutAt,
		Run:       func(_ context.Context, at time.Time) { m.onAgeout(o, at) },
	})
}

func (m *Manager) stopAgeoutLocked(o *operation) {
	o.ageoutAt = time.Time{}
	m.events.Remove(eventKey(o.id, evAgeout))
}

// current reports whether o is still the live object of its ID.
func (m *Manager) currentLocked(o *operation) bool { return m.ops[o.id] == o }

// onStart is the start-time event of a pending (after / at) or recurring
// operation.
func (m *Manager) onStart(o *operation, at time.Time) {
	m.lockLife()
	defer m.unlockLife()
	if !m.currentLocked(o) || o.getState() == stateActive {
		return
	}
	s := o.config().Schedule
	if !s.Recurring {
		m.events.Remove(eventKey(o.id, evStart))
	}
	m.logger.Info("operation started", append(opAttrs(o), "life_start", at)...)
	m.activateLocked(o, at, false)
}

// onLifeEnd ends the life of an active operation: it becomes inactive and
// its statistics stay frozen in the store.
func (m *Manager) onLifeEnd(o *operation, at time.Time) {
	m.lockLife()
	defer m.unlockLife()
	if !m.currentLocked(o) || !o.lifeEnd.Equal(at) || o.getState() != stateActive {
		return
	}
	m.events.Remove(eventKey(o.id, evLifeEnd))
	m.sched.Remove(o.id)
	o.lifeEnd = time.Time{}
	o.setState(stateInactive)
	if s := o.config().Schedule; s.Recurring {
		o.nextStart = nextDaily(s.At, at)
	}
	m.startAgeoutLocked(o, at)
	if o.nextStart.IsZero() {
		m.logger.Info("operation life ended", opAttrs(o)...)
	} else {
		m.logger.Info("operation life ended", append(opAttrs(o), "next_start", o.nextStart)...)
	}
}

// nextDaily returns the first time origin + k×day (k ≥ 0) after t.
func nextDaily(origin, t time.Time) time.Time {
	if origin.After(t) {
		return origin
	}
	k := t.Sub(origin)/day + 1
	return origin.Add(k * day)
}

// onAgeout deletes an operation whose ageout ran out while it was not
// active. It stays in the configuration file and comes back on the next
// reload.
func (m *Manager) onAgeout(o *operation, at time.Time) {
	m.lockLife()
	defer m.unlockLife()
	if !m.currentLocked(o) || !o.ageoutAt.Equal(at) || o.getState() == stateActive {
		return
	}
	m.deleteLocked(o)
	m.configureTracksLocked()
	m.logger.Info("operation aged out and was deleted; it returns on the next reload", opAttrs(o)...)
}

// stopLocked stops everything scheduled for o. Its in-flight attempt is
// canceled and its result dropped.
func (m *Manager) stopLocked(o *operation) {
	o.gen.Add(1) // results of attempts already running are dropped
	m.sched.Remove(o.id)
	for k := eventKind(0); k < numEventKinds; k++ {
		m.events.Remove(eventKey(o.id, k))
	}
	o.lifeEnd, o.nextStart, o.ageoutAt, o.lifeStart = time.Time{}, time.Time{}, time.Time{}, time.Time{}
}

// deleteLocked stops o and removes it from the manager and the store.
func (m *Manager) deleteLocked(o *operation) {
	m.stopLocked(o)
	delete(m.ops, o.id)
	delete(m.lives, o.id)
	if m.store != nil {
		m.store.Remove(o.id)
	}
	if m.react != nil {
		m.react.Remove(o.id)
	}
	if m.eng != nil {
		// Free the operation's ICMP Identifier in the engine; a re-created
		// operation gets one again on its first request.
		m.eng.Forget(uint32(o.id))
	}
}

// Restart starts a new life of an active operation now: its statistics are
// discarded (Store.Reset), its life is reloaded from the configuration, its
// Seq restarts at 1 and its first attempt of the new life is made at once.
// It fails with ErrNotActive if the operation is not active (as Cisco's
// "ip sla restart"), and ErrNotFound if there is no such operation.
func (m *Manager) Restart(id int) error {
	m.lockLife()
	defer m.unlockLife()
	if !m.running {
		return errNotRunning
	}
	o, ok := m.ops[id]
	if !ok {
		return ErrNotFound
	}
	if o.getState() != stateActive {
		return ErrNotActive
	}
	now := m.clk.Now()
	if m.store != nil {
		m.store.Reset(id, now)
	}
	m.lives[id]++
	o.freshLife = true
	m.activateLocked(o, now, false)
	m.logger.Info("operation restarted", opAttrs(o)...)
	return nil
}

// Reset discards the statistics of every operation and puts every operation
// of the current configuration back into its initial schedule transition,
// as at startup. Operations deleted by ageout come back. Unlike Cisco's
// "ip sla reset", the configuration is kept: the file is the source of
// truth.
func (m *Manager) Reset() error {
	m.lockLife()
	defer m.unlockLife()
	if !m.running {
		return errNotRunning
	}
	for _, o := range m.ops {
		m.stopLocked(o)
	}
	m.ops = make(map[int]*operation, len(m.cfg.Operations))
	now := m.clk.Now()
	for _, c := range m.cfg.Operations {
		o := m.newOperation(c)
		m.ops[c.ID] = o
		m.initLocked(o, now)
	}
	m.configureTracksLocked()
	m.logger.Info("all operations reset", "operations", len(m.ops))
	return nil
}

// configureTracksLocked hands the tracks of the current configuration and
// the current operations to the tracker. A track keeps its state when its
// operation and mode are unchanged; a track whose operation is gone (removed
// by a reload or aged out) returns to unknown without notification.
func (m *Manager) configureTracksLocked() {
	if m.track == nil {
		return
	}
	ops := make(map[int]*config.Operation, len(m.ops))
	for id, o := range m.ops {
		ops[id] = o.config()
	}
	m.track.Configure(m.cfg.Tracks, ops)
}

// opAttrs are the log attributes that name an operation: its ID, type and
// target (the contract's op / type / target).
func opAttrs(o *operation) []any {
	c := o.config()
	return []any{"op", o.id, "type", string(c.Type), "target", c.Target.String()}
}
