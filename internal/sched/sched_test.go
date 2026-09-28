package sched

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"goipsla/internal/clock"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder collects the times at which entries ran or were busy.
type recorder struct {
	mu   sync.Mutex
	runs map[int][]time.Time
	busy map[int][]time.Time
	n    int
}

func newRecorder() *recorder {
	return &recorder{runs: map[int][]time.Time{}, busy: map[int][]time.Time{}}
}

func (r *recorder) run(id int) func(context.Context, time.Time) {
	return func(_ context.Context, now time.Time) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.runs[id] = append(r.runs[id], now)
		r.n++
	}
}

func (r *recorder) onBusy(id int) func(time.Time) {
	return func(now time.Time) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.busy[id] = append(r.busy[id], now)
	}
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func (r *recorder) times(id int) (runs, busy []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.runs[id]...), append([]time.Time(nil), r.busy[id]...)
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

// idle reports whether no Run goroutine is in flight.
func idle(s *Scheduler) bool { return s.Idle() }

// settle waits until the scheduler has armed its timer for every Add and
// Remove made so far.
func settle(t *testing.T, s *Scheduler) {
	t.Helper()
	waitFor(t, "scheduler to settle", s.Settled)
}

// start runs s in the background and returns a function that stops it and
// checks that Run returned nil.
func start(t *testing.T, s *Scheduler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
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

func equalTimes(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func TestOffset(t *testing.T) {
	if got := Offset(1, 0); got != 0 {
		t.Errorf("Offset(1, 0) = %v, want 0", got)
	}
	if got := Offset(1, -time.Second); got != 0 {
		t.Errorf("Offset(1, -1s) = %v, want 0", got)
	}
	const freq = 60 * time.Second
	buckets := make([]int, 10)
	for id := 1; id <= 1000; id++ {
		off := Offset(id, freq)
		if off < 0 || off >= freq {
			t.Fatalf("Offset(%d, %v) = %v, out of [0, freq)", id, freq, off)
		}
		buckets[off*10/freq]++
	}
	// 1,000 IDs over 10 buckets: expect ~100 each; a bucket below 50 would
	// mean the offsets are badly clustered.
	for i, n := range buckets {
		if n < 50 {
			t.Errorf("bucket %d has %d offsets, want roughly 100 (buckets %v)", i, n, buckets)
		}
	}
}

func TestOffsetPinned(t *testing.T) {
	// FNV-1a 64 of the big-endian bytes 00 00 00 00 00 00 00 01 is
	// 0xa8c7f732281a3812; modulo 60e9 ns.
	const h = uint64(0xa8c7f732281a3812)
	want := time.Duration(h % uint64(60*time.Second))
	if got := Offset(1, 60*time.Second); got != want {
		t.Errorf("Offset(1, 60s) = %v, want %v", got, want)
	}
}

// TestLattice registers 1,000 entries at a 1 s frequency, advances the clock
// by 60 s one second at a time and checks that every entry ran on its
// lattice First + k×1s and nowhere else.
func TestLattice(t *testing.T) {
	const (
		n     = 1000
		freq  = time.Second
		steps = 60
	)
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	rec := newRecorder()
	first := make(map[int]time.Time, n)
	for id := 1; id <= n; id++ {
		first[id] = t0.Add(Offset(id, freq))
		s.Add(Entry{ID: id, Frequency: freq, First: first[id], Run: rec.run(id), Busy: rec.onBusy(id)})
	}
	// expected returns how many runs are due up to and including now.
	expected := func(now time.Time) int {
		total := 0
		for _, f := range first {
			if !f.After(now) {
				total += int(now.Sub(f)/freq) + 1
			}
		}
		return total
	}

	stop := start(t, s)
	defer stop()
	clk.BlockUntil(1)
	waitFor(t, "initial runs", func() bool { return rec.total() == expected(t0) && idle(s) })
	for i := 1; i <= steps; i++ {
		clk.Advance(freq)
		want := expected(t0.Add(time.Duration(i) * freq))
		waitFor(t, "runs of step", func() bool { return rec.total() == want && idle(s) })
		clk.BlockUntil(1)
	}

	end := t0.Add(steps * freq)
	for id := 1; id <= n; id++ {
		runs, busy := rec.times(id)
		if len(busy) != 0 {
			t.Errorf("entry %d: busy at %v, want none", id, busy)
		}
		var want []time.Time
		for at := first[id]; !at.After(end); at = at.Add(freq) {
			want = append(want, at)
		}
		if len(want) < steps {
			t.Fatalf("entry %d: test expects at least %d runs, computed %d", id, steps, len(want))
		}
		if !equalTimes(runs, want) {
			t.Errorf("entry %d: ran at %v\nwant %v", id, runs, want)
		}
	}
}

func TestBusy(t *testing.T) {
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	rec := newRecorder()
	release := make(chan struct{})
	started := make(chan time.Time, 10)
	var calls int
	var mu sync.Mutex
	s.Add(Entry{
		ID:        7,
		Frequency: time.Second,
		First:     t0.Add(time.Second),
		Run: func(_ context.Context, now time.Time) {
			mu.Lock()
			calls++
			c := calls
			mu.Unlock()
			started <- now
			if c == 1 {
				<-release
			}
		},
		Busy: rec.onBusy(7),
	})
	stop := start(t, s)
	defer stop()

	clk.BlockUntil(1)
	clk.Advance(time.Second)
	if got := <-started; !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("first run at %v, want %v", got, t0.Add(time.Second))
	}
	for i := 2; i <= 3; i++ {
		clk.BlockUntil(1)
		clk.Advance(time.Second)
		waitFor(t, "busy", func() bool { _, b := rec.times(7); return len(b) == i-1 })
	}
	close(release)
	waitFor(t, "idle", func() bool { return idle(s) })
	clk.BlockUntil(1)
	clk.Advance(time.Second)
	if got, want := <-started, t0.Add(4*time.Second); !got.Equal(want) {
		t.Errorf("run after busy at %v, want %v", got, want)
	}
	_, busy := rec.times(7)
	if want := []time.Time{t0.Add(2 * time.Second), t0.Add(3 * time.Second)}; !equalTimes(busy, want) {
		t.Errorf("busy at %v, want %v", busy, want)
	}
}

func TestSkipMissed(t *testing.T) {
	clk := clock.NewFake(t0)
	var logs syncBuffer
	s := New(clk, slog.New(slog.NewTextHandler(&logs, nil)))
	rec := newRecorder()
	s.Add(Entry{ID: 1, Frequency: time.Second, First: t0.Add(time.Second), Run: rec.run(1)})
	stop := start(t, s)
	defer stop()

	clk.BlockUntil(1)
	// The scheduler wakes up 9.5 s late: it runs once, for the latest
	// lattice point, and does not replay the nine it missed.
	clk.Advance(10*time.Second + 500*time.Millisecond)
	waitFor(t, "late run", func() bool { return rec.total() == 1 && idle(s) })
	clk.BlockUntil(1)
	clk.Advance(500 * time.Millisecond)
	waitFor(t, "next run", func() bool { return rec.total() == 2 })
	runs, _ := rec.times(1)
	if want := []time.Time{t0.Add(10 * time.Second), t0.Add(11 * time.Second)}; !equalTimes(runs, want) {
		t.Errorf("ran at %v, want %v", runs, want)
	}
	// The warning says how late the scheduler was (9.5 s behind t0+1s).
	if want := `msg="sched: runs missed while the scheduler was late were skipped" entries=1 max_delay_ms=9500`; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func TestFirstInPast(t *testing.T) {
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	rec := newRecorder()
	// Lattice origin 2.5 s in the past: the entry fires at once for the
	// lattice point t0-0.5s, then stays on the lattice.
	s.Add(Entry{ID: 1, Frequency: time.Second, First: t0.Add(-2500 * time.Millisecond), Run: rec.run(1)})
	stop := start(t, s)
	defer stop()
	waitFor(t, "first run", func() bool { return rec.total() == 1 && idle(s) })
	clk.BlockUntil(1)
	clk.Advance(time.Second)
	waitFor(t, "second run", func() bool { return rec.total() == 2 })
	runs, _ := rec.times(1)
	if want := []time.Time{t0.Add(-500 * time.Millisecond), t0.Add(500 * time.Millisecond)}; !equalTimes(runs, want) {
		t.Errorf("ran at %v, want %v", runs, want)
	}
}

func TestAddWakesScheduler(t *testing.T) {
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	rec := newRecorder()
	s.Add(Entry{ID: 1, Frequency: time.Minute, First: t0.Add(time.Minute), Run: rec.run(1)})
	stop := start(t, s)
	defer stop()
	clk.BlockUntil(1)

	// An entry due before the armed timer must still fire on time.
	s.Add(Entry{ID: 2, Frequency: time.Second, First: t0.Add(time.Second), Run: rec.run(2)})
	settle(t, s)
	clk.Advance(time.Second)
	waitFor(t, "run of the added entry", func() bool { return rec.total() == 1 })
	runs, _ := rec.times(2)
	if want := []time.Time{t0.Add(time.Second)}; !equalTimes(runs, want) {
		t.Errorf("entry 2 ran at %v, want %v", runs, want)
	}
}

func TestAddReplaces(t *testing.T) {
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	oldRec, newRec := newRecorder(), newRecorder()
	s.Add(Entry{ID: 5, Frequency: 3 * time.Second, First: t0.Add(time.Second), Run: oldRec.run(5)})
	stop := start(t, s)
	defer stop()
	clk.BlockUntil(1)
	clk.Advance(time.Second) // old entry runs at t0+1s
	waitFor(t, "old run", func() bool { return oldRec.total() == 1 && idle(s) })

	s.Add(Entry{ID: 5, Frequency: 2 * time.Second, First: t0.Add(2 * time.Second), Run: newRec.run(5)})
	if got := s.Len(); got != 1 {
		t.Fatalf("Len() = %d after replacing, want 1", got)
	}
	settle(t, s)
	for i := 0; i < 5; i++ { // up to t0+6s
		clk.BlockUntil(1)
		clk.Advance(time.Second)
		want := (i + 2) / 2 // new entry runs at t0+2s, 4s, 6s
		waitFor(t, "new runs", func() bool { return newRec.total() == want && idle(s) })
	}
	if got := oldRec.total(); got != 1 {
		t.Errorf("replaced entry ran %d times, want 1", got)
	}
	runs, _ := newRec.times(5)
	if want := []time.Time{t0.Add(2 * time.Second), t0.Add(4 * time.Second), t0.Add(6 * time.Second)}; !equalTimes(runs, want) {
		t.Errorf("new entry ran at %v, want %v", runs, want)
	}
}

// blockingEntry returns an entry whose Run blocks until its ctx ends and
// reports the cancellation on the returned channel.
func blockingEntry(id int, first time.Time) (Entry, <-chan struct{}, <-chan struct{}) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	return Entry{
		ID:        id,
		Frequency: time.Second,
		First:     first,
		Run: func(ctx context.Context, _ time.Time) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			close(canceled)
		},
	}, started, canceled
}

