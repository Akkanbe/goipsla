// Tests of lifecycle.go and reload.go, which share the manager namespace;
// they use the helpers of manager_test.go.
//
//declscope:namespace manager

package manager

import (
	"bytes"
	"errors"
	"log/slog"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/op"
	"goipsla/internal/probe"
	"goipsla/internal/react"
	"goipsla/internal/sched"
	"goipsla/internal/stats"
)

// rig is a manager with a store, a fake clock and a recording sink.
type rig struct {
	m     *Manager
	clk   *clock.Fake
	store *stats.Store
	rec   *sinkRecorder
	stop  func()
}

func newRig(t *testing.T, ops ...*config.Operation) *rig {
	t.Helper()
	return newRigConfig(t, &config.Config{Operations: ops})
}

func newRigConfig(t *testing.T, cfg *config.Config) *rig {
	t.Helper()
	clk := clock.NewFake(t0)
	store := stats.NewStore(clk)
	rec := &sinkRecorder{}
	sink := func(r op.Result) {
		store.Record(r)
		rec.sink(r)
	}
	m := New(cfg, &fakeEngine{}, clk, sink, discardLogger())
	m.SetStore(store)
	r := &rig{m: m, clk: clk, store: store, rec: rec}
	r.stop = start(t, m)
	t.Cleanup(r.stop)
	return r
}

func (r *rig) advance(t *testing.T, d time.Duration) {
	t.Helper()
	advance(t, r.m, r.clk, d)
}

// steps advances the clock one second at a time, waiting after each step
// for operation id to reach the given number of attempts.
func (r *rig) steps(t *testing.T, id int, n int) {
	t.Helper()
	base := r.snap(t, id).Totals.Initiations
	for i := 1; i <= n; i++ {
		r.advance(t, time.Second)
		r.waitInitiations(t, id, base+uint64(i))
	}
}

func (r *rig) snap(t *testing.T, id int) *stats.Snapshot {
	t.Helper()
	s, ok := r.store.Snapshot(id, stats.SnapshotOptions{})
	if !ok {
		t.Fatalf("no statistics row for operation %d", id)
	}
	return s
}

func (r *rig) waitState(t *testing.T, id int, want string) {
	t.Helper()
	waitFor(t, "operation "+want, func() bool { s, _ := r.m.State(id); return s == want })
}

func (r *rig) waitInitiations(t *testing.T, id int, n uint64) {
	t.Helper()
	waitFor(t, "attempts", func() bool {
		s, ok := r.store.Snapshot(id, stats.SnapshotOptions{})
		return ok && s.Totals.Initiations == n
	})
}

// scheduled returns an echo operation that starts at the given time and
// runs every second.
func scheduled(id int, s *config.Schedule) *config.Operation {
	c := echoOp(id, "192.0.2.1")
	c.Frequency = time.Second
	c.Timeout = 500 * time.Millisecond
	c.Schedule = s
	return c
}

func TestInitialTransitions(t *testing.T) {
	now := echoOp(1, "192.0.2.1")
	after := scheduled(2, &config.Schedule{Start: config.StartAfter, After: 30 * time.Second, Forever: true})
	at := scheduled(3, &config.Schedule{Start: config.StartAt, At: t0.Add(time.Hour), Life: time.Minute})
	past := scheduled(4, &config.Schedule{Start: config.StartAt, At: t0.Add(-time.Hour), Life: time.Minute})
	pending := scheduled(5, &config.Schedule{Start: config.StartPending, Forever: true, Ageout: time.Hour})
	r := newRig(t, now, after, at, past, pending)

	tests := []struct {
		id        int
		state     string
		lifeStart time.Time
		nextStart time.Time
		lifeLeft  *time.Duration
		ageout    *time.Duration
	}{
		{1, stateActive, t0.Add(sched.Offset(1, now.Frequency)), time.Time{}, nil, nil},
		{2, statePending, t0.Add(30 * time.Second), t0.Add(30 * time.Second), nil, nil},
		{3, statePending, t0.Add(time.Hour), t0.Add(time.Hour), dur(0), nil},
		{4, stateActive, t0, time.Time{}, dur(time.Minute), nil},
		{5, statePending, t0, time.Time{}, nil, dur(time.Hour)},
	}
	for _, tt := range tests {
		info, ok := r.m.Info(tt.id)
		if !ok {
			t.Fatalf("Info(%d) not found", tt.id)
		}
		if info.State != tt.state || !info.NextStart.Equal(tt.nextStart) ||
			!equalDur(info.LifeLeft, tt.lifeLeft) || !equalDur(info.AgeoutLeft, tt.ageout) {
			t.Errorf("Info(%d) = %+v (life %v, ageout %v), want state %s next %v life %v ageout %v",
				tt.id, info, fmtDur(info.LifeLeft), fmtDur(info.AgeoutLeft), tt.state, tt.nextStart, fmtDur(tt.lifeLeft), fmtDur(tt.ageout))
		}
		if s := r.snap(t, tt.id); !s.LifeStart.Equal(tt.lifeStart) || s.LifeIndex != 1 {
			t.Errorf("operation %d: life %d starts at %v, want life 1 at %v", tt.id, s.LifeIndex, s.LifeStart, tt.lifeStart)
		}
	}

	// after: pending until its start time, then active in the same life.
	r.advance(t, 30*time.Second)
	r.waitState(t, 2, stateActive)
	r.waitInitiations(t, 2, 1)
	if s := r.snap(t, 2); s.LifeIndex != 1 || !s.Latest.Start.Equal(t0.Add(30*time.Second)) {
		t.Errorf("operation 2 after its start: life %d, latest %+v", s.LifeIndex, s.Latest)
	}
}

func dur(d time.Duration) *time.Duration { return &d }

func equalDur(a, b *time.Duration) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func fmtDur(d *time.Duration) string {
	if d == nil {
		return "nil"
	}
	return d.String()
}

func TestLifeEnds(t *testing.T) {
	c := scheduled(7, &config.Schedule{Start: config.StartAt, At: t0, Life: 5 * time.Second})
	r := newRig(t, c)
	r.waitInitiations(t, 7, 1)
	for i := 1; i <= 5; i++ {
		r.advance(t, time.Second)
		if i < 5 {
			r.waitInitiations(t, 7, uint64(i+1))
		}
	}
	r.waitState(t, 7, stateInactive)
	// Attempts at t0 .. t0+4s; the lattice point t0+5s is the end of the life.
	r.advance(t, 10*time.Second)
	if s := r.snap(t, 7); s.Totals.Initiations != 5 || s.LifeIndex != 1 {
		t.Errorf("after the life: life %d with %d attempts, want life 1 with 5 (frozen)", s.LifeIndex, s.Totals.Initiations)
	}
	info, _ := r.m.Info(7)
	if info.LifeLeft == nil || *info.LifeLeft != 0 {
		t.Errorf("LifeLeft of an inactive operation = %v, want 0", fmtDur(info.LifeLeft))
	}
	if got := len(r.rec.get()); got != 5 {
		t.Errorf("sink got %d results, want 5", got)
	}
}

