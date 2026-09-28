// Package sched runs entries periodically on a fixed time lattice.
//
// Every entry fires at First + k×Frequency (k = 0, 1, 2, ...). All entries
// share one min-heap ordered by their next firing time and one clock timer
// armed for the head of the heap, so thousands of operations cost a single
// goroutine while idle. See docs/scheduling.md.
package sched

import (
	"container/heap"
	"context"
	"encoding/binary"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"goipsla/internal/clock"
)

// Entry is the unit of periodic execution.
type Entry struct {
	ID        int
	Frequency time.Duration
	First     time.Time // first firing time; the lattice origin
	// Run is the body. The Scheduler calls it on a new goroutine with the
	// lattice time it fires for. Its ctx ends when the Scheduler stops or
	// the entry is removed or replaced.
	Run func(ctx context.Context, now time.Time)
	// Busy is called instead of Run, on the scheduler goroutine, when the
	// previous Run has not returned yet. It should return quickly. May be
	// nil.
	Busy func(now time.Time)
}

// Scheduler fires entries on their lattices. Add and Remove may be called
// before and while Run is running, from any goroutine.
type Scheduler struct {
	clk    clock.Clock
	logger *slog.Logger
	wake   chan struct{} // buffered(1): the heap head changed

	mu     sync.Mutex
	items  itemHeap
	byID   map[int]*item
	runCtx context.Context // set while Run is running
	gen    uint64          // incremented by every Add, effective Remove and firing

	// armedGen is the gen the timer was last armed for: once it equals gen,
	// the timer reflects every Add and Remove so far. Tests use it to wait
	// for the scheduler to settle.
	armedGen atomic.Uint64
}

type item struct {
	e      Entry
	next   time.Time          // next lattice time to fire at
	index  int                // position in the heap, -1 once removed
	active bool               // a Run goroutine for this item has not returned
	cancel context.CancelFunc // cancels the in-flight Run, if any
}

// New returns a Scheduler that reads time from clk. A nil logger means
// slog.Default().
func New(clk clock.Clock, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		clk:    clk,
		logger: logger,
		wake:   make(chan struct{}, 1),
		byID:   make(map[int]*item),
	}
}

// Add registers e. An entry with the same ID is replaced: its in-flight Run,
// if any, is canceled and the new entry does not inherit its busy state.
// Entries with a non-positive Frequency or a nil Run are rejected with an
// error log. A zero First means "now".
func (s *Scheduler) Add(e Entry) {
	if e.Frequency <= 0 || e.Run == nil {
		s.logger.Error("sched: invalid entry ignored", "op", e.ID, "frequency", e.Frequency, "run_nil", e.Run == nil)
		return
	}
	if e.First.IsZero() {
		e.First = s.clk.Now()
	}
	s.mu.Lock()
	if old, ok := s.byID[e.ID]; ok {
		s.removeLocked(old)
	}
	it := &item{e: e, next: e.First}
	s.byID[e.ID] = it
	heap.Push(&s.items, it)
	s.gen++
	s.mu.Unlock()
	s.poke()
}

// Remove unregisters the entry with the given ID and cancels its in-flight
// Run. Removing an unknown ID is a no-op.
func (s *Scheduler) Remove(id int) {
	s.mu.Lock()
	it, ok := s.byID[id]
	if ok {
		s.removeLocked(it)
		s.gen++
	}
	s.mu.Unlock()
	if ok {
		s.poke()
	}
}

// Settled reports whether the timer is armed for every Add, Remove and
// firing so far. Tests that drive a fake clock wait for it before advancing
// the clock, so that the timer is not armed relative to a time that has
// already moved on.
func (s *Scheduler) Settled() bool {
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	return s.armedGen.Load() == gen
}

// Idle reports whether no Run of a registered entry is in flight.
func (s *Scheduler) Idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.byID {
		if it.active {
			return false
		}
	}
	return true
}

