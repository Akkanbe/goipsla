// Package clock abstracts time so that schedulers, timeouts and statistics can
// be tested deterministically.
//
// Production code obtains time only through a Clock. Real returns the system
// clock; Fake is a manually advanced clock for unit tests. Calling time.Now or
// time.NewTimer directly is reserved for this package and for main packages.
package clock

import "time"

// Clock is the source of time used by goipslad.
type Clock interface {
	// Now returns the current time. For Real it carries a monotonic reading,
	// so differences computed with Sub or Since are immune to wall-clock steps.
	Now() time.Time
	// Since returns the time elapsed since t.
	Since(t time.Time) time.Duration
	// NewTimer creates a Timer that sends the current time on its channel
	// after at least d has elapsed.
	NewTimer(d time.Duration) Timer
	// After waits for d to elapse and then sends the current time on the
	// returned channel. Equivalent to NewTimer(d).C().
	After(d time.Duration) <-chan time.Time
	// Sleep blocks for at least d.
	Sleep(d time.Duration)
}

// Timer is a single-shot timer with the Go 1.23+ time.Timer semantics: after
// Stop or Reset returns, no stale value is received from C.
type Timer interface {
	// C returns the channel on which the expiry time is delivered.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether the call
	// stopped the timer; false means it had already expired (and its value
	// was received) or had been stopped.
	Stop() bool
	// Reset changes the timer to expire after d. It reports whether the timer
	// was active before the call, with the same meaning as Stop.
	Reset(d time.Duration) bool
}

// Real returns the system clock.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (realClock) NewTimer(d time.Duration) Timer         { return realTimer{time.NewTimer(d)} }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) Sleep(d time.Duration)                  { time.Sleep(d) }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
