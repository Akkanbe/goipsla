//declscope:core // Engine is the unit the react package is named for (evaluating reactions, the subject of the package doc)

// Package react evaluates the threshold reactions ("ip sla
// reaction-configuration") and the object tracking ("track ip sla") of the
// operations, and publishes their state changes on an event.Bus.
//
// The rules are those of docs/reactions.md:
// one notification per rising or falling transition, never / immediate /
// consecutive / xofy / average, and state / reachability tracking with
// up and down delays.
package react

import (
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/op"
)

// class is the classification of one value against the thresholds.
type class int8

const (
	neutral class = iota
	violation
	recovery
)

// reaction is one reaction row with its evaluation state.
type reaction struct {
	cfg     config.Reaction
	boolean bool

	occurred   bool
	value      int64
	lastChange time.Time
	changes    int

	runViolate, runRecover int     // consecutive
	classes                []class // xofy: the last Y classifications
	values                 []int64 // average: the last Count values
}

func (r *reaction) reset() {
	r.occurred = false
	r.value = 0
	r.runViolate, r.runRecover = 0, 0
	r.classes = r.classes[:0]
	r.values = r.values[:0]
}

// opReactions are the reactions of one operation.
type opReactions struct {
	id     int
	typ    config.OpType
	target string
	tag    string
	vrf    string
	rows   []*reaction
}

// Engine evaluates the reactions of every operation. It is safe for
// concurrent use.
type Engine struct {
	clk    clock.Clock
	bus    *event.Bus
	logger *slog.Logger

	mu  sync.Mutex
	ops map[int]*opReactions
}

// NewEngine returns an engine publishing on bus. clk may be nil (real clock).
func NewEngine(clk clock.Clock, bus *event.Bus, logger *slog.Logger) *Engine {
	if clk == nil {
		clk = clock.Real()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{clk: clk, bus: bus, logger: logger, ops: map[int]*opReactions{}}
}

// booleanElements have no threshold values: the value is 1 (violation) or
// 0 (recovery).
var booleanElements = map[string]bool{"timeout": true, "verifyError": true}

// Configure (re)registers the reactions of cfg with fresh state: occurred
// false and empty windows, without notification. An operation without
// reactions is removed.
func (e *Engine) Configure(cfg *config.Operation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(cfg.React) == 0 {
		delete(e.ops, cfg.ID)
		return
	}
	e.logger.Debug("reactions configured", "op", cfg.ID, "reactions", len(cfg.React))
	o := &opReactions{id: cfg.ID, typ: cfg.Type, target: cfg.Target.String(), tag: cfg.Tag, vrf: cfg.VRF}
	for _, rc := range cfg.React {
		o.rows = append(o.rows, &reaction{cfg: rc, boolean: booleanElements[rc.Element]})
	}
	e.ops[cfg.ID] = o
}

// SetTag updates the tag the events of operation id carry, keeping the
// state of its reactions (a reload that changes only tag or owner restarts
// nothing).
func (e *Engine) SetTag(id int, tag string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o, ok := e.ops[id]; ok {
		o.tag = tag
	}
}

// Remove forgets the reactions of operation id.
func (e *Engine) Remove(id int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.ops, id)
}

// Reset clears occurred and the windows of operation id without
// notification (restart, reset, reload). The change counters are kept.
func (e *Engine) Reset(id int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o, ok := e.ops[id]; ok {
		for _, r := range o.rows {
			r.reset()
		}
	}
}