// Len returns the number of registered entries.
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func (s *Scheduler) removeLocked(it *item) {
	delete(s.byID, it.e.ID)
	if it.index >= 0 {
		heap.Remove(&s.items, it.index)
	}
	if it.cancel != nil {
		it.cancel()
	}
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run fires entries until ctx is done and then returns nil. It does not wait
// for in-flight Run calls; their contexts, derived from ctx, are canceled.
// Run must not be called concurrently with itself.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.runCtx = nil
		s.mu.Unlock()
	}()

	// The timer is created on first use rather than armed with a dummy
	// duration, so that a fake clock never sees a timer that does not
	// correspond to an entry.
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		// fireDue below sees every change made before this point, so a
		// pending wake-up is stale.
		select {
		case <-s.wake:
		default:
		}
		wait, ok, gen := s.fireDue()
		var timerC <-chan time.Time
		switch {
		case ok && timer == nil:
			timer = s.clk.NewTimer(wait)
		case ok:
			timer.Reset(wait)
		case timer != nil:
			timer.Stop()
		}
		if ok {
			timerC = timer.C()
		}
		s.armedGen.Store(gen)
		select {
		case <-ctx.Done():
			return nil
		case <-timerC:
		case <-s.wake:
		}
	}
}

// fireDue fires every entry whose next time has come and returns how long to
// wait for the new head of the heap (ok is false when the heap is empty),
// together with the gen it saw. Busy callbacks run after the lock is
// released, so they may call Add or Remove.
func (s *Scheduler) fireDue() (wait time.Duration, ok bool, gen uint64) {
	var busy []func()
	defer func() {
		for _, f := range busy {
			f()
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clk.Now()
	skipped := 0
	var maxLate time.Duration // the largest delay behind a skipped lattice point
	defer func() {
		if skipped > 0 {
			s.logger.Warn("sched: runs missed while the scheduler was late were skipped", "entries", skipped,
				"max_delay_ms", float64(maxLate)/float64(time.Millisecond))
		}
	}()
	for len(s.items) > 0 {
		it := s.items[0]
		if it.next.After(now) {
			return it.next.Sub(now), true, s.gen
		}
		// Fire once for the latest lattice point not after now. Points
		// missed while the process was stopped or starved are skipped,
		// not replayed back to back.
		at := it.next
		if missed := now.Sub(at) / it.e.Frequency; missed > 0 {
			maxLate = max(maxLate, now.Sub(at))
			at = at.Add(missed * it.e.Frequency)
			skipped++
		}
		it.next = at.Add(it.e.Frequency)
		heap.Fix(&s.items, 0)
		s.gen++
		if f := s.fireLocked(it, at); f != nil {
			busy = append(busy, f)
		}
	}
	return 0, false, s.gen
}

// fireLocked starts the entry's Run on a goroutine, or returns the Busy call
// to make when the previous Run is still in flight. s.mu must be held.
func (s *Scheduler) fireLocked(it *item, at time.Time) (busy func()) {
	if it.active {
		if b := it.e.Busy; b != nil {
			return func() { b(at) }
		}
		return nil
	}
	ctx, cancel := context.WithCancel(s.runCtx)
	it.active = true
	it.cancel = cancel
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			it.active = false
			it.cancel = nil
			s.mu.Unlock()
		}()
		it.e.Run(ctx, at)
	}()
	return nil
}

// Offset is the dispersion offset of an entry's first run: FNV-1a (64 bit)
// of the ID as an 8-byte big-endian integer, modulo freq. It depends only on
// (id, freq), so it is stable across restarts. It returns 0 if freq <= 0.
func Offset(id int, freq time.Duration) time.Duration {
	if freq <= 0 {
		return 0
	}
	h := fnv.New64a()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(int64(id)))
	h.Write(b[:])
	return time.Duration(h.Sum64() % uint64(freq))
}

// itemHeap orders items by next firing time, then by ID for determinism.
type itemHeap []*item

func (h itemHeap) Len() int { return len(h) }

func (h itemHeap) Less(i, j int) bool {
	if !h[i].next.Equal(h[j].next) {
		return h[i].next.Before(h[j].next)
	}
	return h[i].e.ID < h[j].e.ID
}

func (h itemHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *itemHeap) Push(x any) {
	it := x.(*item)
	it.index = len(*h)
	*h = append(*h, it)
}

func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	it.index = -1
	*h = old[:n-1]
	return it
}
