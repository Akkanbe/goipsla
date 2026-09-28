package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/manager"
	"goipsla/internal/metrics"
	"goipsla/internal/op"
	"goipsla/internal/probe"
	"goipsla/internal/react"
	"goipsla/internal/stats"
)

var providerEpoch = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// fakeProviderEngine answers every request with a fixed RTT, or a timeout for the
// targets in timeouts.
type fakeProviderEngine struct{ timeouts map[netip.Addr]bool }

func (e fakeProviderEngine) Echo(_ context.Context, req probe.Request) probe.Reply {
	if e.timeouts[req.Target] {
		return probe.Reply{Outcome: probe.OutcomeTimeout}
	}
	return probe.Reply{Outcome: probe.OutcomeReply, RTT: 1500 * time.Microsecond}
}

func (fakeProviderEngine) Close() error { return nil }

func (fakeProviderEngine) Stats() probe.Stats { return probe.Stats{Foreign: 3, Late: 1} }

func (fakeProviderEngine) Forget(uint32) {}

func (fakeProviderEngine) Jitter(context.Context, probe.JitterRequest) probe.JitterReply {
	return probe.JitterReply{Err: errors.New("jitter not supported by fakeProviderEngine")}
}

func providerTestOp(id int, target string) *config.Operation {
	return &config.Operation{
		ID:              id,
		Type:            config.ICMPEcho,
		Target:          netip.MustParseAddr(target),
		TargetName:      target,
		Frequency:       5 * time.Second,
		Timeout:         2 * time.Second,
		Threshold:       100 * time.Millisecond,
		RequestDataSize: 28,
		DataPattern:     probe.DefaultPattern,
		Tag:             "lab",
		VRF:             "blue",
		Stats:           config.StatsConfig{HoursKept: 2, DistBuckets: 5, DistInterval: 10 * time.Millisecond},
		History:         config.HistoryConfig{Lives: 2, Buckets: 15, Filter: "all"},
		Enhanced:        &config.EnhancedHistory{Interval: time.Minute, Buckets: 10},
		Schedule:        &config.Schedule{Start: "at", At: providerEpoch, Forever: true},
	}
}

