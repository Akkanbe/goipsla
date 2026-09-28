// Package manager creates the operations of a configuration, runs their
// schedule state machine (rttMonCtrlOperState), applies reloads and delivers
// the results of their attempts.
package manager

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/probe"
	"goipsla/internal/react"
	"goipsla/internal/sched"
	"goipsla/internal/stats"
)

// Operational states, as in rttMonCtrlOperState.
const (
	statePending  = "pending"
	stateInactive = "inactive"
	stateActive   = "active"
)

// Errors of Restart and the other control calls.
var (
	ErrNotFound   = errors.New("operation not found")
	ErrNotActive  = errors.New("operation is not active")
	errNotRunning = errors.New("manager is not running")
)

// Manager owns the operations of one configuration.
type Manager struct {
	clk    clock.Clock
	logger *slog.Logger
	eng    probe.Engine
	sched  *sched.Scheduler // attempts, keyed by operation ID
	events *sched.Scheduler // schedule events, keyed by eventKey
	store  *stats.Store     // nil unless SetStore was called
	react  *react.Engine    // nil unless SetReactions was called
	track  *react.Tracker   // nil unless SetReactions was called

	// sinkMu serializes the sink. Lock order: sinkMu before mu; lockLife
	// takes both for a change that starts or ends lives.
	sinkMu sync.Mutex
	sink   func(op.Result)
	// lives is the index of each operation's current statistics life,
	// counted as stats.Store counts LifeIndex (from 1, +1 per new life,
	// back to 1 after deletion). Changed under lockLife, read under sinkMu.
	lives map[int]int

	// mu guards the operation set and every schedule field of the
	// operations. Attempts do not take it.
	mu sync.RWMutex
	// cfg is the configuration last loaded (the start or the last reload):
	// its operations and tracks are applied. Its global section and actions
	// are not, since they take effect at startup only; effective holds the
	// ones in force.
	cfg       *config.Config
	effective effectiveSettings
	ops       map[int]*operation
	started   bool // Start has run
	running   bool // started and Run has not returned: control calls are accepted
}

// effectiveSettings are the settings read at startup only: the global
// section and the actions (their sinks are built once). A reload compares
// the file against them, not against the previous file, so that it warns as
// long as the file and the running daemon differ.
type effectiveSettings struct {
	global  config.Global
	actions []config.Action
}

// Counts summarizes the current states of the operations.
type Counts struct {
	Total    int
	Active   int
	Pending  int
	Inactive int // includes Skipped
	Skipped  int // types not implemented yet
}

// Info is the schedule information of one operation.
type Info struct {
	State string
	// LifeLeft is the remaining life of an active operation, 0 when it is
	// not active, and nil when the life is forever.
	LifeLeft *time.Duration
	// NextStart is the next scheduled start of a pending or inactive
	// operation (start-time after / at / recurring); zero if none.
	NextStart time.Time
	// AgeoutLeft is the time until a non-active operation is aged out; nil
	// when ageout is disabled or the operation is active.
	AgeoutLeft *time.Duration
}

// operation is the runtime state of one configured operation. A reload that
// changes how an operation measures replaces the whole object.
type operation struct {
	id     int
	cfg    atomic.Pointer[config.Operation] // swapped by reloads that change only tag / owner / react
	runner op.Runner                        // nil when the type is not implemented (skipped)
	seq    atomic.Uint32
	state  atomic.Value // string
	// gen is the life generation: it advances when a life starts and when
	// the operation is stopped, so that the result of an attempt of an
	// earlier life is not delivered (see deliverAttempt).
	gen atomic.Uint64

	// Schedule fields, guarded by Manager.mu.
	lifeEnd   time.Time // active with a finite life: when it ends
	nextStart time.Time // pending / inactive with a known start
	ageoutAt  time.Time // non-active with ageout: when it is deleted
	lifeStart time.Time // active: when the current life started (its first attempt)
	freshLife bool      // the statistics life started by init has not been used yet

	// recent remembers the start times of the last attempts so that a late
	// reply can be reported with the start of the attempt it belongs to.
	recentMu sync.Mutex
	recent   [recentAttempts]attemptStart
}

