// The fake clock implements Clock with the same timer semantics as the real
// one, and the tests of both read its internals: part of the clock unit.
//
//declscope:namespace clock

package clock

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Fake is a Clock for unit tests. Its time only moves when Advance or Set is
// called; timers whose deadline has been reached then fire in deadline order
// (timers with equal deadlines fire in the order they were armed).
//
// All methods are safe for concurrent use: one goroutine may drive the clock
// with Advance while others call Now, NewTimer or Sleep.
//
// Fake has a single timeline. Timer deadlines are absolute points on it, so
// moving the clock backwards with Set delays pending timers accordingly. Real
// timers use the monotonic clock and would not be affected by a wall-clock
// step; tests that need that distinction must model it themselves.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	timers  timerHeap
	seq     uint64        // arming order, breaks ties between equal deadlines
	changed chan struct{} // closed and replaced whenever the set of pending timers changes
}

var _ Clock = (*Fake)(nil)

// NewFake returns a Fake clock set to start. Any monotonic reading in start
// is stripped so that the fake timeline is purely wall-clock based.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start.Round(0), changed: make(chan struct{})}
}

// Now returns the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the fake time elapsed since t.
func (f *Fake) Since(t time.Time) time.Duration {
	return f.Now().Sub(t)
}

// NewTimer creates a timer that fires once the fake time reaches Now()+d.
// A timer with d <= 0 fires immediately.
func (f *Fake) NewTimer(d time.Duration) Timer {
	t := &fakeTimer{clock: f, ch: make(chan time.Time, 1), index: -1}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armLocked(t, d)
	return t
}

// After is equivalent to NewTimer(d).C().
func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.NewTimer(d).C()
}

// Sleep blocks until the fake time has advanced by at least d.
func (f *Fake) Sleep(d time.Duration) {
	<-f.NewTimer(d).C()
}

// Advance moves the fake time forward by d, firing every timer whose deadline
// falls within the interval in deadline order. Each timer delivers its own
// deadline on its channel. Advance holds the clock's lock throughout, so a
// goroutine woken by a timer that calls Now sees the old time plus d. To step
// through the intermediate instants, advance in smaller steps and use
// BlockUntil to wait for re-armed timers. A negative d is treated as zero.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		d = 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advanceToLocked(f.now.Add(d))
}

// Set moves the fake time to t. If t is later than the current time, the
// timers due up to t fire as with Advance. If t is earlier, the time is simply
// set back and pending timers keep their absolute deadlines.
func (f *Fake) Set(t time.Time) {
	t = t.Round(0)
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.After(f.now) {
		f.advanceToLocked(t)
		return
	}
	f.now = t
}

// Pending returns the number of armed timers that have not fired yet,
// including the timers behind Sleep and After calls.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// BlockUntil blocks until at least n timers are pending. Tests call it before
// Advance to make sure the goroutine under test has armed its timer.
func (f *Fake) BlockUntil(n int) {
	_ = f.BlockUntilContext(context.Background(), n)
}

// BlockUntilContext is BlockUntil with cancellation. It returns ctx.Err() if
// ctx is done before n timers are pending.
func (f *Fake) BlockUntilContext(ctx context.Context, n int) error {
	for {
		f.mu.Lock()
		pending, changed := len(f.timers), f.changed
		f.mu.Unlock()
		if pending >= n {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// advanceToLocked fires all timers due at or before target and then sets the
// time to target. f.mu must be held.
func (f *Fake) advanceToLocked(target time.Time) {
	for len(f.timers) > 0 && !f.timers[0].deadline.After(target) {
		t := heap.Pop(&f.timers).(*fakeTimer)
		if t.deadline.After(f.now) {
			f.now = t.deadline
		}
		t.ch <- f.now // never blocks: the channel is drained whenever the timer is re-armed
		f.notifyLocked()
	}
	f.now = target
}

// armLocked schedules t to fire after d. t must not be pending and its
// channel must be empty. f.mu must be held.
func (f *Fake) armLocked(t *fakeTimer, d time.Duration) {
	if d <= 0 {
		t.ch <- f.now
		return
	}
	f.seq++
	t.deadline = f.now.Add(d)
	t.seq = f.seq
	heap.Push(&f.timers, t)
	f.notifyLocked()
}

// disarmLocked removes t from the pending set and discards a fired but
// unreceived value, matching the Go 1.23+ time.Timer semantics. It reports
// whether the timer was active: pending, or fired with its value not yet
// received. f.mu must be held.
func (f *Fake) disarmLocked(t *fakeTimer) bool {
	active := false
	if t.index >= 0 {
		heap.Remove(&f.timers, t.index)
		f.notifyLocked()
		active = true
	}
	select {
	case <-t.ch:
		active = true
	default:
	}
	return active
}

func (f *Fake) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

type fakeTimer struct {
	clock    *Fake
	ch       chan time.Time
	deadline time.Time
	seq      uint64
	index    int // position in the heap, -1 when not pending
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	return t.clock.disarmLocked(t)
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	active := t.clock.disarmLocked(t)
	t.clock.armLocked(t, d)
	return active
}

// timerHeap orders pending timers by deadline, then by arming order.
type timerHeap []*fakeTimer

func (h timerHeap) Len() int { return len(h) }

func (h timerHeap) Less(i, j int) bool {
	if !h[i].deadline.Equal(h[j].deadline) {
		return h[i].deadline.Before(h[j].deadline)
	}
	return h[i].seq < h[j].seq
}

func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *timerHeap) Push(x any) {
	t := x.(*fakeTimer)
	t.index = len(*h)
	*h = append(*h, t)
}

func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	t.index = -1
	*h = old[:n-1]
	return t
}