// TestProviderOverAPI runs the real manager and store behind the real API
// server and reads them back with the API client, as goipsla does.
func TestProviderOverAPI(t *testing.T) {
	ok := providerTestOp(11, "10.100.1.11")
	lost := providerTestOp(13, "10.100.1.13")
	pending := providerTestOp(21, "10.100.2.11")
	pending.Schedule = &config.Schedule{Start: "pending", Forever: true}
	cfg := &config.Config{Operations: []*config.Operation{ok, lost, pending}}

	clk := clock.NewFake(providerEpoch)
	store := stats.NewStore(clk)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := fakeProviderEngine{timeouts: map[netip.Addr]bool{lost.Target: true}}
	mgr := manager.New(cfg, eng, clk, store.Record, logger)
	mgr.SetStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// Both scheduled operations start at providerEpoch; wait for their first results.
	waitForProvider(t, func() bool {
		s11, ok11 := store.Snapshot(11, stats.SnapshotOptions{})
		s13, ok13 := store.Snapshot(13, stats.SnapshotOptions{})
		return ok11 && ok13 && s11.Totals.Initiations == 1 && s13.Totals.Initiations == 1
	})

	started := providerEpoch.Add(-time.Minute)
	prov := newProvider("test-version", store, mgr, "/etc/goipslad/config.yaml", started, logger)
	sock := apitest.Serve(t, prov)
	c := api.NewClient(sock)

	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != "test-version" || !h.StartedAt.Equal(started) || h.ConfigPath != "/etc/goipslad/config.yaml" ||
		h.Operations.Total != 3 || h.Operations.Active != 2 || h.Operations.Pending != 1 {
		t.Errorf("health = %+v", h)
	}

	rows, err := c.Operations(context.Background(), api.OperationFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	want := []struct {
		id    int
		state string
		code  op.ReturnCode
		valid bool
	}{
		{11, api.StateActive, op.RCOK, true},
		{13, api.StateActive, op.RCTimeout, true},
		{21, api.StatePending, op.RCOther, false},
	}
	for i, w := range want {
		r := rows[i]
		if r.ID != w.id || r.State != w.state || r.Latest.Code != w.code || r.Latest.Valid != w.valid || r.VRF != "blue" || r.Tag != "lab" {
			t.Errorf("row %d = %+v, want id %d state %s code %v", i, r, w.id, w.state, w.code)
		}
	}
	if r := rows[0]; r.Latest.RTTMs == nil || *r.Latest.RTTMs != 1.5 || r.Totals.Completions != 1 {
		t.Errorf("row 11 latest %+v totals %+v, want rtt 1.5 ms and 1 completion", r.Latest, r.Totals)
	}
	if r := rows[1]; r.Totals.Timeouts != 1 || r.Totals.Failures != 1 {
		t.Errorf("row 13 totals %+v, want 1 timeout", r.Totals)
	}

	d, err := c.Operation(context.Background(), 11, stats.SnapshotOptions{Hours: true, History: true, Enhanced: true})
	if err != nil {
		t.Fatal(err)
	}
	if !d.LifeStart.Equal(providerEpoch) || d.LifeIndex != 1 || len(d.Hours) != 1 || len(d.Hours[0].Dist) != 5 ||
		len(d.History) != 1 || len(d.Enhanced) != 1 {
		t.Errorf("detail = %+v", d)
	}
	var effective map[string]any
	if err := json.Unmarshal(d.Config, &effective); err != nil || effective["frequency"] != "5s" || effective["vrf"] != "blue" {
		t.Errorf("config = %s (err %v), want the effective configuration", d.Config, err)
	}

	if _, err := c.Operation(context.Background(), 99, stats.SnapshotOptions{}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Operation(99) error = %v, want ErrNotFound", err)
	}

	// Control calls.
	if err := c.Restart(context.Background(), 11); err != nil {
		t.Errorf("Restart(11) = %v", err)
	}
	// A pending operation cannot be restarted: 409 Conflict (api.ErrNotActive).
	err = c.Restart(context.Background(), 21)
	var ae *api.Error
	const notActive = "operation 21 is pending: only an active operation can be restarted"
	if !errors.Is(err, api.ErrNotActive) || !errors.As(err, &ae) || ae.Status != http.StatusConflict ||
		ae.Message != notActive || err.Error() != notActive {
		t.Errorf("Restart(pending) = %q (%+v), want 409 ErrNotActive with the body %q", err, ae, notActive)
	}
	// The same wording as the API's own 404 for GET /v1/operations/99.
	if err := c.Restart(context.Background(), 99); !errors.Is(err, api.ErrNotFound) || err.Error() != "operation 99 not found" {
		t.Errorf("Restart(99) = %q, want ErrNotFound %q", err, "operation 99 not found")
	}
	if err := c.Reset(context.Background()); err != nil {
		t.Errorf("Reset = %v", err)
	}

	// Reload: a configuration that fails to load leaves everything as is.
	prov.load = func(string) (*config.Config, error) {
		return nil, &config.ValidationError{Errors: []config.FieldError{{Path: "operations[0].timeout", Msg: "must be at least 1ms"}}}
	}
	// 400 with every validation error in Errors.
	_, err = c.Reload(context.Background())
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest ||
		!slices.Equal(ae.Errors, []string{"operations[0].timeout: must be at least 1ms"}) {
		t.Errorf("Reload of an invalid file = %v (%+v), want 400 with the validation error", err, ae)
	}
	if rows, _ := c.Operations(context.Background(), api.OperationFilter{}); len(rows) != 3 {
		t.Errorf("after a failed reload: %d operations, want 3", len(rows))
	}
	// A valid one is applied: 21 removed, 11 retagged, 31 added.
	retagged := *ok
	retagged.Tag = "wan"
	added := providerTestOp(31, "10.100.1.31")
	prov.load = func(string) (*config.Config, error) {
		c := &config.Config{Operations: []*config.Operation{&retagged, lost, added}}
		c.Global.MetricsListen = "0.0.0.0:9818"
		return c, nil
	}
	res, err := c.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []int{31}) || !slices.Equal(res.Removed, []int{21}) || len(res.Restarted) != 0 ||
		!slices.Equal(res.Updated, []int{11}) || len(res.Warnings) != 1 {
		t.Errorf("reload result = %+v", res)
	}
	rows, err = c.Operations(context.Background(), api.OperationFilter{Tag: "wan"})
	if err != nil || len(rows) != 1 || rows[0].ID != 11 {
		t.Errorf("rows tagged wan = %+v (err %v), want operation 11 with its new tag", rows, err)
	}
}

