package manager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/probe"
	"goipsla/internal/sched"
	"goipsla/internal/stats"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// fakeEngine records requests and answers them with reply(req). If block is
// set, Echo first waits for it to be closed or for ctx to end.
type fakeEngine struct {
	mu    sync.Mutex
	reqs  []probe.Request
	reply func(req probe.Request) probe.Reply
	block chan struct{}
	// forgotten records the operations Forget was called for.
	forgotten []uint32
}

func (e *fakeEngine) Echo(ctx context.Context, req probe.Request) probe.Reply {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	block := e.block
	e.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return probe.Reply{Outcome: probe.OutcomeError, Err: ctx.Err()}
		}
	}
	if e.reply == nil {
		return probe.Reply{Outcome: probe.OutcomeReply, RTT: time.Millisecond}
	}
	return e.reply(req)
}

func (e *fakeEngine) Close() error { return nil }

func (e *fakeEngine) Stats() probe.Stats { return probe.Stats{} }

func (e *fakeEngine) Forget(opID uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgotten = append(e.forgotten, opID)
}

func (e *fakeEngine) Jitter(context.Context, probe.JitterRequest) probe.JitterReply {
	return probe.JitterReply{Err: errors.New("jitter not supported by fakeEngine")}
}

func (e *fakeEngine) requests() []probe.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]probe.Request(nil), e.reqs...)
}

// sinkRecorder collects the results passed to the sink.
type sinkRecorder struct {
	mu      sync.Mutex
	results []op.Result
}

func (s *sinkRecorder) sink(r op.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
}

func (s *sinkRecorder) get() []op.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]op.Result(nil), s.results...)
}