// recentAttempts is how many attempt start times an operation remembers.
// With frequency > timeout a late reply belongs to one of the last two
// attempts; a few more cover duplicates that arrive later.
const recentAttempts = 8

type attemptStart struct {
	seq   uint32
	start time.Time
}

func (o *operation) config() *config.Operation { return o.cfg.Load() }
func (o *operation) getState() string          { return o.state.Load().(string) }
func (o *operation) setState(s string)         { o.state.Store(s) }

func (o *operation) rememberStart(seq uint32, start time.Time) {
	o.recentMu.Lock()
	o.recent[seq%recentAttempts] = attemptStart{seq: seq, start: start}
	o.recentMu.Unlock()
}

// startOf returns the start time of attempt seq if it is still remembered.
func (o *operation) startOf(seq uint32) (time.Time, bool) {
	o.recentMu.Lock()
	defer o.recentMu.Unlock()
	a := o.recent[seq%recentAttempts]
	if a.seq != seq || a.start.IsZero() {
		return time.Time{}, false
	}
	return a.start, true
}

func (o *operation) forgetStarts() {
	o.recentMu.Lock()
	o.recent = [recentAttempts]attemptStart{}
	o.recentMu.Unlock()
}

// New builds the operations of cfg. Results of attempts, busies and late
// replies are passed to sink, which is never called concurrently. eng is
// shared by every operation. A nil logger means slog.Default().
//
// New only prepares the operations; their schedules start in Run.
func New(cfg *config.Config, eng probe.Engine, clk clock.Clock, sink func(op.Result), logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if sink == nil {
		sink = func(op.Result) {}
	}
	m := &Manager{
		clk:    clk,
		logger: logger,
		eng:    eng,
		sched:  sched.New(clk, logger),
		events: sched.New(clk, logger),
		sink:   sink,
		lives:  make(map[int]int, len(cfg.Operations)),
		cfg:    cfg,
		effective: effectiveSettings{
			global:  cfg.Global,
			actions: slices.Clone(cfg.Actions),
		},
		ops: make(map[int]*operation, len(cfg.Operations)),
	}
	for _, c := range cfg.Operations {
		m.ops[c.ID] = m.newOperation(c)
	}
	return m
}

// newOperation builds the runtime object of c in its initial state
// (pending, or inactive when the type is not implemented yet).
func (m *Manager) newOperation(c *config.Operation) *operation {
	o := &operation{id: c.ID}
	o.cfg.Store(c)
	o.setState(statePending)
	switch c.Type {
	case config.ICMPEcho:
		o.runner = op.NewEcho(c, m.eng, m.clk)
	case config.ICMPJitter:
		o.runner = op.NewJitter(c, m.eng, m.clk)
	default:
		// A type the configuration accepts but no Runner implements yet.
		o.setState(stateInactive)
		m.logger.Warn(string(c.Type)+" is not implemented yet; skipped", "op", c.ID, "type", string(c.Type), "target", c.Target.String())
	}
	return o
}

// SetStore makes the manager start and end the statistics lives of the
// operations in s. It must be called before Start.
func (m *Manager) SetStore(s *stats.Store) { m.store = s }

// SetReactions makes the manager keep the reaction engine and the tracker in
// step with the operations: the reactions of an operation are configured
// when it is created and reset (occurred false, windows empty, no
// notification) when a new life starts; the tracks are reconfigured when the
// set of operations changes. Either may be nil. It must be called before
// Start. Results reach them through the sink, not through the manager.
func (m *Manager) SetReactions(e *react.Engine, t *react.Tracker) {
	m.react, m.track = e, t
}