// TestScheduleFields checks life_left_s, next_start and ageout_left_s.
func TestScheduleFields(t *testing.T) {
	life := providerTestOp(1, "10.100.1.11")
	life.Schedule = &config.Schedule{Start: "at", At: providerEpoch, Life: time.Hour}
	later := providerTestOp(2, "10.100.1.12")
	later.Schedule = &config.Schedule{Start: "at", At: providerEpoch.Add(time.Hour), Forever: true, Ageout: 2 * time.Hour}
	forever := providerTestOp(3, "10.100.1.13")
	cfg := &config.Config{Operations: []*config.Operation{life, later, forever}}

	clk := clock.NewFake(providerEpoch)
	store := stats.NewStore(clk)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(cfg, fakeProviderEngine{}, clk, store.Record, logger)
	mgr.SetStore(store)
	mgr.Start() // the schedule state is set up; Run below only runs it
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()
	defer func() { cancel(); <-done }()

	rows := newProvider("test-version", store, mgr, "x", providerEpoch, logger).Operations()
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if r := rows[0]; r.LifeLeftS == nil || *r.LifeLeftS != 3600 || r.NextStart != nil || r.Ageout != nil {
		t.Errorf("row 1 = %+v", r)
	}
	if r := rows[1]; r.LifeLeftS != nil || r.NextStart == nil || !r.NextStart.Equal(providerEpoch.Add(time.Hour)) ||
		r.Ageout == nil || *r.Ageout != 7200 {
		t.Errorf("row 2 = %+v", r)
	}
	if r := rows[2]; r.LifeLeftS != nil || r.NextStart != nil || r.Ageout != nil {
		t.Errorf("row 3 = %+v", r)
	}
}