func (s *sinkRecorder) waitLen(t *testing.T, n int) []op.Result {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := s.get()
		if len(r) >= n {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d results, have %d: %+v", n, len(r), r)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func echoOp(id int, target string) *config.Operation {
	return &config.Operation{
		ID:              id,
		Type:            config.ICMPEcho,
		Target:          netip.MustParseAddr(target),
		TargetName:      target,
		Frequency:       10 * time.Second,
		Timeout:         2 * time.Second,
		Threshold:       100 * time.Millisecond,
		RequestDataSize: 28,
		DataPattern:     probe.DefaultPattern,
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// start runs m in the background and returns a function that stops it.
func start(t *testing.T, m *Manager) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	// Run initializes the schedules before it reports running; after that
	// settled() tells when the timers are armed.
	waitFor(t, "manager running", func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.running
	})
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func TestNewClassifiesOperations(t *testing.T) {
	now := echoOp(1, "192.0.2.1")
	pending := echoOp(2, "192.0.2.2")
	pending.Schedule = &config.Schedule{Start: "pending", Forever: true}
	jitter := echoOp(3, "192.0.2.3")
	jitter.Type = unimplementedType
	after := echoOp(4, "192.0.2.4")
	after.Schedule = &config.Schedule{Start: "after", After: time.Minute, Forever: true}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	m := New(&config.Config{Operations: []*config.Operation{now, pending, jitter, after}},
		&fakeEngine{}, clock.NewFake(t0), nil, logger)

	// Before Run nothing has started: every implemented type is pending.
	if got, want := m.Counts(), (Counts{Total: 4, Pending: 3, Inactive: 1, Skipped: 1}); got != want {
		t.Errorf("Counts() before Run = %+v, want %+v", got, want)
	}
	stop := start(t, m)
	defer stop()
	waitFor(t, "operations started", func() bool { return m.Counts().Active == 1 })
	if got, want := m.Counts(), (Counts{Total: 4, Active: 1, Pending: 2, Inactive: 1, Skipped: 1}); got != want {
		t.Errorf("Counts() after Run = %+v, want %+v", got, want)
	}
	for id, want := range map[int]string{1: stateActive, 2: statePending, 3: stateInactive, 4: statePending} {
		if got, ok := m.State(id); !ok || got != want {
			t.Errorf("State(%d) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if _, ok := m.State(99); ok {
		t.Error("State(99) ok = true for an unknown ID")
	}
	if !strings.Contains(logs.String(), "icmp-path-echo is not implemented yet; skipped") {
		t.Errorf("log lacks the skip warning:\n%s", logs.String())
	}
}

func TestRunDeliversResults(t *testing.T) {
	c := echoOp(7, "2001:db8::7")
	c.TrafficClass = 0xB8
	replies := []probe.Reply{
		{Outcome: probe.OutcomeReply, RTT: 5 * time.Millisecond},
		{Outcome: probe.OutcomeReply, RTT: 150 * time.Millisecond},
		{Outcome: probe.OutcomeTimeout},
		{Outcome: probe.OutcomeUnreachable, Detail: "destination unreachable: host, from 2001:db8::1"},
	}
	eng := &fakeEngine{reply: func(req probe.Request) probe.Reply { return replies[req.Seq-1] }}
	clk := clock.NewFake(t0)
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{c}}, eng, clk, rec.sink, discardLogger())
	stop := start(t, m)
	defer stop()

	first := t0.Add(sched.Offset(7, c.Frequency))
	waitFor(t, "manager to settle", func() bool { return settled(m) })
	// "now" is active before the first attempt, during the dispersion offset.
	if s, _ := m.State(7); s != stateActive {
		t.Errorf("state before the first attempt = %q, want active", s)
	}
	clk.Set(first)
	rec.waitLen(t, 1)
	if s, _ := m.State(7); s != stateActive {
		t.Errorf("state after the first attempt = %q, want active", s)
	}
	for i := 2; i <= len(replies); i++ {
		waitFor(t, "manager to settle", func() bool { return quiet(m) })
		clk.Advance(c.Frequency)
		rec.waitLen(t, i)
	}

	got := rec.get()
	want := []struct {
		code   op.ReturnCode
		rtt    time.Duration
		detail string
	}{
		{op.RCOK, 5 * time.Millisecond, ""},
		{op.RCOverThreshold, 150 * time.Millisecond, ""},
		{op.RCTimeout, 0, ""},
		{op.RCTimeout, 0, "destination unreachable: host, from 2001:db8::1"},
	}
	for i, w := range want {
		r := got[i]
		start := first.Add(time.Duration(i) * c.Frequency)
		if r.OpID != 7 || r.Type != config.ICMPEcho || r.Seq != uint32(i+1) || !r.Start.Equal(start) ||
			r.Code != w.code || r.RTT != w.rtt || r.Detail != w.detail {
			t.Errorf("result %d = %+v\nwant OpID 7, Seq %d, Start %v, Code %v, RTT %v, Detail %q",
				i, r, i+1, start, w.code, w.rtt, w.detail)
		}
	}
	for i, req := range eng.requests() {
		if req.OpID != 7 || req.Seq != uint32(i+1) || req.TOS != 0xB8 || req.Timeout != 2*time.Second {
			t.Errorf("request %d = %+v", i, req)
		}
	}
}

func TestPendingNotScheduled(t *testing.T) {
	c := echoOp(1, "192.0.2.1")
	c.Schedule = &config.Schedule{Start: "pending"}
	jitter := echoOp(2, "192.0.2.2")
	jitter.Type = unimplementedType
	eng := &fakeEngine{}
	clk := clock.NewFake(t0)
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{c, jitter}}, eng, clk, rec.sink, discardLogger())
	stop := start(t, m)
	clk.Advance(time.Hour)
	time.Sleep(10 * time.Millisecond)
	stop()
	if n := len(eng.requests()); n != 0 {
		t.Errorf("engine got %d requests, want 0", n)
	}
	if r := rec.get(); len(r) != 0 {
		t.Errorf("sink got %+v, want nothing", r)
	}
}

