//declscope:namespace track

package react

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/op"
)

func trackedOps(ids ...int) map[int]*config.Operation {
	m := map[int]*config.Operation{}
	for _, id := range ids {
		m[id] = &config.Operation{ID: id, Type: config.ICMPEcho, Target: netip.MustParseAddr("10.100.1.11"), Tag: "wan"}
	}
	return m
}

func trackResult(id int, c op.ReturnCode) op.Result {
	r := op.Result{OpID: id, Code: c}
	if c == op.RCOK || c == op.RCOverThreshold {
		r.RTT = 4 * time.Millisecond
	}
	return r
}

// trackEvents returns "track state" pairs of the published track events.
func trackEvents(b *event.Bus) []string {
	var out []string
	for _, e := range b.Recent(0) {
		out = append(out, e.Message)
	}
	return out
}

func TestTrackModes(t *testing.T) {
	bus := event.NewBus(quiet())
	tr := NewTracker(clock.NewFake(base), bus, quiet())
	tr.Configure([]config.Track{
		{ID: 1, Operation: 101, Mode: config.TrackState},
		{ID: 2, Operation: 101, Mode: config.TrackReachability},
	}, trackedOps(101))
	if s, _ := tr.Snapshot(1); s.State != trackUnknown {
		t.Fatalf("initial state %q", s.State)
	}
	for _, c := range []op.ReturnCode{op.RCOverThreshold, op.RCOK, op.RCBusy, op.RCSequenceError, op.RCTimeout} {
		tr.Observe(trackResult(101, c))
	}
	want := []string{
		"%TRACK-6-STATE: 1 ip sla 101 state Unknown -> Down", // overThreshold: state is down
		"%TRACK-6-STATE: 2 ip sla 101 reachability Unknown -> Up",
		"%TRACK-6-STATE: 1 ip sla 101 state Down -> Up",
		"%TRACK-6-STATE: 1 ip sla 101 state Up -> Down", // busy / sequenceError ignored
		"%TRACK-6-STATE: 2 ip sla 101 reachability Up -> Down",
	}
	if diff := cmp.Diff(want, trackEvents(bus)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	evs := bus.Recent(0)
	e := evs[1]
	if e.Kind != event.TrackUp || e.TrackID != 2 || e.TrackMode != "reachability" || e.TrackState != "up" ||
		e.LatestRC != "overThreshold" || e.LatestRTTMs == nil || *e.LatestRTTMs != 4 || e.Target != "10.100.1.11" || e.Tag != "wan" {
		t.Errorf("event %+v", e)
	}
	if evs[4].LatestRTTMs != nil || evs[4].LatestRC != "timeout" {
		t.Errorf("down event %+v", evs[4])
	}
	s, _ := tr.Snapshot(1)
	if s.State != trackDown || s.Changes != 3 || s.LatestRC != op.RCTimeout || s.Mode != "state" {
		t.Errorf("snapshot %+v", s)
	}
}

// waitTrackEvents waits until n events have been published.
func waitTrackEvents(t *testing.T, b *event.Bus, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.Stats().Published < uint64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("got %d events, want %d", b.Stats().Published, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitTrackPending waits until track id has the given pending state, so that the
// Run goroutine has seen the change before the fake clock moves.
func waitTrackPending(t *testing.T, tr *Tracker, id int, pending string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := tr.Snapshot(id); s.Pending == pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("track %d never pending %q", id, pending)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTrackDelays(t *testing.T) {
	fake := clock.NewFake(base)
	bus := event.NewBus(quiet())
	tr := NewTracker(fake, bus, quiet())
	tr.Configure([]config.Track{
		{ID: 1, Operation: 101, Mode: config.TrackState, DelayUp: 10 * time.Second, DelayDown: 5 * time.Second},
	}, trackedOps(101))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Run(ctx) }()
	defer func() { cancel(); <-done }()

	tr.Observe(trackResult(101, op.RCOK)) // unknown -> up at once, without the delay
	waitTrackEvents(t, bus, 1)

	// Down for 4 s, then back: no change.
	tr.Observe(trackResult(101, op.RCTimeout))
	waitTrackPending(t, tr, 1, trackDown)
	fake.Advance(4 * time.Second)
	tr.Observe(trackResult(101, op.RCTimeout)) // same judgement: the delay keeps running from the first
	tr.Observe(trackResult(101, op.RCOK))
	if s, _ := tr.Snapshot(1); s.Pending != "" || s.State != trackUp {
		t.Fatalf("after return: %+v", s)
	}
	fake.Advance(10 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if n := len(bus.Recent(0)); n != 1 {
		t.Fatalf("flap produced events: %v", trackEvents(bus))
	}

	// Down for the whole delay: one change, 5 s after the first timeout.
	tr.Observe(trackResult(101, op.RCTimeout))
	waitTrackPending(t, tr, 1, trackDown)
	fake.Advance(3 * time.Second)
	tr.Observe(trackResult(101, op.RCTimeout))
	fake.Advance(2 * time.Second)
	waitTrackEvents(t, bus, 2)
	s, _ := tr.Snapshot(1)
	if s.State != trackDown || s.Pending != "" || !s.LastChange.Equal(base.Add(19*time.Second)) {
		t.Errorf("after delay: %+v", s)
	}

	// Up needs 10 s.
	tr.Observe(trackResult(101, op.RCOK))
	waitTrackPending(t, tr, 1, trackUp)
	fake.Advance(9 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if n := len(bus.Recent(0)); n != 2 {
		t.Fatalf("up before its delay: %v", trackEvents(bus))
	}
	fake.Advance(time.Second)
	waitTrackEvents(t, bus, 3)
	want := []string{
		"%TRACK-6-STATE: 1 ip sla 101 state Unknown -> Up",
		"%TRACK-6-STATE: 1 ip sla 101 state Up -> Down",
		"%TRACK-6-STATE: 1 ip sla 101 state Down -> Up",
	}
	if diff := cmp.Diff(want, trackEvents(bus)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestTrackManyDelaysOneGoroutine(t *testing.T) {
	fake := clock.NewFake(base)
	bus := event.NewBus(quiet())
	tr := NewTracker(fake, bus, quiet())
	var tracks []config.Track
	for i := 1; i <= 1000; i++ {
		tracks = append(tracks, config.Track{ID: i, Operation: i, Mode: config.TrackState, DelayDown: time.Duration(i%10+1) * time.Second})
	}
	ids := make([]int, 1000)
	for i := range ids {
		ids[i] = i + 1
	}
	tr.Configure(tracks, trackedOps(ids...))
	for i := 1; i <= 1000; i++ {
		tr.Observe(trackResult(i, op.RCOK))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for i := 1; i <= 1000; i++ {
		tr.Observe(trackResult(i, op.RCTimeout))
	}
	for sec := 1; sec <= 10; sec++ {
		fake.Advance(time.Second)
		waitTrackEvents(t, bus, 1000+sec*100)
	}
	for _, s := range tr.All() {
		if s.State != trackDown {
			t.Fatalf("track %d is %s", s.ID, s.State)
		}
	}
}

func TestTrackConfigureKeepsState(t *testing.T) {
	bus := event.NewBus(quiet())
	fake := clock.NewFake(base)
	tr := NewTracker(fake, bus, quiet())
	tr.Configure([]config.Track{
		{ID: 1, Operation: 101, Mode: config.TrackState},
		{ID: 2, Operation: 102, Mode: config.TrackState},
		{ID: 3, Operation: 103, Mode: config.TrackState},
		{ID: 4, Operation: 104, Mode: config.TrackState},
	}, trackedOps(101, 102, 103, 104))
	for _, id := range []int{101, 102, 103, 104} {
		tr.Observe(trackResult(id, op.RCOK))
	}
	n := len(bus.Recent(0))

	tr.Configure([]config.Track{
		{ID: 1, Operation: 101, Mode: config.TrackState, DelayDown: 3 * time.Second}, // same: kept, new delay
		{ID: 2, Operation: 102, Mode: config.TrackReachability},                      // mode changed: unknown
		{ID: 3, Operation: 999, Mode: config.TrackState},                             // other op (missing): unknown
		{ID: 5, Operation: 103, Mode: config.TrackState},                             // new
	}, trackedOps(101, 102, 103))
	if len(bus.Recent(0)) != n {
		t.Fatalf("Configure published: %v", trackEvents(bus))
	}
	states := map[int]string{}
	for _, s := range tr.All() {
		states[s.ID] = s.State
	}
	if diff := cmp.Diff(map[int]string{1: "up", 2: "unknown", 3: "unknown", 5: "unknown"}, states); diff != "" {
		t.Errorf("states (-want +got):\n%s", diff)
	}
	s1, _ := tr.Snapshot(1)
	if s1.Changes != 1 || s1.DelayDown != 3*time.Second {
		t.Errorf("kept track: %+v", s1)
	}
	if _, ok := tr.Snapshot(4); ok {
		t.Error("removed track still present")
	}
	tr.Observe(trackResult(999, op.RCOK)) // the missing operation's results never come; nothing to do
	tr.Observe(trackResult(101, op.RCTimeout))
	if s, _ := tr.Snapshot(1); s.Pending != trackDown {
		t.Errorf("new delay not applied: %+v", s)
	}
}

func TestTrackerSetTag(t *testing.T) {
	bus := event.NewBus(quiet())
	tr := NewTracker(clock.NewFake(base), bus, quiet())
	tr.Configure([]config.Track{{ID: 1, Operation: 101, Mode: config.TrackState}}, trackedOps(101))
	tr.Observe(trackResult(101, op.RCOK))
	tr.SetTag(101, "core")
	tr.Observe(trackResult(101, op.RCTimeout))
	evs := bus.Recent(0)
	if len(evs) != 2 || evs[0].Tag != "wan" || evs[1].Tag != "core" {
		t.Errorf("events %+v", evs)
	}
	if s, _ := tr.Snapshot(1); s.State != trackDown || s.Changes != 2 {
		t.Errorf("state %+v", s)
	}
}

// A track whose operation disappears goes unknown and says so (audit B17).
func TestTrackOperationGoneIsLogged(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	tr := NewTracker(clock.NewFake(base), event.NewBus(quiet()), logger)
	tracks := []config.Track{{ID: 1, Operation: 101, Mode: config.TrackState}}
	tr.Configure(tracks, trackedOps(101))
	tr.Observe(trackResult(101, op.RCOK))
	tr.Configure(tracks, trackedOps()) // operation 101 removed
	if s, _ := tr.Snapshot(1); s.State != trackUnknown {
		t.Fatalf("state %q, want unknown", s.State)
	}
	if got := logs.String(); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "track operation is gone") ||
		!strings.Contains(got, "track=1 op=101 was=up") {
		t.Errorf("log %q", got)
	}
	logs.Reset()
	tr.Configure(tracks, trackedOps()) // still gone: said once
	if logs.Len() != 0 {
		t.Errorf("logged again: %q", logs.String())
	}
}