func waitForProvider(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestMetricsRows checks the metrics.Source view of the provider.
func TestMetricsRows(t *testing.T) {
	a := providerTestOp(1, "10.100.1.11")
	b := providerTestOp(2, "10.100.1.12")
	b.Schedule = &config.Schedule{Start: "pending", Forever: true}
	cfg := &config.Config{Operations: []*config.Operation{a, b}}
	clk := clock.NewFake(providerEpoch)
	store := stats.NewStore(clk)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(cfg, fakeProviderEngine{}, clk, store.Record, logger)
	mgr.SetStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitForProvider(t, func() bool {
		s, ok := store.Snapshot(1, stats.SnapshotOptions{})
		return ok && s.Totals.Initiations == 1
	})

	rows := newProvider("test-version", store, mgr, "x", providerEpoch, logger).MetricsView().Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	if r := rows[0]; r.ID != 1 || r.State != "active" || r.Target != "10.100.1.11" || r.Tag != "lab" || r.VRF != "blue" ||
		!r.LifeStart.Equal(providerEpoch) || r.Totals.Completions != 1 || !r.Latest.Valid {
		t.Errorf("row 1 = %+v", r)
	}
	if r := rows[1]; r.ID != 2 || r.State != "pending" || r.Latest.Valid {
		t.Errorf("row 2 = %+v", r)
	}
}

// TestProviderEvents runs the manager with the reaction engine, the tracker
// and a bus, and reads them through the provider (API and metrics views).
func TestProviderEvents(t *testing.T) {
	a := providerTestOp(13, "10.100.1.13")
	a.React = []config.Reaction{
		{Element: "rtt", ThresholdType: "immediate", Upper: 100, Lower: 50, Action: "syslog"},
		{Element: "timeout", ThresholdType: "immediate", Action: "syslog"},
	}
	b := providerTestOp(21, "10.100.2.11")
	cfg := &config.Config{
		Operations: []*config.Operation{a, b},
		Tracks:     []config.Track{{ID: 1, Operation: 13, Mode: "reachability"}},
	}
	clk := clock.NewFake(providerEpoch)
	store := stats.NewStore(clk)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger)
	reactions := react.NewEngine(clk, bus, logger)
	tracker := react.NewTracker(clk, bus, logger)
	eng := fakeProviderEngine{timeouts: map[netip.Addr]bool{a.Target: true}}
	observe := func(r op.Result) { store.Record(r); reactions.Observe(r); tracker.Observe(r) }
	mgr := manager.New(cfg, eng, clk, observe, logger)
	mgr.SetStore(store)
	mgr.SetReactions(reactions, tracker)
	prov := newProvider("test-version", store, mgr, "x", providerEpoch, logger)
	prov.SetEvents(bus, reactions, tracker)
	collected := &providerTestSink{}
	bus.Subscribe(prov.CountSink(collected))
	ctx, cancel := context.WithCancel(context.Background())
	busDone := make(chan error, 1)
	go func() { busDone <- bus.Run(ctx) }()
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()
	defer func() { cancel(); <-done; <-busDone }()

	// 13 times out at providerEpoch: timeout occurred, track 1 down (its first judgement).
	waitForProvider(t, func() bool { return len(collected.get()) == 2 })

	rows, ok := prov.Reactions(0)
	if !ok || len(rows) != 2 || rows[1].Element != "timeout" || !rows[1].Occurred || rows[0].Occurred {
		t.Errorf("Reactions(0) = %+v", rows)
	}
	if rows, ok := prov.Reactions(21); !ok || len(rows) != 0 {
		t.Errorf("Reactions(21) = %+v, %v; want an empty list", rows, ok)
	}
	if _, ok := prov.Reactions(99); ok {
		t.Error("Reactions(99) ok for an unknown operation")
	}
	tracks, ok := prov.Tracks(1)
	if !ok || len(tracks) != 1 || tracks[0].State != "down" || tracks[0].LatestRC != op.RCTimeout {
		t.Errorf("Tracks(1) = %+v", tracks)
	}
	if _, ok := prov.Tracks(2); ok {
		t.Error("Tracks(2) ok for an unknown track")
	}
	evs := prov.Events(10)
	if len(evs) != 2 {
		t.Fatalf("Events = %+v", evs)
	}
	kinds := map[event.Kind]bool{evs[0].Kind: true, evs[1].Kind: true}
	if !kinds[event.ThresholdExceeded] || !kinds[event.TrackDown] {
		t.Errorf("event kinds = %v", kinds)
	}

	src := prov.MetricsView()
	if tr := src.Tracks(); len(tr) != 1 || tr[0].State != "down" || tr[0].Operation != 13 {
		t.Errorf("metrics tracks = %+v", tr)
	}
	if rr := src.Reactions(); len(rr) != 2 || !rr[1].Occurred {
		t.Errorf("metrics reactions = %+v", rr)
	}
	counts := map[string]uint64{}
	for _, c := range src.EventStats() {
		counts[c.Kind+"/"+c.Sink+"/"+c.Result] = c.Count
	}
	if counts["threshold-exceeded/collect/delivered"] != 1 || counts["track-down/collect/delivered"] != 1 ||
		counts["/collect/dropped"] != 0 || counts["/bus/dropped"] != 0 {
		t.Errorf("event stats = %v", counts)
	}
}

// providerTestSink records the events it is given.
type providerTestSink struct {
	mu  sync.Mutex
	evs []event.Event
}

func (*providerTestSink) Name() string { return "collect" }

func (c *providerTestSink) Deliver(_ context.Context, ev event.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
	return nil
}

func (c *providerTestSink) get() []event.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]event.Event(nil), c.evs...)
}