func TestStartAfterAndAt(t *testing.T) {
	after := echoOp(1, "192.0.2.1")
	after.Schedule = &config.Schedule{Start: "after", After: 30 * time.Second, Forever: true}
	at := echoOp(2, "192.0.2.2")
	at.Schedule = &config.Schedule{Start: "at", At: t0.Add(45 * time.Second), Forever: true}
	past := echoOp(3, "192.0.2.3")
	past.Schedule = &config.Schedule{Start: "at", At: t0.Add(-time.Hour), Forever: true}
	for _, c := range []*config.Operation{after, at, past} {
		c.Frequency = time.Hour // one attempt each within the test
	}

	clk := clock.NewFake(t0)
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{after, at, past}}, &fakeEngine{}, clk, rec.sink, discardLogger())
	stop := start(t, m)
	defer stop()

	rec.waitLen(t, 1) // the operation whose start time has passed runs at once
	waitFor(t, "manager to settle", func() bool { return quiet(m) })
	clk.Advance(30 * time.Second)
	rec.waitLen(t, 2)
	if s, _ := m.State(2); s != statePending {
		t.Errorf("State(2) before its start time = %q, want pending", s)
	}
	waitFor(t, "manager to settle", func() bool { return quiet(m) })
	clk.Advance(15 * time.Second)
	got := rec.waitLen(t, 3)

	want := map[int]time.Time{3: t0, 1: t0.Add(30 * time.Second), 2: t0.Add(45 * time.Second)}
	for i, r := range got[:3] {
		if w, ok := want[r.OpID]; !ok || !r.Start.Equal(w) || r.Seq != 1 {
			t.Errorf("result %d = %+v, want op %d to start at %v with seq 1", i, r, r.OpID, w)
		}
	}
	if s, _ := m.State(2); s != stateActive {
		t.Errorf("State(2) after its start time = %q, want active", s)
	}
}

func TestBusy(t *testing.T) {
	c := echoOp(9, "192.0.2.9")
	c.Frequency = time.Second
	c.Timeout = 500 * time.Millisecond
	c.Schedule = &config.Schedule{Start: "after", After: time.Second, Forever: true}
	eng := &fakeEngine{block: make(chan struct{})}
	clk := clock.NewFake(t0)
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{c}}, eng, clk, rec.sink, discardLogger())
	stop := start(t, m)
	defer stop()

	waitFor(t, "manager to settle", func() bool { return settled(m) })
	clk.Advance(time.Second) // attempt 1 starts and hangs in the engine
	deadline := time.Now().Add(10 * time.Second)
	for len(eng.requests()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("attempt 1 did not start")
		}
		time.Sleep(100 * time.Microsecond)
	}
	waitFor(t, "manager to settle", func() bool { return settled(m) })
	clk.Advance(time.Second) // attempt 1 still outstanding: busy
	rec.waitLen(t, 1)
	close(eng.block)
	got := rec.waitLen(t, 2)

	busyAt := t0.Add(2 * time.Second)
	if r := got[0]; r != (op.Result{OpID: 9, Type: config.ICMPEcho, Life: 1, Start: busyAt, End: busyAt, Code: op.RCBusy}) {
		t.Errorf("busy result = %+v", r)
	}
	if r := got[1]; r.Code != op.RCOK || r.Seq != 1 || !r.Start.Equal(t0.Add(time.Second)) {
		t.Errorf("attempt 1 result = %+v", r)
	}
	busies := 0
	for _, r := range got {
		if r.Code == op.RCBusy {
			busies++
		}
	}
	if busies != 1 {
		t.Errorf("%d busy results, want 1", busies)
	}

	// The busy did not consume a sequence number.
	waitFor(t, "manager to settle", func() bool { return quiet(m) })
	clk.Advance(time.Second)
	got = rec.waitLen(t, 3)
	if r := got[2]; r.Seq != 2 || r.Code != op.RCOK {
		t.Errorf("attempt 2 result = %+v, want seq 2 ok", r)
	}
}

