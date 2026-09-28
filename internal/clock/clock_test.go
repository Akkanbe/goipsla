package clock

import (
	"container/heap"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// received returns the value waiting on ch, if any, without blocking.
func received(ch <-chan time.Time) (time.Time, bool) {
	select {
	case v := <-ch:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestFakeNowAdvanceSet(t *testing.T) {
	f := NewFake(epoch)
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	f.Advance(1500 * time.Millisecond)
	if got, want := f.Now(), epoch.Add(1500*time.Millisecond); !got.Equal(want) {
		t.Fatalf("after Advance: Now() = %v, want %v", got, want)
	}
	if got := f.Since(epoch); got != 1500*time.Millisecond {
		t.Fatalf("Since(epoch) = %v, want 1.5s", got)
	}
	f.Advance(-time.Hour)
	if got := f.Since(epoch); got != 1500*time.Millisecond {
		t.Fatalf("negative Advance moved the clock: Since(epoch) = %v", got)
	}
	back := epoch.Add(-time.Minute)
	f.Set(back)
	if got := f.Now(); !got.Equal(back) {
		t.Fatalf("after Set: Now() = %v, want %v", got, back)
	}
}

func TestFakeStripsMonotonic(t *testing.T) {
	start := time.Now() // carries a monotonic reading
	f := NewFake(start)
	if got, want := f.Now().String(), start.Round(0).String(); got != want {
		t.Fatalf("Now() = %s, want %s (monotonic reading must be stripped)", got, want)
	}
}

func TestFakeTimersFireInDeadlineOrder(t *testing.T) {
	f := NewFake(epoch)
	timers := map[string]Timer{
		"c": f.NewTimer(3 * time.Second),
		"a": f.NewTimer(1 * time.Second),
		"b": f.NewTimer(2 * time.Second),
		"d": f.NewTimer(5 * time.Second),
	}
	f.Advance(3 * time.Second)
	// Each timer delivers its own deadline. Firing out of order would hand an
	// earlier timer the later time the clock had already reached.
	for name, d := range map[string]time.Duration{"a": 1 * time.Second, "b": 2 * time.Second, "c": 3 * time.Second} {
		if v, ok := received(timers[name].C()); !ok || !v.Equal(epoch.Add(d)) {
			t.Fatalf("timer %s: got (%v, %v), want fire at +%v", name, v, ok, d)
		}
	}
	if _, ok := received(timers["d"].C()); ok {
		t.Fatal("timer d fired before its deadline")
	}
	if n := f.Pending(); n != 1 {
		t.Fatalf("Pending() = %d, want 1", n)
	}
	f.Advance(2 * time.Second)
	if v, ok := received(timers["d"].C()); !ok || !v.Equal(epoch.Add(5*time.Second)) {
		t.Fatalf("timer d: got (%v, %v), want fire at +5s", v, ok)
	}
}

// TestFakeFiringOrderTies checks that timers with equal deadlines fire in
// arming order, and that Reset counts as re-arming.
func TestFakeFiringOrderTies(t *testing.T) {
	f := NewFake(epoch)
	t1 := f.NewTimer(time.Second)
	t2 := f.NewTimer(time.Second)
	t0 := f.NewTimer(500 * time.Millisecond)
	t1.Reset(time.Second) // now armed after t2

	names := map[*fakeTimer]string{t0.(*fakeTimer): "t0", t1.(*fakeTimer): "t1", t2.(*fakeTimer): "t2"}
	var order []string
	f.mu.Lock()
	for f.timers.Len() > 0 {
		order = append(order, names[heap.Pop(&f.timers).(*fakeTimer)])
	}
	f.mu.Unlock()
	if got, want := strings.Join(order, ","), "t0,t2,t1"; got != want {
		t.Fatalf("firing order = %s, want %s", got, want)
	}
}

func TestFakeTimerDeliversDeadline(t *testing.T) {
	f := NewFake(epoch)
	tm := f.NewTimer(2 * time.Second)
	done := make(chan time.Time)
	go func() {
		v := <-tm.C()
		done <- v
	}()
	f.Advance(10 * time.Second)
	if v := <-done; !v.Equal(epoch.Add(2 * time.Second)) {
		t.Fatalf("timer delivered %v, want its deadline %v", v, epoch.Add(2*time.Second))
	}
	if got := f.Now(); !got.Equal(epoch.Add(10 * time.Second)) {
		t.Fatalf("Now() after Advance = %v, want +10s", got)
	}
}

func TestFakeTimerNotDueDoesNotFire(t *testing.T) {
	f := NewFake(epoch)
	tm := f.NewTimer(time.Second)
	f.Advance(999 * time.Millisecond)
	if _, ok := received(tm.C()); ok {
		t.Fatal("timer fired before its deadline")
	}
	f.Advance(time.Millisecond)
	if _, ok := received(tm.C()); !ok {
		t.Fatal("timer did not fire at its deadline")
	}
}

func TestFakeZeroDurationFiresImmediately(t *testing.T) {
	f := NewFake(epoch)
	for _, d := range []time.Duration{0, -time.Second} {
		tm := f.NewTimer(d)
		if v, ok := received(tm.C()); !ok || !v.Equal(epoch) {
			t.Fatalf("NewTimer(%v): got (%v, %v), want immediate fire at %v", d, v, ok, epoch)
		}
	}
	if v, ok := received(f.After(0)); !ok || !v.Equal(epoch) {
		t.Fatalf("After(0): got (%v, %v), want immediate fire", v, ok)
	}
	f.Sleep(0) // must not block
	if n := f.Pending(); n != 0 {
		t.Fatalf("Pending() = %d, want 0", n)
	}
}

func TestFakeTimerStop(t *testing.T) {
	f := NewFake(epoch)

	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop on a pending timer returned false")
	}
	if tm.Stop() {
		t.Fatal("second Stop returned true")
	}
	f.Advance(2 * time.Second)
	if _, ok := received(tm.C()); ok {
		t.Fatal("stopped timer fired")
	}

	// Fired but not received: Stop discards the stale value and reports true,
	// like time.Timer since Go 1.23.
	tm = f.NewTimer(time.Second)
	f.Advance(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop on a fired, unreceived timer returned false")
	}
	if _, ok := received(tm.C()); ok {
		t.Fatal("stale value received after Stop")
	}

	// Fired and received: Stop reports false.
	tm = f.NewTimer(time.Second)
	f.Advance(time.Second)
	<-tm.C()
	if tm.Stop() {
		t.Fatal("Stop after the value was received returned true")
	}
	if n := f.Pending(); n != 0 {
		t.Fatalf("Pending() = %d, want 0", n)
	}
}

func TestFakeTimerReset(t *testing.T) {
	f := NewFake(epoch)

	tm := f.NewTimer(time.Second)
	if !tm.Reset(3 * time.Second) {
		t.Fatal("Reset on a pending timer returned false")
	}
	f.Advance(2 * time.Second)
	if _, ok := received(tm.C()); ok {
		t.Fatal("timer fired at its old deadline after Reset")
	}
	f.Advance(time.Second)
	if v, ok := received(tm.C()); !ok || !v.Equal(epoch.Add(3*time.Second)) {
		t.Fatalf("got (%v, %v), want fire at +3s", v, ok)
	}

	// Reset after the value was received reports false and re-arms.
	if tm.Reset(time.Second) {
		t.Fatal("Reset on an expired timer returned true")
	}
	if n := f.Pending(); n != 1 {
		t.Fatalf("Pending() = %d, want 1", n)
	}

	// Reset discards a fired, unreceived value.
	f.Advance(time.Second)
	if !tm.Reset(time.Second) {
		t.Fatal("Reset on a fired, unreceived timer returned false")
	}
	if _, ok := received(tm.C()); ok {
		t.Fatal("stale value received after Reset")
	}
	f.Advance(time.Second)
	if v, ok := received(tm.C()); !ok || !v.Equal(epoch.Add(5*time.Second)) {
		t.Fatalf("got (%v, %v), want fire at +5s", v, ok)
	}

	// Reset of a stopped timer re-arms it.
	tm.Reset(time.Second)
	tm.Stop()
	if tm.Reset(time.Second) {
		t.Fatal("Reset on a stopped timer returned true")
	}
	f.Advance(time.Second)
	if _, ok := received(tm.C()); !ok {
		t.Fatal("timer re-armed after Stop did not fire")
	}
}

func TestFakeSetForwardFiresAndBackwardDelays(t *testing.T) {
	f := NewFake(epoch)
	a := f.NewTimer(time.Minute)
	b := f.NewTimer(time.Hour)

	f.Set(epoch.Add(-time.Hour)) // backwards: nothing fires, deadlines stay absolute
	if _, ok := received(a.C()); ok {
		t.Fatal("timer fired when the clock moved backwards")
	}
	f.Set(epoch.Add(30 * time.Minute))
	if v, ok := received(a.C()); !ok || !v.Equal(epoch.Add(time.Minute)) {
		t.Fatalf("a: got (%v, %v), want fire at its deadline", v, ok)
	}
	if _, ok := received(b.C()); ok {
		t.Fatal("b fired early")
	}
	if got := f.Now(); !got.Equal(epoch.Add(30 * time.Minute)) {
		t.Fatalf("Now() = %v, want +30m", got)
	}
}

func TestFakeSleepAndBlockUntil(t *testing.T) {
	f := NewFake(epoch)
	woke := make(chan time.Time)
	go func() {
		f.Sleep(5 * time.Second)
		woke <- f.Now()
	}()
	go func() {
		<-f.After(7 * time.Second)
		woke <- f.Now()
	}()
	f.BlockUntil(2)
	f.Advance(5 * time.Second)
	if v := <-woke; v.Before(epoch.Add(5 * time.Second)) {
		t.Fatalf("Sleep returned at %v, before +5s", v)
	}
	f.Advance(2 * time.Second)
	if v := <-woke; !v.Equal(epoch.Add(7 * time.Second)) {
		t.Fatalf("After returned at %v, want +7s", v)
	}
}

func TestFakeBlockUntilContext(t *testing.T) {
	f := NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.BlockUntilContext(ctx, 1); err != context.Canceled {
		t.Fatalf("BlockUntilContext = %v, want context.Canceled", err)
	}
	f.NewTimer(time.Second)
	if err := f.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("BlockUntilContext with 1 pending timer = %v, want nil", err)
	}
}

// TestFakeConcurrentUse is meant to run under -race: readers call Now and arm
// timers while another goroutine advances the clock.
func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake(epoch)
	const workers = 4
	const steps = 200
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range workers {
		wg.Go(func() {
			last := f.Now()
			for {
				select {
				case <-stop:
					return
				default:
				}
				now := f.Now()
				if now.Before(last) {
					t.Errorf("time went backwards: %v after %v", now, last)
					return
				}
				last = now
				tm := f.NewTimer(time.Millisecond)
				if f.Since(now) < 0 {
					t.Errorf("negative Since")
				}
				tm.Stop()
			}
		})
	}
	wg.Go(func() {
		for range steps {
			f.Advance(time.Millisecond)
		}
		close(stop)
	})
	wg.Wait()
	if got, want := f.Now(), epoch.Add(steps*time.Millisecond); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}