func TestLifeLeftCountsDown(t *testing.T) {
	c := scheduled(8, &config.Schedule{Start: config.StartAt, At: t0, Life: time.Hour})
	r := newRig(t, c)
	r.waitInitiations(t, 8, 1)
	r.advance(t, 1500*time.Millisecond)
	info, _ := r.m.Info(8)
	if info.LifeLeft == nil || *info.LifeLeft != time.Hour-1500*time.Millisecond {
		t.Errorf("LifeLeft = %v, want %v", fmtDur(info.LifeLeft), time.Hour-1500*time.Millisecond)
	}
}

func TestRecurring(t *testing.T) {
	start := t0.Add(time.Hour)
	c := scheduled(9, &config.Schedule{Start: config.StartAt, At: start, Life: 10 * time.Second, Recurring: true})
	r := newRig(t, c)
	r.waitState(t, 9, statePending)

	r.advance(t, time.Hour) // day 1 start
	r.waitState(t, 9, stateActive)
	r.waitInitiations(t, 9, 1)
	r.advance(t, 10*time.Second) // day 1 life ends
	r.waitState(t, 9, stateInactive)
	info, _ := r.m.Info(9)
	if !info.NextStart.Equal(start.Add(24 * time.Hour)) {
		t.Errorf("NextStart after the life = %v, want %v", info.NextStart, start.Add(24*time.Hour))
	}
	if s := r.snap(t, 9); s.LifeIndex != 1 {
		t.Errorf("day 1 life index = %d, want 1", s.LifeIndex)
	}

	r.advance(t, 24*time.Hour-10*time.Second) // day 2 start: a new life
	r.waitState(t, 9, stateActive)
	r.waitInitiations(t, 9, 1)
	s := r.snap(t, 9)
	if s.LifeIndex != 2 || !s.LifeStart.Equal(start.Add(24*time.Hour)) {
		t.Errorf("day 2: life %d starting %v, want life 2 starting %v", s.LifeIndex, s.LifeStart, start.Add(24*time.Hour))
	}
	if got := r.rec.get(); got[len(got)-1].Seq != 1 {
		t.Errorf("first attempt of the new life has Seq %d, want 1", got[len(got)-1].Seq)
	}
}

func TestAgeoutPending(t *testing.T) {
	c := scheduled(10, &config.Schedule{Start: config.StartPending, Forever: true, Ageout: 30 * time.Second})
	other := echoOp(11, "192.0.2.11")
	r := newRig(t, c, other)
	r.advance(t, 29*time.Second)
	if _, ok := r.m.State(10); !ok {
		t.Fatal("operation aged out early")
	}
	r.advance(t, time.Second)
	waitFor(t, "ageout", func() bool { _, ok := r.m.State(10); return !ok })
	if _, ok := r.store.Snapshot(10, stats.SnapshotOptions{}); ok {
		t.Error("statistics row of the aged-out operation still exists")
	}
	if ids := r.m.IDs(); len(ids) != 1 || ids[0] != 11 {
		t.Errorf("IDs() = %v, want [11]", ids)
	}
}

// TestAgeoutPausedWhileActive checks that ageout only counts while the
// operation is not active: it starts when the life ends.
func TestAgeoutPausedWhileActive(t *testing.T) {
	c := scheduled(12, &config.Schedule{Start: config.StartAt, At: t0, Life: 10 * time.Second, Ageout: 20 * time.Second})
	r := newRig(t, c)
	r.waitInitiations(t, 12, 1)
	if info, _ := r.m.Info(12); info.AgeoutLeft != nil {
		t.Errorf("AgeoutLeft while active = %v, want nil", fmtDur(info.AgeoutLeft))
	}
	r.advance(t, 10*time.Second)
	r.waitState(t, 12, stateInactive)
	if info, _ := r.m.Info(12); !equalDur(info.AgeoutLeft, dur(20*time.Second)) {
		t.Errorf("AgeoutLeft at the end of the life = %v, want 20s", fmtDur(info.AgeoutLeft))
	}
	r.advance(t, 19*time.Second)
	if _, ok := r.m.State(12); !ok {
		t.Fatal("aged out before 20 s of inactivity")
	}
	r.advance(t, time.Second)
	waitFor(t, "ageout", func() bool { _, ok := r.m.State(12); return !ok })
}

// TestAgeoutResetByActivation: a recurring operation's ageout restarts at
// every life end; activation stops it.
func TestAgeoutResetByActivation(t *testing.T) {
	start := t0.Add(time.Minute)
	c := scheduled(13, &config.Schedule{Start: config.StartAt, At: start, Life: time.Hour, Ageout: 24 * time.Hour, Recurring: true})
	r := newRig(t, c)
	if info, _ := r.m.Info(13); !equalDur(info.AgeoutLeft, dur(24*time.Hour)) {
		t.Errorf("AgeoutLeft while pending = %v, want 24h", fmtDur(info.AgeoutLeft))
	}
	r.advance(t, time.Minute)
	r.waitState(t, 13, stateActive)
	if info, _ := r.m.Info(13); info.AgeoutLeft != nil {
		t.Errorf("AgeoutLeft after activation = %v, want nil", fmtDur(info.AgeoutLeft))
	}
	r.advance(t, time.Hour)
	r.waitState(t, 13, stateInactive)
	r.advance(t, 23*time.Hour) // the next start comes before the ageout
	r.waitState(t, 13, stateActive)
	if info, _ := r.m.Info(13); info.AgeoutLeft != nil {
		t.Errorf("AgeoutLeft after the second activation = %v, want nil", fmtDur(info.AgeoutLeft))
	}
}

func TestRestart(t *testing.T) {
	c := scheduled(14, &config.Schedule{Start: config.StartAt, At: t0, Life: time.Minute})
	pending := scheduled(15, &config.Schedule{Start: config.StartPending, Forever: true})
	r := newRig(t, c, pending)
	r.waitInitiations(t, 14, 1)
	r.steps(t, 14, 20)

	if err := r.m.Restart(14); err != nil {
		t.Fatal(err)
	}
	// A new life starts now with a first attempt at once and Seq 1.
	r.waitInitiations(t, 14, 1)
	s := r.snap(t, 14)
	if s.LifeIndex != 2 || !s.LifeStart.Equal(t0.Add(20*time.Second)) {
		t.Errorf("after restart: life %d starting %v, want life 2 at %v", s.LifeIndex, s.LifeStart, t0.Add(20*time.Second))
	}
	if got := r.rec.get(); got[len(got)-1].Seq != 1 {
		t.Errorf("first attempt after restart has Seq %d, want 1", got[len(got)-1].Seq)
	}
	if info, _ := r.m.Info(14); !equalDur(info.LifeLeft, dur(time.Minute)) {
		t.Errorf("LifeLeft after restart = %v, want the full life (1m)", fmtDur(info.LifeLeft))
	}

	if err := r.m.Restart(15); !errors.Is(err, ErrNotActive) {
		t.Errorf("Restart(pending) = %v, want ErrNotActive", err)
	}
	if err := r.m.Restart(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("Restart(99) = %v, want ErrNotFound", err)
	}
}