func waitChan(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestReplaceCancelsInFlight(t *testing.T) {
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	e, started, canceled := blockingEntry(1, t0)
	s.Add(e)
	stop := start(t, s)
	defer stop()
	waitChan(t, "run start", started)

	rec := newRecorder()
	s.Add(Entry{ID: 1, Frequency: time.Second, First: t0.Add(time.Second), Run: rec.run(1), Busy: rec.onBusy(1)})
	waitChan(t, "cancellation of the replaced run", canceled)
	settle(t, s)
	clk.BlockUntil(1)
	clk.Advance(time.Second)
	// The replacement does not inherit the busy state of the old entry.
	waitFor(t, "replacement run", func() bool { return rec.total() == 1 })
	if _, busy := rec.times(1); len(busy) != 0 {
		t.Errorf("replacement busy at %v, want none", busy)
	}
}

func TestRemove(t *testing.T) {
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	rec := newRecorder()
	e, started, canceled := blockingEntry(1, t0.Add(time.Second))
	s.Add(e)
	s.Add(Entry{ID: 2, Frequency: time.Second, First: t0.Add(time.Second), Run: rec.run(2)})
	stop := start(t, s)
	defer stop()
	clk.BlockUntil(1)
	clk.Advance(time.Second)
	waitChan(t, "run start", started)

	s.Remove(1)
	s.Remove(99) // unknown: no-op
	waitChan(t, "cancellation of the removed run", canceled)
	settle(t, s)
	if got := s.Len(); got != 1 {
		t.Errorf("Len() = %d after Remove, want 1", got)
	}
	// Each run must have returned before the next second, or the next
	// firing would be a busy instead of a run.
	waitFor(t, "first run of the remaining entry", func() bool { return rec.total() == 1 && idle(s) })
	for i := 2; i <= 3; i++ {
		clk.BlockUntil(1)
		clk.Advance(time.Second)
		waitFor(t, "runs of the remaining entry", func() bool { return rec.total() == i && idle(s) })
	}
	s.Remove(2)
	if got := s.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
	clk.Advance(10 * time.Second)
	time.Sleep(10 * time.Millisecond)
	if got := rec.total(); got != 3 {
		t.Errorf("removed entry ran %d times, want 3", got)
	}
}

func TestRunCancelsInFlight(t *testing.T) {
	clk := clock.NewFake(t0)
	s := New(clk, discardLogger())
	e, started, canceled := blockingEntry(1, t0)
	s.Add(e)
	stop := start(t, s)
	waitChan(t, "run start", started)
	// Run returns without waiting for the in-flight run, whose ctx ends.
	stop()
	waitChan(t, "cancellation of the in-flight run", canceled)
}

func TestInvalidEntryIgnored(t *testing.T) {
	s := New(clock.NewFake(t0), discardLogger())
	s.Add(Entry{ID: 1, Frequency: 0, Run: func(context.Context, time.Time) {}})
	s.Add(Entry{ID: 2, Frequency: time.Second})
	if got := s.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}