// Observe evaluates the reactions of r.OpID against the result. Busy and
// sequence errors are not attempts and are ignored.
func (e *Engine) Observe(r op.Result) {
	if r.Code == op.RCBusy || r.Code == op.RCSequenceError {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.ops[r.OpID]
	if !ok {
		return
	}
	for i, row := range o.rows {
		v, ok := elementValue(row.cfg.Element, &r)
		if !ok {
			continue
		}
		row.value = v
		if ev, changed := row.evaluate(v); changed {
			row.changes++
			row.lastChange = e.clk.Now()
			e.publish(o, row, i+1, ev)
		}
	}
}

// evaluate applies one value and reports whether occurred changed. The
// returned value is the one that decided the change (the average for
// average).
func (r *reaction) evaluate(v int64) (decided int64, changed bool) {
	c := r.classify(v)
	decided = v
	var next bool
	switch r.cfg.ThresholdType {
	case config.ThresholdImmediate:
		next = r.next(c == violation, c == recovery)
	case config.ThresholdConsecutive:
		switch c {
		case violation:
			r.runViolate++
			r.runRecover = 0
		case recovery:
			r.runRecover++
			r.runViolate = 0
		default:
			r.runViolate, r.runRecover = 0, 0
		}
		n := r.cfg.Count
		next = r.next(r.runViolate >= n, r.runRecover >= n)
	case config.ThresholdXofY:
		r.classes = append(r.classes, c)
		if len(r.classes) > r.cfg.Y {
			r.classes = r.classes[len(r.classes)-r.cfg.Y:]
		}
		var nv, nr int
		for _, k := range r.classes {
			switch k {
			case violation:
				nv++
			case recovery:
				nr++
			}
		}
		next = r.next(nv >= r.cfg.X, nr >= r.cfg.X)
	case config.ThresholdAverage:
		r.values = append(r.values, v)
		if len(r.values) > r.cfg.Count {
			r.values = r.values[len(r.values)-r.cfg.Count:]
		}
		if len(r.values) < r.cfg.Count {
			return v, false
		}
		var sum float64
		for _, x := range r.values {
			sum += float64(x)
		}
		avg := sum / float64(len(r.values))
		decided = int64(math.Round(avg))
		r.value = decided
		next = r.next(avg > float64(r.cfg.Upper), avg < float64(r.cfg.Lower))
	default: // never
		return v, false
	}
	if next == r.occurred {
		return decided, false
	}
	r.occurred = next
	if r.cfg.ThresholdType == config.ThresholdConsecutive {
		r.runViolate, r.runRecover = 0, 0
	}
	return decided, true
}

// next returns the new occurred state: rising sets it, falling clears it.
func (r *reaction) next(rising, falling bool) bool {
	if !r.occurred && rising {
		return true
	}
	if r.occurred && falling {
		return false
	}
	return r.occurred
}

// classify: numeric elements violate above Upper and recover below Lower
// (the thresholds themselves are neither); boolean ones violate at 1.
func (r *reaction) classify(v int64) class {
	if r.boolean {
		if v != 0 {
			return violation
		}
		return recovery
	}
	switch {
	case v > int64(r.cfg.Upper):
		return violation
	case v < int64(r.cfg.Lower):
		return recovery
	}
	return neutral
}

// publish raises the event of a change of row, the index-th (1-based)
// reaction of o.
func (e *Engine) publish(o *opReactions, row *reaction, index int, decided int64) {
	kind := event.ThresholdCleared
	if row.occurred {
		kind = event.ThresholdExceeded
	}
	ev := event.Event{
		Time:          row.lastChange,
		Kind:          kind,
		OpID:          o.id,
		Type:          o.typ,
		Target:        o.target,
		Tag:           o.tag,
		VRF:           o.vrf,
		Element:       row.cfg.Element,
		ThresholdType: row.cfg.ThresholdType,
		Value:         decided,
		Action:        row.cfg.Action,
		ReactionIndex: index,
		Message:       event.ThresholdMessage(o.id, kind, row.cfg.Element, row.boolean, decided, row.cfg.Upper, row.cfg.Lower),
	}
	if !row.boolean {
		ev.Upper, ev.Lower = row.cfg.Upper, row.cfg.Lower
	}
	if e.bus != nil {
		e.bus.Publish(ev)
	}
}

// Snapshot returns the reaction states of operation id, in configuration
// order; nil if it has none.
func (e *Engine) Snapshot(id int) []ReactionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.ops[id]
	if !ok {
		return nil
	}
	return o.states()
}

// All returns the reaction states of every operation that has reactions.
func (e *Engine) All() map[int][]ReactionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[int][]ReactionState, len(e.ops))
	for id, o := range e.ops {
		out[id] = o.states()
	}
	return out
}

// IDs returns the operations with reactions, sorted.
func (e *Engine) IDs() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]int, 0, len(e.ops))
	for id := range e.ops {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (o *opReactions) states() []ReactionState {
	out := make([]ReactionState, 0, len(o.rows))
	for _, r := range o.rows {
		out = append(out, ReactionState{
			Element:       r.cfg.Element,
			ThresholdType: r.cfg.ThresholdType,
			Upper:         r.cfg.Upper,
			Lower:         r.cfg.Lower,
			Count:         r.cfg.Count,
			X:             r.cfg.X,
			Y:             r.cfg.Y,
			Action:        r.cfg.Action,
			Occurred:      r.occurred,
			Value:         r.value,
			LastChange:    r.lastChange,
			Changes:       r.changes,
		})
	}
	return out
}