func TestReset(t *testing.T) {
	a := scheduled(16, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	after := scheduled(17, &config.Schedule{Start: config.StartAfter, After: 5 * time.Second, Forever: true})
	aged := scheduled(18, &config.Schedule{Start: config.StartPending, Forever: true, Ageout: 3 * time.Second})
	r := newRig(t, a, after, aged)
	r.waitInitiations(t, 16, 1)
	r.steps(t, 16, 6)
	r.waitState(t, 17, stateActive)
	waitFor(t, "ageout of 18", func() bool { _, ok := r.m.State(18); return !ok })

	if err := r.m.Reset(); err != nil {
		t.Fatal(err)
	}
	// Every operation is back in its initial transition with a new life;
	// the aged-out one is back too.
	now := t0.Add(6 * time.Second)
	for id, want := range map[int]string{16: stateActive, 17: statePending, 18: statePending} {
		if got, ok := r.m.State(id); !ok || got != want {
			t.Errorf("State(%d) after reset = %q, %v; want %q", id, got, ok, want)
		}
	}
	if s := r.snap(t, 17); s.LifeIndex != 2 || s.Totals.Initiations != 0 || !s.LifeStart.Equal(now.Add(5*time.Second)) {
		t.Errorf("operation 17 after reset: %+v", s)
	}
	if s := r.snap(t, 18); s.LifeIndex != 1 {
		t.Errorf("operation 18 (re-created) life index = %d, want 1", s.LifeIndex)
	}
	r.waitInitiations(t, 16, 1) // its start time (t0) has passed: it restarts at once
	if s := r.snap(t, 16); s.LifeIndex != 2 {
		t.Errorf("operation 16 life index after reset = %d, want 2", s.LifeIndex)
	}
}

func TestControlBeforeRun(t *testing.T) {
	m := New(&config.Config{Operations: []*config.Operation{echoOp(1, "192.0.2.1")}}, &fakeEngine{}, clock.NewFake(t0), nil, discardLogger())
	if err := m.Restart(1); !errors.Is(err, errNotRunning) {
		t.Errorf("Restart before Run = %v, want errNotRunning", err)
	}
	if err := m.Reset(); !errors.Is(err, errNotRunning) {
		t.Errorf("Reset before Run = %v, want errNotRunning", err)
	}
	if _, err := m.Reload(&config.Config{}); !errors.Is(err, errNotRunning) {
		t.Errorf("Reload before Run = %v, want errNotRunning", err)
	}
}

func TestReloadClassifies(t *testing.T) {
	keep := scheduled(1, nil)
	measure := scheduled(2, nil)
	tag := scheduled(3, nil)
	gone := scheduled(4, nil)
	oldCfg := &config.Config{Operations: []*config.Operation{keep, measure, tag, gone}}
	oldCfg.Global.APISocket = "/run/a.sock"
	r := newRigConfig(t, oldCfg)
	for _, id := range []int{1, 2, 3, 4} {
		r.waitState(t, id, stateActive)
	}
	for i := 1; i <= 3; i++ {
		r.advance(t, time.Second)
		for _, id := range []int{1, 2, 3} {
			r.waitInitiations(t, id, uint64(i))
		}
	}

	keep2 := *keep
	measure2 := *measure
	measure2.Threshold = 50 * time.Millisecond
	tag2 := *tag
	tag2.Tag = "renamed"
	tag2.React = []config.Reaction{{Element: "rtt", ThresholdType: "immediate", Upper: 5, Lower: 3}}
	added := scheduled(5, nil)
	newCfg := &config.Config{Operations: []*config.Operation{&keep2, &measure2, &tag2, added}}
	newCfg.Global.APISocket = "/run/b.sock"

	res, err := r.m.Reload(newCfg)
	if err != nil {
		t.Fatal(err)
	}
	want := ReloadResult{Added: []int{5}, Removed: []int{4}, Restarted: []int{2}, Updated: []int{3}}
	if !equalInts(res.Added, want.Added) || !equalInts(res.Removed, want.Removed) ||
		!equalInts(res.Restarted, want.Restarted) || !equalInts(res.Updated, want.Updated) {
		t.Errorf("Reload = %+v, want %+v", res, want)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != "global.api-socket changed; it requires a restart of goipslad to take effect" {
		t.Errorf("warnings = %q", res.Warnings)
	}

	// Restarted: new life, statistics from zero. Updated and unchanged:
	// statistics kept. Removed: row gone.
	if s := r.snap(t, 2); s.LifeIndex != 2 || s.Totals.Initiations > 1 {
		t.Errorf("restarted operation: life %d, %d attempts", s.LifeIndex, s.Totals.Initiations)
	}
	for _, id := range []int{1, 3} {
		if s := r.snap(t, id); s.LifeIndex != 1 || s.Totals.Initiations < 3 {
			t.Errorf("operation %d lost its statistics: life %d, %d attempts", id, s.LifeIndex, s.Totals.Initiations)
		}
	}
	if _, ok := r.store.Snapshot(4, stats.SnapshotOptions{}); ok {
		t.Error("removed operation still has statistics")
	}
	if c, _ := r.m.Config(3); c.Tag != "renamed" {
		t.Errorf("Config(3).Tag = %q, want renamed", c.Tag)
	}
	if s := r.snap(t, 3); s.Tag != "renamed" {
		t.Errorf("statistics row of 3 has tag %q, want renamed (Store.SetConfig)", s.Tag)
	}
	if c, _ := r.m.Config(2); c.Threshold != 50*time.Millisecond {
		t.Errorf("Config(2).Threshold = %v, want 50ms", c.Threshold)
	}
	if s, _ := r.m.State(5); s != stateActive {
		t.Errorf("added operation state = %q, want active", s)
	}

	// Measurement continues for every remaining operation.
	for _, id := range []int{1, 3} {
		r.waitInitiations(t, id, r.snap(t, id).Totals.Initiations)
	}
	before1, before3 := r.snap(t, 1).Totals.Initiations, r.snap(t, 3).Totals.Initiations
	r.advance(t, time.Second)
	r.waitInitiations(t, 1, before1+1)
	r.waitInitiations(t, 3, before3+1)

	// Reloading the same configuration changes no operation, but the
	// api-socket of the file still differs from the one in force since
	// startup: the warning stays (audit, Codex reload.go:102).
	res, err = r.m.Reload(newCfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added)+len(res.Removed)+len(res.Restarted)+len(res.Updated) != 0 {
		t.Errorf("reload of the same configuration = %+v, want no change", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "global.api-socket changed") {
		t.Errorf("warnings of the second reload = %q, want the api-socket warning again", res.Warnings)
	}

	// Back to the api-socket in force: no warning.
	back := *newCfg
	back.Global.APISocket = oldCfg.Global.APISocket
	res, err = r.m.Reload(&back)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings after going back to the startup value = %q, want none", res.Warnings)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReloadReaddsAgedOut: an operation deleted by ageout is re-created by
// the next reload because it is still in the file.
func TestReloadReaddsAgedOut(t *testing.T) {
	c := scheduled(20, &config.Schedule{Start: config.StartPending, Forever: true, Ageout: time.Second})
	cfg := &config.Config{Operations: []*config.Operation{c}}
	r := newRigConfig(t, cfg)
	r.advance(t, time.Second)
	waitFor(t, "ageout", func() bool { _, ok := r.m.State(20); return !ok })
	res, err := r.m.Reload(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(res.Added, []int{20}) {
		t.Errorf("Added = %v, want [20]", res.Added)
	}
	if s, ok := r.m.State(20); !ok || s != statePending {
		t.Errorf("State(20) = %q, %v; want pending", s, ok)
	}
}

func TestMeasurementDiff(t *testing.T) {
	base := echoOp(1, "192.0.2.1")
	base.Schedule = &config.Schedule{Forever: true, Start: config.StartNow}
	mods := map[string]func(c *config.Operation){
		"type":              func(c *config.Operation) { c.Type = config.ICMPJitter },
		"target":            func(c *config.Operation) { c.Target = c.Target.Next() },
		"frequency":         func(c *config.Operation) { c.Frequency++ },
		"timeout":           func(c *config.Operation) { c.Timeout++ },
		"threshold":         func(c *config.Operation) { c.Threshold++ },
		"request-data-size": func(c *config.Operation) { c.RequestDataSize++ },
		"verify-data":       func(c *config.Operation) { c.VerifyData = true },
		"one-way-delay":     func(c *config.Operation) { c.OneWayDelay = true },
		"vrf":               func(c *config.Operation) { c.VRF = "blue" },
		"statistics":        func(c *config.Operation) { c.Stats.HoursKept = 5 },
		"history":           func(c *config.Operation) { c.History.Lives = 2 },
		"history.enhanced":  func(c *config.Operation) { c.Enhanced = &config.EnhancedHistory{Interval: time.Minute, Buckets: 3} },
		"schedule":          func(c *config.Operation) { c.Schedule = &config.Schedule{Life: time.Hour, Start: config.StartNow} },
	}
	for name, mod := range mods {
		c := *base
		mod(&c)
		if d := measurementDiff(base, &c, t0); len(d) != 1 || d[0] != name {
			t.Errorf("%s: measurementDiff = %v", name, d)
		}
		if d := notificationDiff(base, &c); len(d) != 0 {
			t.Errorf("%s: notificationDiff = %v", name, d)
		}
	}
	c := *base
	c.Tag, c.Owner = "x", "y"
	if d := measurementDiff(base, &c, t0); len(d) != 0 {
		t.Errorf("tag/owner: measurementDiff = %v", d)
	}
	if d := notificationDiff(base, &c); len(d) != 2 {
		t.Errorf("tag/owner: notificationDiff = %v", d)
	}
}

func TestSameSchedule(t *testing.T) {
	loc := time.FixedZone("JST", 9*3600)
	now := time.Date(2026, 9, 27, 15, 0, 0, 0, loc)
	daily := func(at time.Time) *config.Schedule {
		return &config.Schedule{Start: config.StartAt, At: at, Forever: true}
	}
	todayPassed := time.Date(2026, 9, 27, 13, 30, 0, 0, loc)
	tomorrow := todayPassed.Add(24 * time.Hour)
	tests := []struct {
		name string
		a, b *config.Schedule
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil vs now", nil, &config.Schedule{Start: config.StartNow, Forever: true}, false},
		{"after same", &config.Schedule{Start: config.StartAfter, After: time.Minute, Forever: true},
			&config.Schedule{Start: config.StartAfter, After: time.Minute, Forever: true}, true},
		{"after differs", &config.Schedule{Start: config.StartAfter, After: time.Minute, Forever: true},
			&config.Schedule{Start: config.StartAfter, After: time.Hour, Forever: true}, false},
		{"HH:MM re-resolved to tomorrow", daily(todayPassed), daily(tomorrow), true},
		{"HH:MM changed", daily(todayPassed), daily(tomorrow.Add(time.Minute)), false},
		{"future at moved by a day", daily(tomorrow), daily(tomorrow.Add(24 * time.Hour)), false},
		{"life differs", &config.Schedule{Start: config.StartNow, Life: time.Hour},
			&config.Schedule{Start: config.StartNow, Life: 2 * time.Hour}, false},
		{"forever ignores life", &config.Schedule{Start: config.StartNow, Forever: true, Life: time.Hour},
			&config.Schedule{Start: config.StartNow, Forever: true}, true},
	}
	for _, tt := range tests {
		if got := sameSchedule(tt.a, tt.b, now); got != tt.want {
			t.Errorf("%s: sameSchedule = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestConcurrentControl runs reloads, restarts and resets at the same time
// as attempts, for the race detector.
func TestConcurrentControl(t *testing.T) {
	var ops []*config.Operation
	for id := 1; id <= 20; id++ {
		ops = append(ops, scheduled(id, nil))
	}
	cfg := &config.Config{Operations: ops}
	r := newRigConfig(t, cfg)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				switch (g + i) % 4 {
				case 0:
					if _, err := r.m.Reload(cfg); err != nil {
						t.Error(err)
					}
				case 1:
					_ = r.m.Restart(1 + i%20)
				case 2:
					if err := r.m.Reset(); err != nil {
						t.Error(err)
					}
				default:
					r.m.Counts()
					r.m.Info(1 + i%20)
					r.m.LateReply(uint32(1+i%20), 1, r.clk.Now())
				}
			}
		}(g)
	}
	for i := 0; i < 20; i++ {
		r.clk.Advance(100 * time.Millisecond)
	}
	wg.Wait()
}

// TestJitterRegistered checks that icmp-jitter operations run through
// op.NewJitter (the fake engine answers Jitter with an error, which the
// runner reports as a result like any other).
func TestJitterRegistered(t *testing.T) {
	c := scheduled(31, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	c.Type = config.ICMPJitter
	c.Interval = 20 * time.Millisecond
	c.NumPackets = 10
	r := newRig(t, c)
	r.waitState(t, 31, stateActive)
	res := r.rec.waitLen(t, 1)
	if res[0].OpID != 31 || res[0].Type != config.ICMPJitter || res[0].Seq != 1 {
		t.Errorf("jitter result = %+v", res[0])
	}
	if n := r.m.Counts(); n.Skipped != 0 || n.Active != 1 {
		t.Errorf("Counts() = %+v, want the jitter operation active", n)
	}
}

// TestStartBeforeRun: Start registers every operation and its statistics row
// before any scheduler runs, so the API can be published right after it
// (review P2 S2).
func TestStartBeforeRun(t *testing.T) {
	now := echoOp(1, "192.0.2.1")
	pending := scheduled(2, &config.Schedule{Start: config.StartPending, Forever: true})
	clk := clock.NewFake(t0)
	store := stats.NewStore(clk)
	m := New(&config.Config{Operations: []*config.Operation{now, pending}}, &fakeEngine{}, clk, store.Record, discardLogger())
	m.SetStore(store)
	m.Start()
	if rows := store.Summary(); len(rows) != 2 {
		t.Fatalf("store has %d rows after Start, want 2", len(rows))
	}
	if n := m.Counts(); n.Active != 1 || n.Pending != 1 {
		t.Errorf("Counts() after Start = %+v", n)
	}
	m.mu.RLock()
	running := m.running
	m.mu.RUnlock()
	if !running {
		t.Error("not running after Start")
	}
	m.Start() // idempotent: no second life
	if s, _ := store.Snapshot(1, stats.SnapshotOptions{}); s.LifeIndex != 1 {
		t.Errorf("life index after a second Start = %d, want 1", s.LifeIndex)
	}
	// Run keeps the state Start built.
	stop := start(t, m)
	defer stop()
	if s, _ := store.Snapshot(1, stats.SnapshotOptions{}); s.LifeIndex != 1 {
		t.Errorf("life index after Run = %d, want 1", s.LifeIndex)
	}
}

// TestLateReplyAfterRestart: a late reply to an attempt of the life before a
// restart is not counted in the new life (review P3/P4/P6 B4).
func TestLateReplyAfterRestart(t *testing.T) {
	c := scheduled(40, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	r := newRig(t, c)
	r.waitInitiations(t, 40, 1)
	r.steps(t, 40, 2) // attempts 1..3 of life 1
	if err := r.m.Restart(40); err != nil {
		t.Fatal(err)
	}
	r.waitInitiations(t, 40, 1) // attempt 1 of life 2
	base := len(r.rec.get())
	r.m.LateReply(40, 3, r.clk.Now()) // attempt 3 of life 1: not in life 2
	if got := r.rec.get(); len(got) != base {
		t.Errorf("late reply to the previous life reached the sink: %+v", got[base:])
	}
	if s := r.snap(t, 40); s.Totals.SequenceErrors != 0 {
		t.Errorf("new life counts %d sequence errors, want 0", s.Totals.SequenceErrors)
	}
	r.m.LateReply(40, 1, r.clk.Now()) // attempt 1 of life 2: counted
	waitFor(t, "sequence error", func() bool { return r.snap(t, 40).Totals.SequenceErrors == 1 })
}

// TestLateReplyRestartBeforeCallback: probe detects a late reply to attempt
// 3 of life 1, and before its OnLateReply callback runs, a restart starts
// life 2 whose own attempt 3 is already in the table (review R2). The send
// time of the answered request tells the two attempt 3 apart: the reply is
// dropped, and life 2 counts no sequence error.
func TestLateReplyRestartBeforeCallback(t *testing.T) {
	c := scheduled(42, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	r := newRig(t, c)
	r.waitInitiations(t, 42, 1)
	r.steps(t, 42, 2) // attempts 1..3 of life 1
	old3 := resultOf(t, r.rec.get(), 1, 3)
	r.advance(t, 500*time.Millisecond) // the restart comes between two lattice points
	if err := r.m.Restart(42); err != nil {
		t.Fatal(err)
	}
	r.waitInitiations(t, 42, 1)
	r.steps(t, 42, 2) // attempts 1..3 of life 2
	new3 := resultOf(t, r.rec.get(), 2, 3)
	base := len(r.rec.get())

	r.m.LateReply(42, 3, old3.Start) // the callback of the reply found before the restart
	if got := r.rec.get(); len(got) != base {
		t.Errorf("late reply to attempt 3 of life 1 reached the sink as life 2's: %+v", got[base:])
	}
	if s := r.snap(t, 42); s.Totals.SequenceErrors != 0 {
		t.Errorf("life 2 counts %d sequence errors, want 0", s.Totals.SequenceErrors)
	}
	// Sent before life 2 started, though after nothing in the table: dropped.
	r.m.LateReply(42, 1, new3.Start.Add(-time.Hour))
	// A late reply to attempt 3 of life 2 itself is counted, in life 2.
	r.m.LateReply(42, 3, new3.Start)
	got := r.rec.waitLen(t, base+1)
	if res := got[base]; res.Code != op.RCSequenceError || res.Seq != 3 || res.Life != 2 || !res.Start.Equal(new3.Start) {
		t.Errorf("late reply to attempt 3 of life 2 = %+v, want sequenceError seq 3 life 2 start %v", res, new3.Start)
	}
	waitFor(t, "sequence error", func() bool { return r.snap(t, 42).Totals.SequenceErrors == 1 })
	if n := len(r.rec.get()); n != base+1 {
		t.Errorf("sink got %d results after the late replies, want %d", n, base+1)
	}
}

// TestLateReplyAfterLifeEnd: a late reply that arrives after a finite life
// ended is dropped, so the frozen statistics of the inactive operation do
// not change (review audit-fix2). The same reply during the life counts.
func TestLateReplyAfterLifeEnd(t *testing.T) {
	c := scheduled(44, &config.Schedule{Start: config.StartAt, At: t0, Life: 3 * time.Second})
	r := newRig(t, c)
	r.waitInitiations(t, 44, 1)
	r.steps(t, 44, 2) // attempts 1..3; the life ends at t0+3s
	first := resultOf(t, r.rec.get(), 1, 1)
	last := resultOf(t, r.rec.get(), 1, 3)
	r.m.LateReply(44, 1, first.Start) // during the life: counted
	waitFor(t, "sequence error", func() bool { return r.snap(t, 44).Totals.SequenceErrors == 1 })

	r.advance(t, time.Second)
	r.waitState(t, 44, stateInactive)
	base := len(r.rec.get())
	frozen := r.snap(t, 44).Totals
	r.m.LateReply(44, 3, last.Start) // the last attempt, answered after the end
	r.m.LateReply(44, 1, first.Start)
	if got := r.rec.get(); len(got) != base {
		t.Errorf("late reply after the life ended reached the sink: %+v", got[base:])
	}
	if got := r.snap(t, 44).Totals; got != frozen {
		t.Errorf("frozen totals changed after the life ended: %+v, want %+v", got, frozen)
	}
}

// TestResultCarriesLife: results carry the index of their life as the
// statistics count it (R3): 1, then 2 after a restart, then 3 after a reload
// that rebuilds the operation, and 1 again after removal and re-creation.
func TestResultCarriesLife(t *testing.T) {
	c := scheduled(43, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	r := newRig(t, c)
	lastLife := func() int {
		got := r.rec.get()
		return got[len(got)-1].Life
	}
	r.waitInitiations(t, 43, 1)
	if l := lastLife(); l != 1 {
		t.Errorf("life of the first result = %d, want 1", l)
	}
	if err := r.m.Restart(43); err != nil {
		t.Fatal(err)
	}
	r.waitInitiations(t, 43, 1)
	if l, want := lastLife(), r.snap(t, 43).LifeIndex; l != 2 || l != want {
		t.Errorf("life after restart = %d, want 2 (store: %d)", l, want)
	}
	c2 := *c
	c2.Timeout = c.Timeout + time.Millisecond // a measurement change rebuilds the operation
	if _, err := r.m.Reload(&config.Config{Operations: []*config.Operation{&c2}}); err != nil {
		t.Fatal(err)
	}
	r.steps(t, 43, 1)
	if l, want := lastLife(), r.snap(t, 43).LifeIndex; l != 3 || l != want {
		t.Errorf("life after a rebuilding reload = %d, want 3 (store: %d)", l, want)
	}
	if _, err := r.m.Reload(&config.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Reload(&config.Config{Operations: []*config.Operation{&c2}}); err != nil {
		t.Fatal(err)
	}
	r.steps(t, 43, 1)
	if l, want := lastLife(), r.snap(t, 43).LifeIndex; l != 1 || l != want {
		t.Errorf("life after removal and re-creation = %d, want 1 (store: %d)", l, want)
	}
}

// resultOf returns the result of attempt seq of life life.
func resultOf(t *testing.T, got []op.Result, life int, seq uint32) op.Result {
	t.Helper()
	for _, r := range got {
		if r.Life == life && r.Seq == seq && r.Code != op.RCSequenceError {
			return r
		}
	}
	t.Fatalf("no result of attempt %d of life %d in %+v", seq, life, got)
	return op.Result{}
}

// TestReloadOneWayDelay: changing only one-way-delay rebuilds the operation
// with a new statistics life (review P3/P4/P6 B1).
func TestReloadOneWayDelay(t *testing.T) {
	c := scheduled(41, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	c.Type = config.ICMPJitter
	c.Interval, c.NumPackets = 20*time.Millisecond, 10
	r := newRigConfig(t, &config.Config{Operations: []*config.Operation{c}})
	r.waitInitiations(t, 41, 1)
	c2 := *c
	c2.OneWayDelay = true
	res, err := r.m.Reload(&config.Config{Operations: []*config.Operation{&c2}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(res.Restarted, []int{41}) {
		t.Errorf("Restarted = %v, want [41]", res.Restarted)
	}
	if s := r.snap(t, 41); s.LifeIndex != 2 {
		t.Errorf("life index after the reload = %d, want 2", s.LifeIndex)
	}
	if cfg, _ := r.m.Config(41); !cfg.OneWayDelay {
		t.Error("the rebuilt operation does not use one-way-delay")
	}
}

// reactRig is a rig whose sink feeds the reaction engine and the tracker,
// as goipslad does, with an engine whose replies the test controls.
type reactRig struct {
	*rig
	eng     *fakeEngine
	engine  *react.Engine
	tracker *react.Tracker
	bus     *event.Bus
}

func newReactRig(t *testing.T, cfg *config.Config) *reactRig {
	t.Helper()
	clk := clock.NewFake(t0)
	store := stats.NewStore(clk)
	bus := event.NewBus(discardLogger())
	engine := react.NewEngine(clk, bus, discardLogger())
	tracker := react.NewTracker(clk, bus, discardLogger())
	rec := &sinkRecorder{}
	sink := func(r op.Result) {
		store.Record(r)
		engine.Observe(r)
		tracker.Observe(r)
		rec.sink(r)
	}
	var mu sync.Mutex
	timeout := false
	eng := &fakeEngine{reply: func(probe.Request) probe.Reply {
		mu.Lock()
		defer mu.Unlock()
		if timeout {
			return probe.Reply{Outcome: probe.OutcomeTimeout}
		}
		return probe.Reply{Outcome: probe.OutcomeReply, RTT: time.Millisecond}
	}}
	m := New(cfg, eng, clk, sink, discardLogger())
	m.SetStore(store)
	m.SetReactions(engine, tracker)
	r := &reactRig{rig: &rig{m: m, clk: clk, store: store, rec: rec}, eng: eng, engine: engine, tracker: tracker, bus: bus}
	setTimeout = func(v bool) { mu.Lock(); timeout = v; mu.Unlock() }
	r.stop = start(t, m)
	t.Cleanup(r.stop)
	return r
}

// setTimeout switches the reactRig engine between replies and timeouts.
var setTimeout func(bool)

func occurred(e *react.Engine, id int, element string) bool {
	for _, s := range e.Snapshot(id) {
		if s.Element == element {
			return s.Occurred
		}
	}
	return false
}

func TestReactionsFollowLifecycle(t *testing.T) {
	timeoutReact := []config.Reaction{{Element: "timeout", ThresholdType: "immediate", Action: "none"}}
	a := scheduled(50, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	a.React = timeoutReact
	b := scheduled(51, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	b.React = timeoutReact
	cfg := &config.Config{
		Operations: []*config.Operation{a, b},
		Tracks:     []config.Track{{ID: 1, Operation: 50, Mode: "state"}, {ID: 2, Operation: 51, Mode: "reachability"}},
	}
	r := newReactRig(t, cfg)
	r.waitInitiations(t, 50, 1)
	r.waitInitiations(t, 51, 1)
	if n := len(r.engine.Snapshot(50)); n != 1 {
		t.Fatalf("reactions of 50 after Start: %d rows, want 1", n)
	}
	waitFor(t, "tracks up", func() bool {
		s1, _ := r.tracker.Snapshot(1)
		s2, _ := r.tracker.Snapshot(2)
		return s1.State == "up" && s2.State == "up"
	})

	// Time out: occurred goes true for both.
	setTimeout(true)
	r.steps(t, 50, 1)
	waitFor(t, "occurred", func() bool { return occurred(r.engine, 50, "timeout") && occurred(r.engine, 51, "timeout") })

	// restart: a new life resets occurred without a notification.
	setTimeout(false)
	before := thresholdEvents(r.bus)
	if err := r.m.Restart(50); err != nil {
		t.Fatal(err)
	}
	if occurred(r.engine, 50, "timeout") {
		t.Error("occurred still true after restart")
	}

	// A reload that changes only the tag of 51 keeps its reaction state; one
	// that changes its reactions resets it. Neither notifies.
	b2 := *b
	b2.Tag = "renamed"
	if _, err := r.m.Reload(&config.Config{Operations: []*config.Operation{a, &b2}, Tracks: cfg.Tracks}); err != nil {
		t.Fatal(err)
	}
	if !occurred(r.engine, 51, "timeout") {
		t.Error("a tag-only reload reset the reaction state")
	}
	b3 := b2
	b3.React = []config.Reaction{{Element: "timeout", ThresholdType: "consecutive", Count: 2, Action: "none"}}
	res, err := r.m.Reload(&config.Config{Operations: []*config.Operation{a, &b3}, Tracks: cfg.Tracks})
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(res.Updated, []int{51}) || occurred(r.engine, 51, "timeout") {
		t.Errorf("reaction reload: %+v, occurred %v; want updated [51] and occurred false", res, occurred(r.engine, 51, "timeout"))
	}
	if got := thresholdEvents(r.bus); got != before {
		t.Errorf("restart and reloads published %d threshold events, want none", got-before)
	}

	// Removing 51 removes its reactions and sends its track back to unknown.
	if _, err := r.m.Reload(&config.Config{Operations: []*config.Operation{a}, Tracks: cfg.Tracks}); err != nil {
		t.Fatal(err)
	}
	if rows := r.engine.Snapshot(51); rows != nil {
		t.Errorf("reactions of removed 51: %+v", rows)
	}
	if s, _ := r.tracker.Snapshot(2); s.State != "unknown" {
		t.Errorf("track 2 of the removed operation is %q, want unknown", s.State)
	}
	if s, _ := r.tracker.Snapshot(1); s.State == "unknown" {
		t.Error("track 1 lost its state on reload")
	}
}

func thresholdEvents(b *event.Bus) int {
	n := 0
	for _, ev := range b.Recent(0) {
		if ev.Kind.IsThreshold() {
			n++
		}
	}
	return n
}

// TestReloadTagKeepsReactionState: a reload that changes only the tag keeps
// the reaction state and makes later events carry the new tag (SetTag).
func TestReloadTagKeepsReactionState(t *testing.T) {
	c := scheduled(60, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	c.React = []config.Reaction{{Element: "timeout", ThresholdType: "immediate", Action: "none"}}
	cfg := &config.Config{Operations: []*config.Operation{c}}
	r := newReactRig(t, cfg)
	r.waitInitiations(t, 60, 1)

	setTimeout(true)
	r.steps(t, 60, 1)
	waitFor(t, "occurred", func() bool { return occurred(r.engine, 60, "timeout") })

	c2 := *c
	c2.Tag = "renamed"
	res, err := r.m.Reload(&config.Config{Operations: []*config.Operation{&c2}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(res.Updated, []int{60}) || !occurred(r.engine, 60, "timeout") {
		t.Fatalf("tag reload: %+v, occurred %v; want updated [60] with occurred kept", res, occurred(r.engine, 60, "timeout"))
	}
	setTimeout(false)
	r.steps(t, 60, 1)
	waitFor(t, "cleared", func() bool { return !occurred(r.engine, 60, "timeout") })
	evs := r.bus.Recent(0)
	last := evs[len(evs)-1]
	if last.Kind != event.ThresholdCleared || last.Tag != "renamed" {
		t.Errorf("event after the tag reload = %+v, want threshold-cleared with tag renamed", last)
	}
}

func TestGlobalDiffAPISocketGroup(t *testing.T) {
	w := globalDiff(config.Global{APISocketGroup: "ipsla"}, config.Global{APISocketGroup: "wheel"})
	if len(w) != 1 || !strings.Contains(w[0], "global.api-socket-group changed") {
		t.Errorf("globalDiff = %q", w)
	}
}

// staleRig holds the result of operation 71 inside the sink while an attempt
// of operation 70 finishes and waits to be delivered, so that a lifecycle
// change (restart, removal) can happen in between (audit C3 / C12).
type staleRig struct {
	m        *Manager
	clk      *clock.Fake
	engine   *react.Engine
	tracker  *react.Tracker
	bus      *event.Bus
	bIn      chan struct{} // closed when 71's result is inside the sink
	release  chan struct{} // closed to let the sink go on
	aDone    chan struct{} // closed when 70's first attempt has returned from the engine
	changed  atomic.Bool   // set once the lifecycle change has returned
	late     atomic.Int32  // results of 70's first attempt delivered after the change
	stop     func()
	engineMu sync.Mutex
	aSeen    int
}

func newStaleRig(t *testing.T, tracks []config.Track) *staleRig {
	t.Helper()
	a := scheduled(70, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	a.React = []config.Reaction{{Element: "timeout", ThresholdType: "immediate", Action: "none"}}
	b := scheduled(71, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	clk := clock.NewFake(t0)
	r := &staleRig{clk: clk, bIn: make(chan struct{}), release: make(chan struct{}), aDone: make(chan struct{})}
	r.bus = event.NewBus(discardLogger())
	r.engine = react.NewEngine(clk, r.bus, discardLogger())
	r.tracker = react.NewTracker(clk, r.bus, discardLogger())
	var bOnce sync.Once
	sink := func(res op.Result) {
		if res.OpID == 71 {
			first := false
			bOnce.Do(func() { first = true })
			if first {
				close(r.bIn)
				<-r.release
			}
		}
		// 70's first attempt timed out; the attempts of the new life reply.
		if res.OpID == 70 && res.Code == op.RCTimeout && r.changed.Load() {
			r.late.Add(1)
		}
		r.engine.Observe(res)
		r.tracker.Observe(res)
	}
	eng := &fakeEngine{reply: func(req probe.Request) probe.Reply {
		if req.OpID == 70 {
			r.engineMu.Lock()
			r.aSeen++
			first := r.aSeen == 1
			r.engineMu.Unlock()
			if first {
				<-r.bIn // 71 holds the sink by now
				defer close(r.aDone)
				return probe.Reply{Outcome: probe.OutcomeTimeout}
			}
		}
		return probe.Reply{Outcome: probe.OutcomeReply, RTT: time.Millisecond}
	}}
	m := New(&config.Config{Operations: []*config.Operation{a, b}, Tracks: tracks}, eng, clk, sink, discardLogger())
	m.SetReactions(r.engine, r.tracker)
	r.m = m
	r.stop = start(t, m)
	t.Cleanup(r.stop)
	return r
}

// change runs f (a lifecycle change) while 70's first result is waiting to
// be delivered, then lets the sink go on and waits for everything to settle.
func (r *staleRig) change(t *testing.T, f func()) {
	t.Helper()
	waitChan(t, "71 in the sink", r.bIn)
	waitChan(t, "70's attempt returned", r.aDone)
	time.Sleep(20 * time.Millisecond) // let 70's attempt reach the sink's lock
	done := make(chan struct{})
	go func() {
		f()
		r.changed.Store(true)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond) // a fixed manager waits here for the sink
	close(r.release)
	waitChan(t, "the lifecycle change", done)
	waitFor(t, "manager to settle", func() bool { return quiet(r.m) })
}

func waitChan(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestRestartDropsResultOfPreviousLife: a result of the life before a
// restart that was waiting for the sink must not reach it after the restart
// (it would count in the new life's reactions and tracks).
func TestRestartDropsResultOfPreviousLife(t *testing.T) {
	r := newStaleRig(t, nil)
	r.change(t, func() {
		if err := r.m.Restart(70); err != nil {
			t.Error(err)
		}
	})
	if n := r.late.Load(); n != 0 {
		t.Errorf("%d result(s) of the previous life reached the sink after the restart", n)
	}
	for _, ev := range r.bus.Recent(0) {
		if ev.OpID == 70 && ev.Kind == event.ThresholdCleared {
			t.Errorf("the new life cleared a threshold it never exceeded: %+v", ev)
		}
	}
}

// TestRemovalDropsPendingResult: a result of an operation that a reload
// removed must not reach the sink; the tracker would otherwise publish a
// track event with an empty type and target (audit C12).
func TestRemovalDropsPendingResult(t *testing.T) {
	tracks := []config.Track{{ID: 1, Operation: 70, Mode: "state"}}
	r := newStaleRig(t, tracks)
	b := scheduled(71, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	r.change(t, func() {
		if _, err := r.m.Reload(&config.Config{Operations: []*config.Operation{b}, Tracks: tracks}); err != nil {
			t.Error(err)
		}
	})
	if n := r.late.Load(); n != 0 {
		t.Errorf("%d result(s) of the removed operation reached the sink after the reload", n)
	}
	for _, ev := range r.bus.Recent(0) {
		if ev.Kind.IsTrack() && (ev.Type == "" || ev.Target == "") {
			t.Errorf("track event without type / target: %+v", ev)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestTransitionLogsNameTheOperation: state transitions are logged with the
// operation's ID, type and target, and pending operations are logged one by
// one (audit C7).
func TestTransitionLogsNameTheOperation(t *testing.T) {
	now := scheduled(80, &config.Schedule{Start: config.StartAt, At: t0, Forever: true})
	later := scheduled(81, &config.Schedule{Start: config.StartAt, At: t0.Add(time.Hour), Forever: true})
	pending := scheduled(82, &config.Schedule{Start: config.StartPending, Forever: true})
	var logs syncBuffer
	m := New(&config.Config{Operations: []*config.Operation{now, later, pending}}, &fakeEngine{}, clock.NewFake(t0), nil,
		slog.New(slog.NewTextHandler(&logs, nil)))
	stop := start(t, m)
	defer stop()
	if err := m.Restart(80); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{
		`msg="operation pending until its start time" op=81 type=icmp-echo target=192.0.2.1 start=`,
		`msg="operation pending (start-time pending); a reload that changes start-time starts it" op=82 type=icmp-echo target=192.0.2.1`,
		`msg="operation restarted" op=80 type=icmp-echo target=192.0.2.1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// operationFields classifies every field of config.Operation for reload
// (audit C8). A field added to config.Operation fails
// TestReloadClassifiesEveryField until it is placed here, and in
// measurementDiff or notificationDiff (the omission of OneWayDelay, review
// P3 B1, is the kind of mistake this catches).
var operationFields = map[string]string{
	// measurement: a change rebuilds the operation (measurementDiff)
	"Type": "measurement", "Target": "measurement", "SourceIP": "measurement", "SourceInterface": "measurement",
	"VRF": "measurement", "TOS": "measurement", "TrafficClass": "measurement", "FlowLabel": "measurement",
	"Frequency": "measurement", "Timeout": "measurement", "Threshold": "measurement",
	"RequestDataSize": "measurement", "DataPattern": "measurement", "VerifyData": "measurement",
	"Interval": "measurement", "NumPackets": "measurement", "OneWayDelay": "measurement",
	"Stats": "measurement", "History": "measurement", "Enhanced": "measurement", "Schedule": "measurement",
	// notification: a change keeps measuring (notificationDiff)
	"Tag": "notification", "Owner": "notification", "React": "notification",
	// not compared: the key, and how the file spelled the operation
	"ID": "none", "TargetName": "none", "Template": "none",
}

// mutate changes v to a different value of its type.
func mutate(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Interface().(type) {
	case netip.Addr:
		v.Set(reflect.ValueOf(netip.MustParseAddr("198.51.100.77")))
		return
	case time.Time:
		v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Hour)))
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint8, reflect.Uint32:
		v.SetUint(v.Uint() + 1)
	case reflect.String:
		v.SetString(v.String() + "x")
	case reflect.Struct:
		mutate(t, v.Field(0))
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		} else {
			v.Set(reflect.Zero(v.Type()))
		}
	case reflect.Slice:
		v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
	default:
		t.Fatalf("mutate: unhandled kind %s", v.Kind())
	}
}

func TestReloadClassifiesEveryField(t *testing.T) {
	base := scheduled(1, &config.Schedule{Start: config.StartNow, Forever: true})
	typ := reflect.TypeOf(*base)
	for i := range typ.NumField() {
		f := typ.Field(i)
		class, ok := operationFields[f.Name]
		if !ok {
			t.Errorf("config.Operation.%s is not classified for reload: add it to operationFields and to measurementDiff or notificationDiff", f.Name)
			continue
		}
		c := *base
		mutate(t, reflect.ValueOf(&c).Elem().Field(i))
		m, n := measurementDiff(base, &c, t0), notificationDiff(base, &c)
		switch class {
		case "measurement":
			if len(m) != 1 || len(n) != 0 {
				t.Errorf("%s: measurementDiff %v, notificationDiff %v; want it measured", f.Name, m, n)
			}
		case "notification":
			if len(m) != 0 || len(n) != 1 {
				t.Errorf("%s: measurementDiff %v, notificationDiff %v; want it notified", f.Name, m, n)
			}
		default:
			if len(m) != 0 || len(n) != 0 {
				t.Errorf("%s: measurementDiff %v, notificationDiff %v; want it ignored", f.Name, m, n)
			}
		}
	}
	for name := range operationFields {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("operationFields names %s, which config.Operation does not have", name)
		}
	}
}

// TestGlobalDiffSNMP: a change of global.snmp is not applied by a reload and
// is reported as needing a restart (audit C10).
func TestGlobalDiffSNMP(t *testing.T) {
	a := config.Global{SNMP: &config.SNMPConfig{AgentX: "tcp:127.0.0.1:705"}}
	for _, b := range []config.Global{
		{SNMP: &config.SNMPConfig{AgentX: "tcp:127.0.0.1:706"}},
		{SNMP: &config.SNMPConfig{AgentX: "tcp:127.0.0.1:705", Traps: []config.TrapTarget{{Host: "192.0.2.9"}}}},
		{},
	} {
		w := globalDiff(a, b)
		if len(w) != 1 || !strings.Contains(w[0], "global.snmp changed") {
			t.Errorf("globalDiff(%+v) = %q", b.SNMP, w)
		}
	}
	if w := globalDiff(a, a); len(w) != 0 {
		t.Errorf("no change: %q", w)
	}
}

// TestReloadConfigWarnings: the warnings of the new configuration join the
// reload result (audit C9 / B8).
func TestReloadConfigWarnings(t *testing.T) {
	c := scheduled(90, nil)
	r := newRigConfig(t, &config.Config{Operations: []*config.Operation{c}})
	next := &config.Config{Operations: []*config.Operation{c}, Warnings: []string{"templates.unused: template is not used"}}
	res, err := r.m.Reload(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != "templates.unused: template is not used" {
		t.Errorf("warnings = %q", res.Warnings)
	}
}

// TestForgetOnDeletion: the engine forgets an operation's Identifier when a
// reload removes it or it ages out, and not when it is only rebuilt (audit
// C9 / A7).
func TestForgetOnDeletion(t *testing.T) {
	keep := scheduled(100, nil)
	gone := scheduled(101, nil)
	aged := scheduled(102, &config.Schedule{Start: config.StartPending, Forever: true, Ageout: time.Second})
	eng := &fakeEngine{}
	clk := clock.NewFake(t0)
	m := New(&config.Config{Operations: []*config.Operation{keep, gone, aged}}, eng, clk, nil, discardLogger())
	stop := start(t, m)
	defer stop()

	rebuilt := *keep
	rebuilt.Threshold++
	if _, err := m.Reload(&config.Config{Operations: []*config.Operation{&rebuilt, aged}}); err != nil {
		t.Fatal(err)
	}
	advance(t, m, clk, time.Second)
	waitFor(t, "ageout", func() bool { _, ok := m.State(102); return !ok })
	eng.mu.Lock()
	got := append([]uint32(nil), eng.forgotten...)
	eng.mu.Unlock()
	if !slices.Equal(got, []uint32{101, 102}) {
		t.Errorf("Forget called for %v, want [101 102]", got)
	}
}

// TestReloadActionsComparedWithStartup: actions take effect at startup
// only, so every reload compares them with the ones in force, not with the
// previous file.
func TestReloadActionsComparedWithStartup(t *testing.T) {
	c := scheduled(110, nil)
	start := &config.Config{Operations: []*config.Operation{c}, Actions: []config.Action{{On: []string{"track-up"}, Exec: "/a"}}}
	r := newRigConfig(t, start)
	changed := &config.Config{Operations: []*config.Operation{c}, Actions: []config.Action{{On: []string{"track-up"}, Exec: "/b"}}}
	for i := range 2 {
		res, err := r.m.Reload(changed)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "actions changed") {
			t.Errorf("reload %d: warnings %q, want the actions warning", i+1, res.Warnings)
		}
	}
	res, err := r.m.Reload(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("back to the actions in force: warnings %q", res.Warnings)
	}
}