// TestFakeTickerPattern drives a goroutine that re-arms its timer after each
// expiry, the pattern the scheduler uses.
func TestFakeTickerPattern(t *testing.T) {
	f := NewFake(epoch)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tm := f.NewTimer(time.Second)
		defer tm.Stop()
		for {
			select {
			case v := <-tm.C():
				ticks <- v
				tm.Reset(time.Second)
			case <-ctx.Done():
				return
			}
		}
	}()
	for i := 1; i <= 5; i++ {
		f.BlockUntil(1)
		f.Advance(time.Second)
		if v := <-ticks; !v.Equal(epoch.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("tick %d at %v, want +%ds", i, v, i)
		}
	}
}

func TestRealClock(t *testing.T) {
	c := Real()
	start := c.Now()
	tm := c.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(5 * time.Second):
		t.Fatal("real timer did not fire")
	}
	if tm.Stop() {
		t.Fatal("Stop after the value was received returned true")
	}
	if tm.Reset(time.Hour) {
		t.Fatal("Reset on an expired timer returned true")
	}
	if !tm.Stop() {
		t.Fatal("Stop on a pending timer returned false")
	}
	<-c.After(time.Millisecond)
	c.Sleep(time.Millisecond)
	if d := c.Since(start); d < 2*time.Millisecond {
		t.Fatalf("Since(start) = %v, want >= 2ms", d)
	}
}