// TestSNMPView checks the snmp.Source view of the provider.
func TestSNMPView(t *testing.T) {
	life := providerTestOp(1, "10.100.1.11")
	life.Schedule = &config.Schedule{Start: "at", At: providerEpoch, Life: time.Hour}
	life.React = []config.Reaction{{Element: "rtt", ThresholdType: "immediate", Upper: 100, Lower: 50, Action: "trap"}}
	forever := providerTestOp(2, "10.100.1.12")
	cfg := &config.Config{Operations: []*config.Operation{life, forever}}
	clk := clock.NewFake(providerEpoch)
	store := stats.NewStore(clk)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reactions := react.NewEngine(clk, nil, logger)
	mgr := manager.New(cfg, fakeProviderEngine{}, clk, store.Record, logger)
	mgr.SetStore(store)
	mgr.SetReactions(reactions, nil)
	mgr.Start()
	prov := newProvider("test-version", store, mgr, "x", providerEpoch, logger)
	prov.SetEvents(nil, reactions, nil)
	src := prov.SNMPView()

	if ids := src.IDs(); !slices.Equal(ids, []int{1, 2}) {
		t.Errorf("IDs = %v", ids)
	}
	if c, ok := src.Config(2); !ok || c.Target != forever.Target {
		t.Errorf("Config(2) = %+v, %v", c, ok)
	}
	if st, left, fv, ok := src.State(1); !ok || st != "active" || fv || left != time.Hour {
		t.Errorf("State(1) = %s %v %v %v", st, left, fv, ok)
	}
	if st, _, fv, ok := src.State(2); !ok || st != "active" || !fv {
		t.Errorf("State(2) = %s forever %v %v", st, fv, ok)
	}
	if _, _, _, ok := src.State(9); ok {
		t.Error("State(9) ok for an unknown operation")
	}
	if s, ok := src.Snapshot(1, stats.SnapshotOptions{}); !ok || s.ID != 1 || !s.LifeStart.Equal(providerEpoch) {
		t.Errorf("Snapshot(1) = %+v, %v", s, ok)
	}
	if rs := src.Reactions(1); len(rs) != 1 || rs[0].Action != "trap" {
		t.Errorf("Reactions(1) = %+v", rs)
	}
	if rs := src.Reactions(2); len(rs) != 0 {
		t.Errorf("Reactions(2) = %+v", rs)
	}
}

// TestMetricsDiagnostics: the metrics view reports the engine's counters and
// the results the store discarded (audit C9 / A4).
func TestMetricsDiagnostics(t *testing.T) {
	cfg := &config.Config{Operations: []*config.Operation{providerTestOp(1, "10.100.1.11")}}
	clk := clock.NewFake(providerEpoch)
	store := stats.NewStore(clk)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(cfg, fakeProviderEngine{}, clk, store.Record, logger)
	mgr.SetStore(store)
	mgr.Start()
	prov := newProvider("test-version", store, mgr, "x", providerEpoch, logger)
	prov.SetProbe(fakeProviderEngine{})
	src, ok := prov.MetricsView().(metrics.DiagnosticsSource)
	if !ok {
		t.Fatal("the metrics view is not a metrics.DiagnosticsSource")
	}
	if d := src.Diagnostics(); d.Probe.Foreign != 3 || d.Probe.Late != 1 || d.ResultsDiscarded != 0 {
		t.Errorf("Diagnostics() = %+v", d)
	}
}

// TestReloadLogsSource: a reload from the API and one from SIGHUP log the
// same lines with source=api / source=sighup (audit, Codex main.go:179).
func TestReloadLogsSource(t *testing.T) {
	c := providerTestOp(1, "10.100.1.11")
	cfg := &config.Config{Operations: []*config.Operation{c}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	clk := clock.NewFake(providerEpoch)
	store := stats.NewStore(clk)
	mgr := manager.New(cfg, fakeProviderEngine{}, clk, store.Record, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.SetStore(store)
	mgr.Start()
	prov := newProvider("test-version", store, mgr, "/etc/goipslad/config.yaml", providerEpoch, logger)
	prov.load = func(string) (*config.Config, error) {
		next := &config.Config{Operations: []*config.Operation{c}, Warnings: []string{"templates.x: not used"}}
		return next, nil
	}
	if _, err := prov.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	prov.ReloadOnSignal()
	prov.load = func(string) (*config.Config, error) {
		return nil, &config.ValidationError{Errors: []config.FieldError{{Path: "operations[0].timeout", Msg: "must be at least 1ms"}}}
	}
	prov.ReloadOnSignal()
	out := buf.String()
	for _, want := range []string{
		`msg="reloading the configuration" source=api config=/etc/goipslad/config.yaml`,
		`msg="configuration reloaded" source=api config=/etc/goipslad/config.yaml added=[] removed=[] restarted=[] updated=[] warnings=1`,
		`msg="reloading the configuration" source=sighup config=/etc/goipslad/config.yaml`,
		`msg="configuration reloaded" source=sighup config=/etc/goipslad/config.yaml added=[] removed=[] restarted=[] updated=[] warnings=1`,
		`msg="reload failed; keeping the running configuration" source=sighup config=/etc/goipslad/config.yaml`,
		`errors="[operations[0].timeout: must be at least 1ms]"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}