// Counts reports how many operations are in each state.
func (m *Manager) Counts() Counts {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var n Counts
	for _, o := range m.ops {
		n.Total++
		switch o.getState() {
		case stateActive:
			n.Active++
		case statePending:
			n.Pending++
		default:
			n.Inactive++
			if o.runner == nil {
				n.Skipped++
			}
		}
	}
	return n
}

// IDs returns the IDs of the current operations in ascending order.
// Operations removed by ageout are not included.
func (m *Manager) IDs() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedIDs(m.ops)
}

// Config returns the effective configuration of operation id as currently
// applied (it follows reloads).
func (m *Manager) Config(id int) (*config.Operation, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.ops[id]
	if !ok {
		return nil, false
	}
	return o.config(), true
}

// State returns the operational state of operation id ("pending", "inactive"
// or "active"). ok is false if the ID is unknown.
func (m *Manager) State(id int) (state string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.ops[id]
	if !ok {
		return "", false
	}
	return o.getState(), true
}

// Info returns the schedule information of operation id.
func (m *Manager) Info(id int) (Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.ops[id]
	if !ok {
		return Info{}, false
	}
	now := m.clk.Now()
	info := Info{State: o.getState(), NextStart: o.nextStart}
	if s := o.config().Schedule; s != nil && !s.Forever {
		left := time.Duration(0)
		if info.State == stateActive && !o.lifeEnd.IsZero() {
			left = max(o.lifeEnd.Sub(now), 0)
		}
		info.LifeLeft = &left
	}
	if !o.ageoutAt.IsZero() {
		left := max(o.ageoutAt.Sub(now), 0)
		info.AgeoutLeft = &left
	}
	return info, true
}

// Start puts every operation into the initial transition of its schedule
// and starts its statistics life (Store.Add). After Start, the state,
// schedule and statistics of every operation can be read and the control
// calls (Reload, Restart, Reset) work; attempts and timed transitions begin
// when Run runs the schedulers. The daemon calls Start before it publishes
// the API, so that no client sees an operation without its statistics row.
// Start is idempotent; Run calls it if it has not been called.
func (m *Manager) Start() {
	m.lockLife()
	defer m.unlockLife()
	if m.started {
		return
	}
	now := m.clk.Now()
	for _, id := range sortedIDs(m.ops) {
		m.initLocked(m.ops[id], now)
	}
	m.configureTracksLocked()
	m.started = true
	m.running = true
}

// Run runs the schedules of the operations until ctx is done (calling Start
// first if needed). It returns nil on cancellation.
func (m *Manager) Run(ctx context.Context) error {
	m.Start()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	done := make(chan error, 1)
	go func() { done <- m.events.Run(ctx) }()
	err := m.sched.Run(ctx)
	if eerr := <-done; err == nil {
		err = eerr
	}
	return err
}