func TestLateReply(t *testing.T) {
	clk := clock.NewFake(t0)
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{echoOp(3, "192.0.2.3")}}, &fakeEngine{}, clk, rec.sink, discardLogger())
	m.LateReply(3, 17, t0) // no attempt 17 in this life: dropped
	m.LateReply(99, 1, t0) // unknown operation: ignored
	if got := rec.get(); len(got) != 0 {
		t.Errorf("sink got %+v, want nothing", got)
	}
}

func TestShutdownDropsAbandonedAttempt(t *testing.T) {
	c := echoOp(1, "192.0.2.1")
	c.Schedule = &config.Schedule{Start: "at", At: t0, Forever: true}
	eng := &fakeEngine{block: make(chan struct{})}
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{c}}, eng, clock.NewFake(t0), rec.sink, discardLogger())
	stop := start(t, m)
	deadline := time.Now().Add(10 * time.Second)
	for len(eng.requests()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("attempt did not start")
		}
		time.Sleep(100 * time.Microsecond)
	}
	stop()
	time.Sleep(10 * time.Millisecond)
	if r := rec.get(); len(r) != 0 {
		t.Errorf("sink got %+v after shutdown, want nothing", r)
	}
}

// TestSinkSerialized checks that the sink is never called concurrently even
// when many operations complete at once.
func TestSinkSerialized(t *testing.T) {
	var ops []*config.Operation
	for id := 1; id <= 200; id++ {
		c := echoOp(id, "192.0.2.1")
		c.Schedule = &config.Schedule{Start: "at", At: t0, Forever: true}
		ops = append(ops, c)
	}
	var inSink, overlaps, n int
	var mu sync.Mutex
	sink := func(op.Result) {
		mu.Lock()
		inSink++
		if inSink > 1 {
			overlaps++
		}
		mu.Unlock()
		time.Sleep(10 * time.Microsecond)
		mu.Lock()
		inSink--
		n++
		mu.Unlock()
	}
	m := New(&config.Config{Operations: ops}, &fakeEngine{}, clock.NewFake(t0), sink, discardLogger())
	stop := start(t, m)
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		done := n == len(ops)
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not all operations reported")
		}
		time.Sleep(time.Millisecond)
	}
	if overlaps != 0 {
		t.Errorf("sink called concurrently %d times", overlaps)
	}
}

// TestStartIsActualTime checks that Result.Start is the time the attempt
// actually started, not the lattice time it was scheduled for, while the
// schedule itself stays on the lattice.
func TestStartIsActualTime(t *testing.T) {
	c := echoOp(4, "192.0.2.4")
	c.Schedule = &config.Schedule{Start: "after", After: 10 * time.Second, Forever: true}
	clk := clock.NewFake(t0)
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{c}}, &fakeEngine{}, clk, rec.sink, discardLogger())
	stop := start(t, m)
	defer stop()

	// The scheduler wakes 300 ms after the lattice point t0+10s.
	waitFor(t, "manager to settle", func() bool { return quiet(m) })
	clk.Advance(10*time.Second + 300*time.Millisecond)
	rec.waitLen(t, 1)
	// The next attempt is still due on the lattice, at t0+20s.
	waitFor(t, "manager to settle", func() bool { return quiet(m) })
	clk.Advance(9*time.Second + 700*time.Millisecond)
	got := rec.waitLen(t, 2)

	for i, want := range []time.Time{t0.Add(10*time.Second + 300*time.Millisecond), t0.Add(20 * time.Second)} {
		if r := got[i]; !r.Start.Equal(want) || r.Seq != uint32(i+1) {
			t.Errorf("result %d: Start %v, Seq %d; want Start %v, Seq %d", i, r.Start, r.Seq, want, i+1)
		}
	}
}

