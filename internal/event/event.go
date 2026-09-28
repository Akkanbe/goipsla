// Package event carries threshold-reaction and tracking events from the
// react package to their sinks: the log, syslog, webhooks and external
// programs (and SNMP traps in P7).
//
// A Bus fans events out to every subscribed Sink. Publish never blocks: an
// event goes into a queue of 10,000 and from there into one queue of 1,000
// per sink, each drained by its own goroutine, so a slow sink (a webhook that
// times out) delays only itself. Events that do not fit are dropped and
// counted.
package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
)

// Kind is the kind of an event. The values are those of config Action.On.
type Kind string

// Event kinds.
const (
	ThresholdExceeded Kind = "threshold-exceeded"
	ThresholdCleared  Kind = "threshold-cleared"
	TrackUp           Kind = "track-up"
	TrackDown         Kind = "track-down"
)

// IsThreshold reports whether k is a threshold-* kind.
func (k Kind) IsThreshold() bool { return k == ThresholdExceeded || k == ThresholdCleared }

// IsTrack reports whether k is a track-* kind.
func (k Kind) IsTrack() bool { return k == TrackUp || k == TrackDown }

// Event is one reaction or tracking state change.
type Event struct {
	Time   time.Time     `json:"time"`
	Kind   Kind          `json:"kind"`
	OpID   int           `json:"op_id"`
	Type   config.OpType `json:"type"`
	Target string        `json:"target"`
	Tag    string        `json:"tag,omitempty"`
	VRF    string        `json:"vrf,omitempty"`
	// Threshold reactions (Kind threshold-*).
	Element       string `json:"element,omitempty"`
	ThresholdType string `json:"threshold_type,omitempty"`
	Value         int64  `json:"value,omitempty"` // the value that decided the change (rttMonReactValue)
	Upper         int    `json:"upper,omitempty"`
	Lower         int    `json:"lower,omitempty"`
	Action        string `json:"action,omitempty"` // action of the reaction (syslog / trap routing)
	// ReactionIndex is the reaction's row, 1-based, in the operation's react
	// list when the event was raised (rttMonReactConfigIndex). It stays
	// right even if a reload reorders the rows before the event is delivered.
	ReactionIndex int `json:"reaction_index,omitempty"`
	// Tracking (Kind track-*).
	TrackID     int      `json:"track_id,omitempty"`
	TrackMode   string   `json:"track_mode,omitempty"`
	TrackState  string   `json:"track_state,omitempty"` // up | down
	LatestRC    string   `json:"latest_rc,omitempty"`
	LatestRTTMs *float64 `json:"latest_rtt_ms,omitempty"`
	Message     string   `json:"message"` // one human-readable line, also sent to syslog
}

// JSON encodes ev as sent to webhooks and exec programs: compact, without
// escaping "<", ">" and "&" (the messages contain "->").
func (ev Event) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ThresholdMessage formats the Cisco-like line of a threshold event:
//
//	%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold exceeded for rtt (value 350, rising 300, falling 200)
//	%RTT-4-OPER_TIMEOUT: IP SLAs(5): Threshold below for timeout
//
// boolean is true for the elements without a value (timeout, verifyError).
func ThresholdMessage(id int, kind Kind, element string, boolean bool, value int64, upper, lower int) string {
	verb := "exceeded"
	if kind == ThresholdCleared {
		verb = "below"
	}
	tag := "%RTT-3-IPSLATHRESHOLD"
	if element == "timeout" {
		tag = "%RTT-4-OPER_TIMEOUT"
	}
	msg := fmt.Sprintf("%s: IP SLAs(%d): Threshold %s for %s", tag, id, verb, element)
	if !boolean {
		msg += fmt.Sprintf(" (value %d, rising %d, falling %d)", value, upper, lower)
	}
	return msg
}

// TrackMessage formats the line of a tracking change:
//
//	%TRACK-6-STATE: 1 ip sla 101 reachability Up -> Down
func TrackMessage(trackID, opID int, mode, from, to string) string {
	return fmt.Sprintf("%%TRACK-6-STATE: %d ip sla %d %s %s -> %s", trackID, opID, mode, capitalize(from), capitalize(to))
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Sink is a destination of events. Deliver may block; the Bus calls it from
// the sink's own goroutine, one event at a time.
type Sink interface {
	Name() string
	Deliver(ctx context.Context, ev Event) error
}

// Queue sizes.
const (
	queueSize     = 10000
	sinkQueueSize = 1000
	recentSize    = 1000
)

// SinkStats counts the fate of the events offered to one sink.
type SinkStats struct {
	Name      string `json:"name"`
	Delivered uint64 `json:"delivered"` // Deliver returned nil (including events the sink filtered out)
	Failed    uint64 `json:"failed"`    // Deliver returned an error
	Dropped   uint64 `json:"dropped"`   // the sink's queue was full
}

// BusStats are the counters of a Bus.
type BusStats struct {
	Published uint64      `json:"published"`
	Dropped   uint64      `json:"dropped"` // the main queue was full
	Sinks     []SinkStats `json:"sinks"`   // in subscription order
}

type sinkQueue struct {
	sink    Sink
	ch      chan Event
	stats   SinkStats
	failLog throttle // "event delivery failed", from the sink's goroutine
	dropLog throttle // "event sink queue full", from Run's goroutine
	// failing counts the failures since the last success (the sink's
	// goroutine), for "event delivery recovered".
	failing uint64
}

// Bus distributes events to sinks.
type Bus struct {
	logger *slog.Logger
	clk    clock.Clock // for the warning throttles; tests replace it
	queue  chan Event

	mu        sync.Mutex
	sinks     []*sinkQueue
	published uint64
	dropped   uint64
	dropLog   throttle // "event queue full", under mu
	recent    []Event  // ring of recentSize
	next      int      // next write position in recent
	full      bool
}

// NewBus returns a bus. Subscribe the sinks, then call Run.
func NewBus(logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bus{logger: logger, clk: clock.Real(), queue: make(chan Event, queueSize), recent: make([]Event, recentSize)}
}

// warnInterval is the least time between two warnings of one kind for one
// sink: a syslog or webhook that is down would otherwise log once per event.
const warnInterval = time.Minute

// throttle limits a repeated warning to one per warnInterval and counts
// what it held back. Each throttle is used from one goroutine.
type throttle struct {
	last       time.Time
	suppressed uint64
}

// allow reports whether to log now; when it does, it also returns how many
// were suppressed since the last one.
func (t *throttle) allow(now time.Time) (bool, uint64) {
	if !t.last.IsZero() && now.Sub(t.last) < warnInterval {
		t.suppressed++
		return false, 0
	}
	n := t.suppressed
	t.last, t.suppressed = now, 0
	return true, n
}

// Subscribe adds a sink. Call it before Run.
func (b *Bus) Subscribe(s Sink) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sinks = append(b.sinks, &sinkQueue{sink: s, ch: make(chan Event, sinkQueueSize), stats: SinkStats{Name: s.Name()}})
}