func sortedIDs(ops map[int]*operation) []int {
	ids := make([]int, 0, len(ops))
	for id := range ops {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// attempt runs one attempt of o and delivers its result. Result.Start is the
// time the attempt actually started (clk.Now()), not the lattice time it was
// scheduled for: the two differ by the timer latency, and Start should say
// when the Echo Request went out.
func (m *Manager) attempt(ctx context.Context, o *operation) {
	gen := o.gen.Load()
	seq := o.seq.Add(1)
	start := m.clk.Now()
	o.rememberStart(seq, start)
	res := o.runner.Run(ctx, seq, start)
	if ctx.Err() != nil {
		// The daemon is stopping, or the operation was stopped, removed or
		// restarted: the attempt was cut short and its outcome says
		// nothing about the target.
		m.logger.Debug("attempt abandoned", "op", o.id, "seq", seq)
		return
	}
	m.deliverAttempt(o, gen, res)
}

// deliverAttempt passes the result of an attempt started in life generation
// gen to the sink, unless the operation has since been restarted, stopped or
// removed: such a result belongs to no current life, and would otherwise
// count in the new life's reactions and tracks (audit C3 / C12). The check
// and the sink run under sinkMu, which every life change also holds.
func (m *Manager) deliverAttempt(o *operation, gen uint64, res op.Result) {
	m.sinkMu.Lock()
	defer m.sinkMu.Unlock()
	if o.gen.Load() != gen {
		m.logger.Debug("result of an earlier life dropped", "op", o.id, "seq", res.Seq, "rc", res.Code.String())
		return
	}
	m.sinkLocked(o, res)
}

// lockLife takes the locks for a change that starts or ends lives: sinkMu
// first (no result is being delivered meanwhile), then mu.
func (m *Manager) lockLife() {
	m.sinkMu.Lock()
	m.mu.Lock()
}

func (m *Manager) unlockLife() {
	m.mu.Unlock()
	m.sinkMu.Unlock()
}

// busy reports an attempt that was not started because the previous one is
// still outstanding. No sequence number is consumed (Seq is 0). Start and End
// are the time the busy was detected (clk.Now()).
func (m *Manager) busy(o *operation) {
	now := m.clk.Now()
	m.deliver(o, op.Result{OpID: o.id, Type: o.config().Type, Start: now, End: now, Code: op.RCBusy})
}

// LateReply reports a reply that matched a request of operation opID after
// its timeout, or a duplicate reply. It is meant for probe.Options.OnLateReply;
// sentAt is when the answered request was sent. It passes a sequenceError
// result to the sink, with Start the start of the attempt the reply belongs
// to and End the time of the report.
//
// The reply is dropped (debug log only) unless it belongs to an attempt of
// the current life: the operation must be active, seq one of its recent
// attempts, and sentAt neither before that attempt's start nor before the
// life's start. Seq restarts at 1 with every life (restart, recurring,
// re-creation by reload), so the same seq may name an attempt of the new
// life when probe reports a reply to the old one after the restart
// (review R2); the send time tells them apart. The check and the delivery
// run under sinkMu, which every life change also holds.
func (m *Manager) LateReply(opID, seq uint32, sentAt time.Time) {
	m.sinkMu.Lock()
	defer m.sinkMu.Unlock()
	m.mu.RLock()
	o, ok := m.ops[int(opID)]
	var lifeStart time.Time
	if ok {
		lifeStart = o.lifeStart
	}
	m.mu.RUnlock()
	if !ok {
		m.logger.Debug("late reply for unknown operation", "op", opID, "seq", seq)
		return
	}
	// A life that ended (onLifeEnd) keeps its statistics frozen: a late
	// reply to its last attempts is not counted either. The state changes
	// only under lockLife, which holds sinkMu.
	if st := o.getState(); st != stateActive {
		m.logger.Debug("late reply for an operation that is not active; dropped",
			"op", opID, "seq", seq, "state", st, "sent_at", sentAt)
		return
	}
	start, ok := o.startOf(seq)
	if !ok || lifeStart.IsZero() || sentAt.Before(start) || sentAt.Before(lifeStart) {
		m.logger.Debug("late reply for an attempt not in the current life; dropped",
			"op", opID, "seq", seq, "sent_at", sentAt)
		return
	}
	m.sinkLocked(o, op.Result{
		OpID:   o.id,
		Type:   o.config().Type,
		Seq:    seq,
		Start:  start,
		End:    m.clk.Now(),
		Code:   op.RCSequenceError,
		Detail: "late or duplicate reply",
	})
}

func (m *Manager) deliver(o *operation, res op.Result) {
	m.sinkMu.Lock()
	defer m.sinkMu.Unlock()
	m.sinkLocked(o, res)
}

// sinkLocked passes res to the sink with the index of o's current life
// (Result.Life, as the statistics count it). m.sinkMu must be held; lives
// change only under it (lockLife).
func (m *Manager) sinkLocked(o *operation, res op.Result) {
	res.Life = m.lives[o.id]
	m.sink(res)
}
