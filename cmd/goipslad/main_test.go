// Tests of main.go, the command itself (see main.go).
//
//declscope:core

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/manager"
	"goipsla/internal/op"
	"goipsla/internal/probe"
	"goipsla/internal/stats"
)

func TestLogSetting(t *testing.T) {
	tests := []struct {
		flag     string
		flagSet  bool
		fromFile string
		want     string
	}{
		{"info", false, "", "info"},
		{"info", false, "debug", "debug"},
		{"warn", true, "debug", "warn"},
		{"text", true, "", "text"},
	}
	for _, tt := range tests {
		if got := logSetting(tt.flag, tt.flagSet, tt.fromFile); got != tt.want {
			t.Errorf("logSetting(%q, %v, %q) = %q, want %q", tt.flag, tt.flagSet, tt.fromFile, got, tt.want)
		}
	}
}

func TestSink(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	cfg := &config.Config{Operations: []*config.Operation{{ID: 11, Target: netip.MustParseAddr("10.100.1.11")}}}
	sink := newResultSink(logger, targetsOf(cfg), nil, true, time.Now)
	sink(op.Result{OpID: 11, Type: config.ICMPEcho, Seq: 3, Code: op.RCOK, RTT: 1234567 * time.Nanosecond})
	sink(op.Result{OpID: 11, Type: config.ICMPEcho, Seq: 4, Code: op.RCTimeout, Detail: "destination unreachable: host, from 10.100.1.254"})
	want := `level=INFO msg=result op=11 type=icmp-echo target=10.100.1.11 seq=3 rc=ok rtt_ms=1.235
level=INFO msg=result op=11 type=icmp-echo target=10.100.1.11 seq=4 rc=timeout detail="destination unreachable: host, from 10.100.1.254"
`
	if got := buf.String(); got != want {
		t.Errorf("sink output:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrintConfigError(t *testing.T) {
	var buf bytes.Buffer
	printConfigError(&buf, "/etc/goipslad/config.yaml", &config.ValidationError{Errors: []config.FieldError{
		{Path: "operations[0].timeout", Msg: "must be less than frequency"},
		{Msg: "no operations"},
	}})
	want := "goipslad: invalid configuration /etc/goipslad/config.yaml:\n" +
		"  operations[0].timeout: must be less than frequency\n" +
		"  no operations\n"
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	buf.Reset()
	printConfigError(&buf, "x.yaml", errors.New("open x.yaml: no such file or directory"))
	if got := buf.String(); !strings.Contains(got, "goipslad: open x.yaml: no such file or directory") {
		t.Errorf("got %q", got)
	}
}

// targetsOf returns the sink's target lookup for a fixed configuration.
func targetsOf(cfg *config.Config) func(int) string {
	return func(id int) string {
		for _, c := range cfg.Operations {
			if c.ID == id {
				return c.Target.String()
			}
		}
		return ""
	}
}

func TestSinkLevels(t *testing.T) {
	cfg := &config.Config{Operations: []*config.Operation{{
		ID: 11, Type: config.ICMPEcho, Target: netip.MustParseAddr("10.100.1.11"),
		Frequency: 5 * time.Second, Timeout: 2 * time.Second, Threshold: 100 * time.Millisecond,
	}}}
	t0 := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, logAll := range []bool{false, true} {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		store := stats.NewStore(clock.NewFake(t0))
		store.Add(cfg.Operations[0], t0)
		sink := newResultSink(logger, targetsOf(cfg), store.Record, logAll, time.Now)
		sink(op.Result{OpID: 11, Type: config.ICMPEcho, Seq: 1, Start: t0, Code: op.RCOK, RTT: time.Millisecond})
		sink(op.Result{OpID: 11, Type: config.ICMPEcho, Seq: 2, Start: t0, Code: op.RCOverThreshold, RTT: time.Second})
		sink(op.Result{OpID: 11, Type: config.ICMPEcho, Seq: 3, Start: t0, Code: op.RCTimeout})

		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		// The first ok is the first result of the life: info either way.
		wantOK := `level=INFO msg="result first"`
		if logAll {
			wantOK = "level=INFO msg=result"
		}
		if len(lines) != 3 || !strings.Contains(lines[0], wantOK) || !strings.Contains(lines[1], "level=INFO") || !strings.Contains(lines[2], "level=INFO") {
			t.Errorf("logAll=%v: log =\n%s", logAll, buf.String())
		}
		snap, _ := store.Snapshot(11, stats.SnapshotOptions{})
		if snap.Totals.Initiations != 3 || snap.Totals.Completions != 2 || snap.Totals.Timeouts != 1 {
			t.Errorf("logAll=%v: totals = %+v", logAll, snap.Totals)
		}
	}
}

// slowSink delivers after a pause.
type slowSink struct{ d time.Duration }

func (slowSink) Name() string { return "slow" }

func (s slowSink) Deliver(context.Context, event.Event) error { time.Sleep(s.d); return nil }

func TestDrainBus(t *testing.T) {
	bus := event.NewBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	bus.Subscribe(slowSink{50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bus.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for i := 0; i < 3; i++ {
		bus.Publish(event.Event{Kind: event.TrackUp, OpID: i})
	}
	if drainBus(bus, 10*time.Millisecond) {
		t.Error("drainBus reported drained before the slow sink finished")
	}
	if !drainBus(bus, 5*time.Second) {
		t.Error("drainBus did not drain within 5 s")
	}
	if st := bus.Stats(); st.Sinks[0].Delivered != 3 {
		t.Errorf("delivered %d, want 3", st.Sinks[0].Delivered)
	}
}

// TestResultLogThinning: with 1,000 operations failing the same way the log
// must not carry a line per result (audit C6).
func TestResultLogThinning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	sink := newResultSink(logger, func(int) string { return "192.0.2.1" }, nil, false, func() time.Time { return now })
	res := func(seq uint32, rc op.ReturnCode) op.Result {
		return op.Result{OpID: 7, Type: config.ICMPEcho, Life: 1, Seq: seq, Code: rc, RTT: time.Millisecond}
	}
	seq := uint32(0)
	step := func(rc op.ReturnCode, n int) {
		for range n {
			seq++
			sink(res(seq, rc))
			now = now.Add(time.Second)
		}
	}
	step(op.RCOK, 3)       // first result of the life: info; the repeated ok: debug only
	step(op.RCTimeout, 70) // changed at 1, then repeats; a summary after 60 s
	step(op.RCOK, 2)       // changed (with the summary of the rest)
	sink(op.Result{OpID: 7, Type: config.ICMPEcho, Life: 1, Seq: 99, Code: op.RCSequenceError})
	sink(op.Result{OpID: 7, Type: config.ICMPEcho, Life: 1, Seq: 99, Code: op.RCSequenceError})

	want := `level=INFO msg="result first" op=7 type=icmp-echo target=192.0.2.1 seq=1 rc=ok rtt_ms=1 life=1
level=INFO msg="result changed" op=7 type=icmp-echo target=192.0.2.1 seq=4 rc=timeout from=ok
level=INFO msg="result repeated" op=7 target=192.0.2.1 rc=timeout count=60
level=INFO msg="result repeated" op=7 target=192.0.2.1 rc=timeout count=9
level=INFO msg="result changed" op=7 type=icmp-echo target=192.0.2.1 seq=74 rc=ok rtt_ms=1 from=timeout
level=INFO msg=result op=7 type=icmp-echo target=192.0.2.1 seq=99 rc=sequenceError
`
	if got := buf.String(); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

// TestResultLogFollowsLife: a new life of an operation starts the result log
// over (review R3): its first result is logged at info even when the code is
// the one the previous life ended with, and the busy / sequenceError rate
// limit and the repeat count of the previous life are dropped.
func TestResultLogFollowsLife(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	sink := newResultSink(logger, func(int) string { return "192.0.2.1" }, nil, false, func() time.Time { return now })
	res := func(life int, seq uint32, rc op.ReturnCode) {
		sink(op.Result{OpID: 7, Type: config.ICMPEcho, Life: life, Seq: seq, Code: rc})
		now = now.Add(time.Second)
	}
	res(1, 1, op.RCTimeout)       // first of life 1
	res(1, 2, op.RCTimeout)       // repeat: debug
	res(1, 2, op.RCSequenceError) // a late reply: info
	res(2, 1, op.RCTimeout)       // restart: first of life 2, same code
	res(2, 1, op.RCSequenceError) // the rate limit of life 1 does not carry over
	res(2, 2, op.RCTimeout)       // repeat: debug
	res(1, 1, op.RCTimeout)       // removed and re-created by a reload: life 1 again
	res(1, 2, op.RCTimeout)       // repeat: debug
	res(1, 1, op.RCTimeout)       // re-created once more with the same life index: Seq starts over

	want := `level=INFO msg="result first" op=7 type=icmp-echo target=192.0.2.1 seq=1 rc=timeout life=1
level=INFO msg=result op=7 type=icmp-echo target=192.0.2.1 seq=2 rc=sequenceError
level=INFO msg="result first" op=7 type=icmp-echo target=192.0.2.1 seq=1 rc=timeout life=2
level=INFO msg=result op=7 type=icmp-echo target=192.0.2.1 seq=1 rc=sequenceError
level=INFO msg="result first" op=7 type=icmp-echo target=192.0.2.1 seq=1 rc=timeout life=1
level=INFO msg="result first" op=7 type=icmp-echo target=192.0.2.1 seq=1 rc=timeout life=1
`
	if got := buf.String(); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

func TestHandleSignals(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	sigs := make(chan os.Signal, 4)
	canceled, reloaded := make(chan struct{}), 0
	exitCode := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		handleSignals(sigs, func() { close(canceled) }, func() bool { reloaded++; return true }, logger, func(c int) { exitCode <- c })
		close(done)
	}()
	sigs <- syscall.SIGHUP
	sigs <- syscall.SIGTERM
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel")
	}
	sigs <- syscall.SIGHUP // ignored while stopping
	sigs <- syscall.SIGINT // second stop signal: forced exit
	select {
	case c := <-exitCode:
		if c != 1 {
			t.Errorf("exit(%d), want exit(1)", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second signal did not force the exit")
	}
	<-done
	if reloaded != 1 {
		t.Errorf("reloaded %d times, want 1", reloaded)
	}
	for _, want := range []string{`msg="shutting down" signal=terminated`, `msg="ignoring SIGHUP while stopping"`, `level=ERROR msg="forcing exit" signal=interrupt`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestLogStarted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := &config.Config{Actions: []config.Action{{Exec: "/hook"}, {Webhook: "http://h/x", Exec: "/b"}}}
	cfg.Global.APISocketGroup = "ipsla"
	cfg.Global.MetricsListen = "127.0.0.1:9818"
	cfg.Global.Syslog = &config.SyslogConfig{Facility: "local0"}
	cfg.Global.SNMP = &config.SNMPConfig{AgentX: "tcp:127.0.0.1:705", Traps: []config.TrapTarget{{Host: "192.0.2.9"}}}
	logStarted(logger, cfg, "/run/goipslad/goipslad.sock", manager.Counts{Total: 3, Active: 1, Pending: 1, Inactive: 1}, true)
	want := `msg="goipslad started" api_socket=/run/goipslad/goipslad.sock api_socket_group=ipsla metrics_listen=127.0.0.1:9818 snmp_agentx=tcp:127.0.0.1:705 trap_targets=1 syslog_facility=local0 webhook_sinks=1 exec_sinks=2 operations_active=1 operations_pending=1 operations_inactive=1 log_results=true`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("log:\n%s\nwant a line containing:\n%s", buf.String(), want)
	}
}

func TestPartStopLogs(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	order := []string{}
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	p := &part{name: "widget",
		run:    func(ctx context.Context) error { <-ctx.Done(); note("run returned"); return nil },
		before: func() { note("before") },
		after:  func() { note("after") },
	}
	exited := make(chan *part, 1)
	p.start(exited)
	if err := p.stop(logger); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "before,run returned,after" {
		t.Errorf("order %s", got)
	}
	for _, want := range []string{`msg="stopping widget"`, `msg="widget stopped" elapsed=`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestWarnUndrained(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	bus := event.NewBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	bus.Subscribe(slowSink{time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bus.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for i := range 3 {
		bus.Publish(event.Event{Kind: event.TrackUp, OpID: i})
	}
	warnUndrained(logger, bus, 50*time.Millisecond)
	if !strings.Contains(buf.String(), `msg="event bus not drained; queued events are lost" timeout=50ms undelivered=3 sinks=[slow]`) &&
		!strings.Contains(buf.String(), `undelivered=2 sinks=[slow]`) {
		t.Errorf("log:\n%s", buf.String())
	}
}

// TestStopSignalDuringReload: a SIGTERM that comes while a reload runs, after
// more SIGHUPs, stops the daemon at once; the SIGHUPs that came during the
// reload make exactly one more reload (audit, Codex main.go:174 / :433).
func TestStopSignalDuringReload(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var mu sync.Mutex
	runs := 0
	started := make(chan struct{}, 4)
	reload := coalesce(ctx, func() {
		mu.Lock()
		runs++
		mu.Unlock()
		started <- struct{}{}
		<-release
	})
	sigs := make(chan os.Signal, 2)
	canceled := make(chan struct{})
	go handleSignals(sigs, func() { close(canceled) }, reload, logger, func(int) {})

	sigs <- syscall.SIGHUP
	<-started // the first reload runs (and blocks)
	for range 3 {
		sigs <- syscall.SIGHUP // merged into one pending reload
	}
	sigs <- syscall.SIGTERM
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM was not handled while a reload was running")
	}
	close(release) // the running reload ends; the pending one does not run after the stop
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs < 1 || runs > 2 {
		t.Errorf("%d reload runs, want 1 (plus at most the one pending before the stop)", runs)
	}
}

func TestCoalesce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var mu sync.Mutex
	runs := 0
	req := coalesce(ctx, func() {
		mu.Lock()
		runs++
		mu.Unlock()
		started <- struct{}{}
		<-release
	})
	if !req() {
		t.Fatal("the first request was not queued")
	}
	<-started
	queued := 0
	for range 5 {
		if req() {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("%d of 5 requests during a run were queued, want 1", queued)
	}
	release <- struct{}{} // end the first run
	<-started             // the merged one runs
	close(release)
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != 2 {
		t.Errorf("%d runs, want 2", runs)
	}
}

// TestFatalAfterLogger: a startup failure after the logger exists is reported
// both on stderr, as one plain line for systemctl status, and as an error
// log with err, and run returns 1 (audit C4, review R6).
func TestFatalAfterLogger(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file") // a regular file where a directory is needed
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	engineOK := func(probe.Options) (probe.Engine, error) { return closeOnlyEngine{}, nil }
	tests := []struct {
		name      string
		engine    func(probe.Options) (probe.Engine, error)
		socket    string
		plain     string // the stderr line
		structure string // the error log
	}{
		{
			name:      "no CAP_NET_RAW",
			engine:    func(probe.Options) (probe.Engine, error) { return nil, syscall.EPERM },
			plain:     "goipslad: cannot start the ICMP engine (need CAP_NET_RAW): operation not permitted\n",
			structure: `level=ERROR msg="cannot start the ICMP engine (need CAP_NET_RAW)" err="operation not permitted"`,
		},
		{
			name:      "ICMP engine",
			engine:    func(probe.Options) (probe.Engine, error) { return nil, errors.New("no IPv4 socket") },
			plain:     "goipslad: cannot start the ICMP engine: no IPv4 socket\n",
			structure: `level=ERROR msg="cannot start the ICMP engine" err="no IPv4 socket"`,
		},
		{
			name:      "API socket",
			engine:    engineOK,
			socket:    filepath.Join(blocker, "goipslad.sock"),
			plain:     "goipslad: cannot start the API: ",
			structure: `level=ERROR msg="cannot start the API" err=`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socket := tt.socket
			if socket == "" {
				socket = filepath.Join(t.TempDir(), "goipslad.sock")
			}
			conf := filepath.Join(t.TempDir(), "goipslad.yaml")
			yaml := "global:\n  api-socket: " + socket + "\n  log: {format: text}\n" +
				"operations:\n  - id: 7\n    type: icmp-echo\n    target: 192.0.2.1\n"
			if err := os.WriteFile(conf, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			saved := newEngine
			newEngine = tt.engine
			t.Cleanup(func() { newEngine = saved })

			var stdout, stderr bytes.Buffer
			if code := run([]string{"--config", conf}, &stdout, &stderr); code != 1 {
				t.Errorf("run returned %d, want 1", code)
			}
			got := stderr.String()
			if !strings.Contains(got, "\n"+tt.plain) && !strings.HasPrefix(got, tt.plain) {
				t.Errorf("stderr lacks the plain line %q:\n%s", tt.plain, got)
			}
			if !strings.Contains(got, tt.structure) {
				t.Errorf("stderr lacks the error log %q:\n%s", tt.structure, got)
			}
		})
	}
}

// closeOnlyEngine is an engine for runs that fail before any attempt: only
// Close is called.
type closeOnlyEngine struct{ probe.Engine }

func (closeOnlyEngine) Close() error { return nil }
