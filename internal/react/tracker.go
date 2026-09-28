//declscope:namespace track

package react

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/op"
)

// Track states.
const (
	trackUnknown = "unknown"
	trackUp      = "up"
	trackDown    = "down"
)

type track struct {
	cfg config.Track
	op  trackedOp

	state      string
	pending    string
	deadline   time.Time // when pending takes effect
	since      time.Time // when the judgement changed to pending
	changes    int
	lastChange time.Time
	latestRC   op.ReturnCode
	latestRTT  time.Duration
}

type trackedOp struct {
	typ    config.OpType
	target string
	tag    string
	vrf    string
}

// Tracker maintains the tracks ("track N ip sla OP state|reachability").
// Observe judges each result; state changes wait for delay.up / delay.down,
// which one goroutine (Run) times for every track.
type Tracker struct {
	clk    clock.Clock
	bus    *event.Bus
	logger *slog.Logger
	wake   chan struct{}

	mu     sync.Mutex
	tracks map[int]*track
	byOp   map[int][]*track
}

// NewTracker returns a tracker publishing on bus. clk may be nil (real
// clock).
func NewTracker(clk clock.Clock, bus *event.Bus, logger *slog.Logger) *Tracker {
	if clk == nil {
		clk = clock.Real()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracker{
		clk: clk, bus: bus, logger: logger,
		wake:   make(chan struct{}, 1),
		tracks: map[int]*track{},
		byOp:   map[int][]*track{},
	}
}

// Configure replaces the tracks. A track with the same ID, operation and
// mode as before keeps its state (and a running delay, re-timed with the new
// delay), so that a reload does not flap a failover. Other tracks start
// unknown; a track whose operation is missing from ops stays unknown.
// Nothing is published.
func (t *Tracker) Configure(tracks []config.Track, ops map[int]*config.Operation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make(map[int]*track, len(tracks))
	byOp := make(map[int][]*track)
	for _, c := range tracks {
		tr := &track{cfg: c, state: trackUnknown}
		if old, ok := t.tracks[c.ID]; ok && old.cfg.Operation == c.Operation && old.cfg.Mode == c.Mode {
			*tr = *old
			tr.cfg = c
			if tr.pending != "" {
				tr.deadline = tr.since.Add(tr.delay(tr.pending))
			}
		}
		if o, ok := ops[c.Operation]; ok && o != nil {
			tr.op = trackedOp{typ: o.Type, target: o.Target.String(), tag: o.Tag, vrf: o.VRF}
		} else {
			if tr.state != trackUnknown {
				t.logger.Info("track operation is gone; track is unknown", "track", c.ID, "op", c.Operation, "was", tr.state)
			}
			tr.state, tr.pending = trackUnknown, ""
		}
		next[c.ID] = tr
		byOp[c.Operation] = append(byOp[c.Operation], tr)
	}
	t.tracks, t.byOp = next, byOp
	t.poke()
}

// SetTag updates the tag the events of the tracks of operation opID carry,
// keeping their state.
func (t *Tracker) SetTag(opID int, tag string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.byOp[opID] {
		tr.op.tag = tag
	}
}

func (tr *track) delay(to string) time.Duration {
	if to == trackUp {
		return tr.cfg.DelayUp
	}
	return tr.cfg.DelayDown
}

// judgeTrack maps a return code to up or down for a mode.
func judgeTrack(mode string, rc op.ReturnCode) string {
	if rc == op.RCOK || (mode == config.TrackReachability && rc == op.RCOverThreshold) {
		return trackUp
	}
	return trackDown
}

// Observe judges a result for the tracks of its operation. Busy and
// sequence errors are ignored.
func (t *Tracker) Observe(r op.Result) {
	if r.Code == op.RCBusy || r.Code == op.RCSequenceError {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clk.Now()
	poke := false
	for _, tr := range t.byOp[r.OpID] {
		tr.latestRC = r.Code
		tr.latestRTT = r.RTT
		j := judgeTrack(tr.cfg.Mode, r.Code)
		switch {
		case j == tr.state:
			tr.pending = "" // back before the delay ran out: no change
		case tr.state == trackUnknown:
			tr.pending = ""
			t.changeLocked(tr, j, now) // the first judgement takes effect at once
		case j == tr.pending:
			// the delay is already running
		default:
			d := tr.delay(j)
			if d <= 0 {
				tr.pending = ""
				t.changeLocked(tr, j, now)
				continue
			}
			tr.pending, tr.since, tr.deadline = j, now, now.Add(d)
			poke = true
		}
	}
	if poke {
		t.poke()
	}
}

func (t *Tracker) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *Tracker) changeLocked(tr *track, to string, now time.Time) {
	from := tr.state
	tr.state = to
	tr.changes++
	tr.lastChange = now
	kind := event.TrackDown
	if to == trackUp {
		kind = event.TrackUp
	}
	ev := event.Event{
		Time:       now,
		Kind:       kind,
		OpID:       tr.cfg.Operation,
		Type:       tr.op.typ,
		Target:     tr.op.target,
		Tag:        tr.op.tag,
		VRF:        tr.op.vrf,
		TrackID:    tr.cfg.ID,
		TrackMode:  tr.cfg.Mode,
		TrackState: to,
		LatestRC:   tr.latestRC.String(),
		Message:    event.TrackMessage(tr.cfg.ID, tr.cfg.Operation, tr.cfg.Mode, from, to),
	}
	if tr.latestRC.HasRTT() {
		ms := op.RTTMillis(tr.latestRTT)
		ev.LatestRTTMs = &ms
	}
	if t.bus != nil {
		t.bus.Publish(ev)
	}
}

// Run applies the delayed changes when their delay runs out, until ctx is
// done. One timer serves every track.
func (t *Tracker) Run(ctx context.Context) error {
	timer := t.clk.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		t.mu.Lock()
		now := t.clk.Now()
		var next time.Time
		for _, tr := range t.tracks {
			if tr.pending == "" {
				continue
			}
			if !tr.deadline.After(now) {
				to := tr.pending
				tr.pending = ""
				t.changeLocked(tr, to, now)
				continue
			}
			if next.IsZero() || tr.deadline.Before(next) {
				next = tr.deadline
			}
		}
		t.mu.Unlock()

		wait := time.Hour
		if !next.IsZero() {
			wait = next.Sub(now)
		}
		timer.Stop()
		timer.Reset(wait)
		// The timer counts wait from the clock at Reset, not from now. If the
		// clock moved in between, it would expire after next; look again
		// instead of waiting for it.
		if !next.IsZero() && !t.clk.Now().Before(next) {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.wake:
		case <-timer.C():
		}
	}
}

// Snapshot returns the state of track id.
func (t *Tracker) Snapshot(id int) (TrackState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.tracks[id]
	if !ok {
		return TrackState{}, false
	}
	return tr.snapshot(), true
}

// All returns every track, sorted by ID.
func (t *Tracker) All() []TrackState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TrackState, 0, len(t.tracks))
	for _, tr := range t.tracks {
		out = append(out, tr.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (tr *track) snapshot() TrackState {
	return TrackState{
		ID: tr.cfg.ID, Operation: tr.cfg.Operation, Mode: tr.cfg.Mode,
		State: tr.state, Pending: tr.pending, Changes: tr.changes, LastChange: tr.lastChange,
		LatestRC: tr.latestRC, LatestRTT: tr.latestRTT,
		DelayUp: tr.cfg.DelayUp, DelayDown: tr.cfg.DelayDown,
	}
}