// Publish queues ev for every sink and keeps it in the recent-events ring.
// It never blocks: when the queue is full the event is dropped (it still
// appears in Recent) and a warning is logged.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	b.published++
	b.recent[b.next] = ev
	b.next = (b.next + 1) % recentSize
	if b.next == 0 {
		b.full = true
	}
	b.mu.Unlock()
	select {
	case b.queue <- ev:
	default:
		b.mu.Lock()
		b.dropped++
		n := b.dropped
		ok, suppressed := b.dropLog.allow(b.clk.Now())
		b.mu.Unlock()
		if ok {
			b.logger.Warn("event queue full; event dropped", "kind", string(ev.Kind), "op", ev.OpID, "dropped_total", n, "suppressed", suppressed)
		}
	}
}

// Recent returns up to limit of the latest events, oldest first. limit <= 0
// or above the ring size returns all that are kept.
func (b *Bus) Recent(limit int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.next
	if b.full {
		n = recentSize
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Event, 0, limit)
	for i := limit; i > 0; i-- {
		out = append(out, b.recent[(b.next-i+recentSize)%recentSize])
	}
	return out
}

// Stats returns a copy of the counters.
func (b *Bus) Stats() BusStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := BusStats{Published: b.published, Dropped: b.dropped}
	for _, q := range b.sinks {
		st.Sinks = append(st.Sinks, q.stats)
	}
	return st
}

// Run delivers queued events until ctx is done. Events still queued then are
// not delivered.
func (b *Bus) Run(ctx context.Context) error {
	b.mu.Lock()
	sinks := append([]*sinkQueue(nil), b.sinks...)
	b.mu.Unlock()

	var wg sync.WaitGroup
	for _, q := range sinks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.drain(ctx, q)
		}()
	}
	defer func() {
		wg.Wait()
		b.logTotals()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-b.queue:
			for _, q := range sinks {
				select {
				case q.ch <- ev:
				default:
					b.mu.Lock()
					q.stats.Dropped++
					n := q.stats.Dropped
					b.mu.Unlock()
					if ok, suppressed := q.dropLog.allow(b.clk.Now()); ok {
						b.logger.Warn("event sink queue full; event dropped", "sink", q.stats.Name, "kind", string(ev.Kind), "op", ev.OpID, "dropped_total", n, "suppressed", suppressed)
					}
				}
			}
		}
	}
}

func (b *Bus) drain(ctx context.Context, q *sinkQueue) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-q.ch:
			err := q.sink.Deliver(ctx, ev)
			b.mu.Lock()
			if err != nil {
				q.stats.Failed++
			} else {
				q.stats.Delivered++
			}
			b.mu.Unlock()
			switch {
			case err != nil && ctx.Err() == nil:
				q.failing++
				if ok, suppressed := q.failLog.allow(b.clk.Now()); ok {
					b.logger.Warn("event delivery failed", append(eventAttrs(q, ev), "err", err, "suppressed", suppressed)...)
				}
			case err == nil && q.failing > 0:
				b.logger.Info("event delivery recovered", "sink", q.stats.Name, "failed", q.failing, "suppressed", q.failLog.suppressed)
				q.failing = 0
				q.failLog = throttle{} // the next failure is logged at once
			}
		}
	}
}

// eventAttrs are the log attributes of ev delivered to q: the sink, the
// kind, the operation and target, and the element (threshold-*) or the
// track (track-*).
func eventAttrs(q *sinkQueue, ev Event) []any {
	a := []any{"sink", q.stats.Name, "kind", string(ev.Kind), "op", ev.OpID, "target", ev.Target}
	if ev.Kind.IsTrack() {
		return append(a, "track_id", ev.TrackID)
	}
	return append(a, "element", ev.Element)
}

// logTotals logs, when the bus stops, what each sink failed or dropped over
// the daemon's life (info; nothing for a sink that lost nothing).
func (b *Bus) logTotals() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, q := range b.sinks {
		if st := q.stats; st.Failed > 0 || st.Dropped > 0 {
			b.logger.Info("event sink totals", "sink", st.Name, "delivered", st.Delivered, "failed", st.Failed, "dropped", st.Dropped)
		}
	}
}