func TestSetStoreStartsLives(t *testing.T) {
	now := echoOp(1, "192.0.2.1")
	after := echoOp(2, "192.0.2.2")
	after.Schedule = &config.Schedule{Start: "after", After: 30 * time.Second, Forever: true}
	pending := echoOp(3, "192.0.2.3")
	pending.Schedule = &config.Schedule{Start: "pending", Forever: true}
	jitter := echoOp(4, "192.0.2.4")
	jitter.Type = unimplementedType

	clk := clock.NewFake(t0)
	store := stats.NewStore(clk)
	m := New(&config.Config{Operations: []*config.Operation{now, after, pending, jitter}},
		&fakeEngine{}, clk, store.Record, discardLogger())
	m.SetStore(store)
	stop := start(t, m)
	defer stop()
	waitFor(t, "manager to settle", func() bool { return settled(m) })

	wantStart := map[int]time.Time{
		1: t0.Add(sched.Offset(1, now.Frequency)),
		2: t0.Add(30 * time.Second),
		3: t0,
		4: t0,
	}
	for id, want := range wantStart {
		snap, ok := store.Snapshot(id, stats.SnapshotOptions{})
		if !ok {
			t.Errorf("no statistics row for operation %d", id)
			continue
		}
		if !snap.LifeStart.Equal(want) || snap.LifeIndex != 1 {
			t.Errorf("operation %d: life %d starts at %v, want life 1 at %v", id, snap.LifeIndex, snap.LifeStart, want)
		}
	}

	// Results reach the store through the sink.
	clk.Advance(30 * time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, _ := store.Snapshot(2, stats.SnapshotOptions{})
		if snap.Totals.Completions == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation 2 totals = %+v, want 1 completion", snap.Totals)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func TestLateReplyCarriesAttemptStart(t *testing.T) {
	c := echoOp(6, "192.0.2.6")
	c.Schedule = &config.Schedule{Start: "after", After: 10 * time.Second, Forever: true}
	clk := clock.NewFake(t0)
	rec := &sinkRecorder{}
	m := New(&config.Config{Operations: []*config.Operation{c}}, &fakeEngine{}, clk, rec.sink, discardLogger())
	stop := start(t, m)
	defer stop()
	waitFor(t, "manager to settle", func() bool { return quiet(m) })
	clk.Advance(10 * time.Second)
	rec.waitLen(t, 1)
	clk.Advance(3 * time.Second)

	sent := t0.Add(10 * time.Second)
	m.LateReply(6, 99, sent)                      // not an attempt of this life: dropped
	m.LateReply(6, 1, sent.Add(-time.Nanosecond)) // sent before attempt 1 started: dropped
	m.LateReply(6, 1, sent)                       // remembered attempt
	got := rec.waitLen(t, 2)
	if r := got[1]; r.Code != op.RCSequenceError || r.Seq != 1 || r.Life != 1 || !r.Start.Equal(t0.Add(10*time.Second)) || !r.End.Equal(t0.Add(13*time.Second)) {
		t.Errorf("late reply for seq 1 = %+v, want Start %v End %v", r, t0.Add(10*time.Second), t0.Add(13*time.Second))
	}
	if len(got) != 2 {
		t.Errorf("sink got %d results, want 2 (the reply for seq 99 dropped)", len(got))
	}
}

// waitFor polls cond until it holds or a real-time deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// settled reports whether both schedulers of m have armed their timers for
// everything that happened so far. Tests wait for it before advancing the
// fake clock.
func settled(m *Manager) bool { return m.sched.Settled() && m.events.Settled() }

// quiet reports that m has settled and that no attempt or schedule event is
// in flight: advancing the clock then cannot turn the next attempt into a
// busy because the previous goroutine has not returned yet.
func quiet(m *Manager) bool { return settled(m) && m.sched.Idle() && m.events.Idle() }

// advance moves the fake clock after the manager has settled, and waits for
// it to settle again.
func advance(t *testing.T, m *Manager, clk *clock.Fake, d time.Duration) {
	t.Helper()
	waitFor(t, "manager to settle", func() bool { return quiet(m) })
	clk.Advance(d)
	waitFor(t, "manager to settle", func() bool { return settled(m) })
}

// unimplementedType is a type without a Runner, to exercise the skip path
// now that icmp-echo and icmp-jitter are both implemented.
const unimplementedType config.OpType = "icmp-path-echo"
