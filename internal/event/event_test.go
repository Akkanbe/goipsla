package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/clock"
	"goipsla/internal/config"
)

var base = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func thresholdEvent(id int, action string) Event {
	return Event{
		Time: base, Kind: ThresholdExceeded, OpID: id, Type: config.ICMPEcho, Target: "10.100.1.11", Tag: "wan",
		Element: "rtt", ThresholdType: "immediate", Value: 350, Upper: 300, Lower: 200, Action: action, ReactionIndex: 2,
		Message: ThresholdMessage(id, ThresholdExceeded, "rtt", false, 350, 300, 200),
	}
}

func trackEvent(track int, state string) Event {
	k := TrackUp
	if state == "down" {
		k = TrackDown
	}
	rtt := 4.0
	return Event{
		Time: base, Kind: k, OpID: 101, Type: config.ICMPEcho, Target: "10.100.1.11", TrackID: track,
		TrackMode: "reachability", TrackState: state, LatestRC: "ok", LatestRTTMs: &rtt,
		Message: TrackMessage(track, 101, "reachability", "down", state),
	}
}

// recorder is a sink that records events; block, if set, delays each Deliver.
type recorder struct {
	name  string
	block chan struct{}
	fail  bool

	mu  sync.Mutex
	got []int
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Deliver(ctx context.Context, ev Event) error {
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.got = append(r.got, ev.OpID)
	r.mu.Unlock()
	if r.fail {
		return errors.New("boom")
	}
	return nil
}

func (r *recorder) ids() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.got...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func runBus(t *testing.T, b *Bus) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

func TestMessages(t *testing.T) {
	tests := []struct{ got, want string }{
		{ThresholdMessage(5, ThresholdExceeded, "rtt", false, 350, 300, 200), "%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold exceeded for rtt (value 350, rising 300, falling 200)"},
		{ThresholdMessage(5, ThresholdCleared, "jitterAvg", false, 1, 100, 100), "%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold below for jitterAvg (value 1, rising 100, falling 100)"},
		{ThresholdMessage(5, ThresholdExceeded, "timeout", true, 1, 0, 0), "%RTT-4-OPER_TIMEOUT: IP SLAs(5): Threshold exceeded for timeout"},
		{ThresholdMessage(5, ThresholdCleared, "timeout", true, 0, 0, 0), "%RTT-4-OPER_TIMEOUT: IP SLAs(5): Threshold below for timeout"},
		{ThresholdMessage(5, ThresholdExceeded, "verifyError", true, 1, 0, 0), "%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold exceeded for verifyError"},
		{TrackMessage(1, 101, "reachability", "up", "down"), "%TRACK-6-STATE: 1 ip sla 101 reachability Up -> Down"},
		{TrackMessage(3, 7, "state", "unknown", "up"), "%TRACK-6-STATE: 3 ip sla 7 state Unknown -> Up"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got  %s\nwant %s", tt.got, tt.want)
		}
	}
}

func TestEventJSON(t *testing.T) {
	b, err := trackEvent(1, "up").JSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"2026-09-27T12:00:00Z","kind":"track-up","op_id":101,"type":"icmp-echo","target":"10.100.1.11",` +
		`"track_id":1,"track_mode":"reachability","track_state":"up","latest_rc":"ok","latest_rtt_ms":4,` +
		`"message":"%TRACK-6-STATE: 1 ip sla 101 reachability Down -> Up"}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestBusOrderAndStats(t *testing.T) {
	b := NewBus(quiet())
	a, f := &recorder{name: "a"}, &recorder{name: "f", fail: true}
	b.Subscribe(a)
	b.Subscribe(f)
	runBus(t, b)
	for i := 1; i <= 50; i++ {
		b.Publish(Event{OpID: i})
	}
	waitFor(t, "delivery", func() bool { return len(a.ids()) == 50 && len(f.ids()) == 50 })
	want := make([]int, 50)
	for i := range want {
		want[i] = i + 1
	}
	if diff := cmp.Diff(want, a.ids()); diff != "" {
		t.Errorf("order (-want +got):\n%s", diff)
	}
	waitFor(t, "stats", func() bool { return b.Stats().Sinks[1].Failed == 50 })
	st := b.Stats()
	wantSt := BusStats{Published: 50, Sinks: []SinkStats{{Name: "a", Delivered: 50}, {Name: "f", Failed: 50}}}
	if diff := cmp.Diff(wantSt, st); diff != "" {
		t.Errorf("stats (-want +got):\n%s", diff)
	}
}

func TestSlowSinkDoesNotBlockOthers(t *testing.T) {
	b := NewBus(quiet())
	slow := &recorder{name: "slow", block: make(chan struct{})}
	fast := &recorder{name: "fast"}
	b.Subscribe(slow)
	b.Subscribe(fast)
	runBus(t, b)
	const n = sinkQueueSize + 100
	// Publish in batches and let the fast sink catch up after each: on a
	// loaded machine its goroutine may not run while a burst of n events is
	// fanned out, and its own queue would overflow too.
	for i := 1; i <= n; i++ {
		b.Publish(Event{OpID: i})
		if i%100 == 0 || i == n {
			waitFor(t, "fast sink", func() bool { return len(fast.ids()) == i })
		}
	}
	// The slow sink holds one event in Deliver and sinkQueueSize in its
	// queue; the rest were dropped for it alone.
	waitFor(t, "drops", func() bool { return b.Stats().Sinks[0].Dropped == n-sinkQueueSize-1 })
	close(slow.block)
	waitFor(t, "slow sink", func() bool { return len(slow.ids()) == sinkQueueSize+1 })
	st := b.Stats()
	if st.Sinks[1].Dropped != 0 || st.Sinks[1].Delivered != n || st.Dropped != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestQueueOverflow(t *testing.T) {
	b := NewBus(quiet())
	b.Subscribe(&recorder{name: "a"})
	// Not running: the main queue fills up.
	for i := 0; i < queueSize+5; i++ {
		b.Publish(Event{OpID: i})
	}
	st := b.Stats()
	if st.Published != queueSize+5 || st.Dropped != 5 {
		t.Errorf("stats %+v", st)
	}
}

func TestRecent(t *testing.T) {
	b := NewBus(quiet())
	if got := b.Recent(10); len(got) != 0 {
		t.Fatalf("empty bus: %v", got)
	}
	for i := 1; i <= 3; i++ {
		b.Publish(Event{OpID: i})
	}
	ids := func(evs []Event) []int {
		var out []int
		for _, e := range evs {
			out = append(out, e.OpID)
		}
		return out
	}
	if diff := cmp.Diff([]int{2, 3}, ids(b.Recent(2))); diff != "" {
		t.Error(diff)
	}
	if diff := cmp.Diff([]int{1, 2, 3}, ids(b.Recent(0))); diff != "" {
		t.Error(diff)
	}
	for i := 4; i <= recentSize+10; i++ {
		b.Publish(Event{OpID: i})
	}
	all := ids(b.Recent(0))
	if len(all) != recentSize || all[0] != 11 || all[len(all)-1] != recentSize+10 {
		t.Errorf("wrapped ring: len %d first %d last %d", len(all), all[0], all[len(all)-1])
	}
	if diff := cmp.Diff([]int{recentSize + 9, recentSize + 10}, ids(b.Recent(2))); diff != "" {
		t.Error(diff)
	}
}

func TestLogSink(t *testing.T) {
	var buf bytes.Buffer
	s := NewLogSink(slog.New(slog.NewJSONHandler(&buf, nil)))
	if err := s.Deliver(context.Background(), thresholdEvent(5, "trap")); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{
		"msg": "event", "level": "INFO", "kind": "threshold-exceeded", "op": 5.0, "type": "icmp-echo",
		"target": "10.100.1.11", "tag": "wan", "element": "rtt", "threshold_type": "immediate",
		"value": 350.0, "upper": 300.0, "lower": 200.0, "action": "trap", "reaction_index": 2.0,
		"message": "%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold exceeded for rtt (value 350, rising 300, falling 200)",
	} {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	for _, k := range []string{"vrf", "track_id", "latest_rtt_ms", "op_id"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s logged", k)
		}
	}
	// One "time" (the log line's); the event's own is event_time (audit).
	if n := strings.Count(buf.String(), `"time":`); n != 1 {
		t.Errorf("%d time keys in %s", n, buf.String())
	}
	if _, ok := m["event_time"]; !ok {
		t.Errorf("no event_time in %s", buf.String())
	}
}

func TestActionFilter(t *testing.T) {
	f := newActionFilter(config.Action{On: []string{"track-down", "threshold-exceeded"}, Track: 2})
	tests := []struct {
		ev   Event
		want bool
	}{
		{trackEvent(2, "down"), true},
		{trackEvent(1, "down"), false}, // other track
		{trackEvent(2, "up"), false},   // kind not listed
		{thresholdEvent(5, "none"), true},
		{Event{Kind: ThresholdCleared}, false},
	}
	for i, tt := range tests {
		if got := f.wants(tt.ev); got != tt.want {
			t.Errorf("%d: wants = %v", i, got)
		}
	}
	all := newActionFilter(config.Action{On: []string{"track-up"}})
	if !all.wants(trackEvent(9, "up")) {
		t.Error("track 0 should match every track")
	}
}

func TestWebhook(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	status := []int{500, 500, 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s", r.Method, r.Header.Get("Content-Type"))
		}
		bodies = append(bodies, string(b))
		code := 200
		if len(bodies) <= len(status) {
			code = status[len(bodies)-1]
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()

	s := NewWebhookSink(config.Action{On: []string{"track-down"}, Webhook: srv.URL}, 1, quiet())
	s.delays = []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	if s.Name() != "webhook#1 "+srv.URL { // srv.URL has no path
		t.Errorf("name %q", s.Name())
	}
	ev := trackEvent(1, "down")
	if err := s.Deliver(context.Background(), ev); err != nil {
		t.Fatalf("500, 500, 200: %v", err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d requests, want 3", len(bodies))
	}
	var got Event
	if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil || got.TrackID != 1 || got.Kind != TrackDown {
		t.Errorf("body %s (%v)", bodies[0], err)
	}

	// Filtered out: no request.
	if err := s.Deliver(context.Background(), trackEvent(1, "up")); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Error("filtered event was posted")
	}

	// Always failing: 1 + 3 retries, then an error.
	status = []int{503, 503, 503, 503, 503, 503, 503, 503}
	bodies = nil
	err := s.Deliver(context.Background(), ev)
	if err == nil || !strings.Contains(err.Error(), "status 503 Service Unavailable (after 4 attempts)") {
		t.Errorf("got %v", err)
	}
	if len(bodies) != 4 {
		t.Errorf("%d requests, want 4", len(bodies))
	}
	if !cmp.Equal(webhookRetryDelays, []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}) {
		t.Errorf("retry delays %v", webhookRetryDelays)
	}
}

func TestWebhookCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	s := NewWebhookSink(config.Action{On: []string{"track-down"}, Webhook: srv.URL}, 1, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if err := s.Deliver(ctx, trackEvent(1, "down")); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Error("cancel did not interrupt the retry wait")
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "notify.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExec(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	script := writeScript(t, `{
  echo "kind=$GOIPSLA_EVENT_KIND op=$GOIPSLA_OP_ID target=$GOIPSLA_TARGET tag=$GOIPSLA_TAG element=$GOIPSLA_ELEMENT value=$GOIPSLA_VALUE track=$GOIPSLA_TRACK_ID state=$GOIPSLA_TRACK_STATE pwd=$(pwd)"
  cat
} > `+out+"\n")
	s := NewExecSink(config.Action{On: []string{"threshold-exceeded"}, Exec: script}, 1, quiet())
	if err := s.Deliver(context.Background(), thresholdEvent(5, "none")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(b), "\n", 2)
	want := "kind=threshold-exceeded op=5 target=10.100.1.11 tag=wan element=rtt value=350 track=0 state= pwd=/"
	if lines[0] != want {
		t.Errorf("env line\n got %s\nwant %s", lines[0], want)
	}
	var ev Event
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil || ev.OpID != 5 || ev.Element != "rtt" {
		t.Errorf("stdin %q (%v)", lines[1], err)
	}

	// Filtered out: not run.
	os.Remove(out)
	if err := s.Deliver(context.Background(), trackEvent(1, "up")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Error("filtered event ran the program")
	}
}

func TestExecFailureAndTimeout(t *testing.T) {
	fail := NewExecSink(config.Action{On: []string{"track-up"}, Exec: writeScript(t, "echo oops >&2\nexit 3\n")}, 1, quiet())
	err := fail.Deliver(context.Background(), trackEvent(1, "up"))
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("exit 3: %v", err)
	}

	slow := NewExecSink(config.Action{On: []string{"track-up"}, Exec: writeScript(t, "sleep 10\n")}, 1, quiet())
	slow.timeout = 200 * time.Millisecond
	start := time.Now()
	err = slow.Deliver(context.Background(), trackEvent(1, "up"))
	if err == nil || !strings.Contains(err.Error(), "killed after 200ms") {
		t.Errorf("timeout: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}

	// The timeout kills what the program started too.
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "child")
	tree := NewExecSink(config.Action{On: []string{"track-up"}, Exec: writeScript(t, "sleep 100 &\necho $! > "+pidfile+"\nwait\n")}, 1, quiet())
	tree.timeout = 300 * time.Millisecond
	if err := tree.Deliver(context.Background(), trackEvent(1, "up")); err == nil || !strings.Contains(err.Error(), "killed after") {
		t.Errorf("tree timeout: %v", err)
	}
	assertGone(t, pidfile)

	// A child left running after the program exits is killed as well.
	pidfile2 := filepath.Join(dir, "orphan")
	orphan := NewExecSink(config.Action{On: []string{"track-up"}, Exec: writeScript(t, "sleep 100 >/dev/null 2>&1 &\necho $! > "+pidfile2+"\nexit 0\n")}, 1, quiet())
	if err := orphan.Deliver(context.Background(), trackEvent(1, "up")); err != nil {
		t.Errorf("orphan: %v", err)
	}
	assertGone(t, pidfile2)

	missing := NewExecSink(config.Action{On: []string{"track-up"}, Exec: "/nonexistent/goipsla-notify"}, 1, quiet())
	if err := missing.Deliver(context.Background(), trackEvent(1, "up")); err == nil {
		t.Error("missing program: no error")
	}
}

// assertGone checks that the process whose pid is in file has ended.
func assertGone(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		// A killed child of ours may linger as a zombie until init reaps it;
		// /proc/<pid>/stat state Z counts as gone.
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil || strings.Contains(string(st), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running: %s", pid, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecConcurrencyLimit(t *testing.T) {
	dir := t.TempDir()
	// Each run records its start, waits, and records its end; the log shows
	// how many ran at once.
	script := writeScript(t, fmt.Sprintf("echo start >> %s/log\nsleep 0.3\necho end >> %s/log\n", dir, dir))
	var wg sync.WaitGroup
	for i := 0; i < execMaxRunning+3; i++ {
		s := NewExecSink(config.Action{On: []string{"track-up"}, Exec: script}, 1, quiet())
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Deliver(context.Background(), trackEvent(1, "up")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	running, peak := 0, 0
	for _, l := range strings.Fields(string(b)) {
		if l == "start" {
			running++
		} else {
			running--
		}
		peak = max(peak, running)
	}
	if peak > execMaxRunning || peak < 2 {
		t.Errorf("peak concurrency %d, limit %d", peak, execMaxRunning)
	}
}

func TestSyslog(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "log.sock")

	// Not listening yet: the constructor only warns, Deliver fails.
	s, err := newSyslogSink("local0", "unixgram", path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Deliver(context.Background(), trackEvent(1, "up")); err == nil {
		t.Fatal("deliver without syslog: no error")
	}

	pc, err := net.ListenPacket("unixgram", path)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	read := func() string {
		t.Helper()
		if err := pc.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2048)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return ""
		}
		return string(buf[:n])
	}

	// Reconnects; track events always go.
	if err := s.Deliver(context.Background(), trackEvent(1, "up")); err != nil {
		t.Fatal(err)
	}
	msg := read()
	// local0 (16) * 8 + info (6) = 134
	if !strings.HasPrefix(msg, "<134>") || !strings.Contains(msg, "goipslad[") ||
		!strings.HasSuffix(strings.TrimSpace(msg), "%TRACK-6-STATE: 1 ip sla 101 reachability Down -> Up") {
		t.Errorf("got %q", msg)
	}

	// threshold-* only with action syslog / trap-and-syslog.
	for _, a := range []string{"none", "trap"} {
		if err := s.Deliver(context.Background(), thresholdEvent(5, a)); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []string{"syslog", "trap-and-syslog"} {
		if err := s.Deliver(context.Background(), thresholdEvent(5, a)); err != nil {
			t.Fatal(err)
		}
		if m := read(); !strings.Contains(m, "%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold exceeded for rtt") {
			t.Errorf("action %s: got %q", a, m)
		}
	}
	if err := pc.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, _, err := pc.ReadFrom(make([]byte, 2048)); err == nil {
		t.Errorf("unexpected extra message (%d bytes): none / trap were sent", n)
	}

	if _, err := NewSyslogSink("local9", quiet()); err == nil {
		t.Error("bad facility accepted")
	}
}

// syncBuffer is a concurrency-safe log sink for tests.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines(substr string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range strings.Split(s.b.String(), "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

func TestDeliveryWarningsThrottled(t *testing.T) {
	var logs syncBuffer
	b := NewBus(slog.New(slog.NewTextHandler(&logs, nil)))
	fake := clock.NewFake(base)
	b.clk = fake
	b.Subscribe(&recorder{name: "down", fail: true})
	b.Subscribe(&recorder{name: "also-down", fail: true})
	runBus(t, b)

	failed := func(n uint64) func() bool {
		return func() bool { st := b.Stats(); return st.Sinks[0].Failed == n && st.Sinks[1].Failed == n }
	}
	for i := 1; i <= 5; i++ {
		b.Publish(Event{OpID: i})
	}
	waitFor(t, "5 failures", failed(5))
	fake.Advance(59 * time.Second)
	b.Publish(Event{OpID: 6})
	waitFor(t, "6 failures", failed(6))
	if got := logs.lines("event delivery failed"); len(got) != 2 { // one per sink
		t.Fatalf("within a minute: %d warnings\n%s", len(got), strings.Join(got, "\n"))
	}
	fake.Advance(time.Second)
	b.Publish(Event{OpID: 7})
	waitFor(t, "7 failures", failed(7))
	got := logs.lines("event delivery failed")
	if len(got) != 4 {
		t.Fatalf("after a minute: %d warnings\n%s", len(got), strings.Join(got, "\n"))
	}
	for _, l := range got[2:] {
		if !strings.Contains(l, "suppressed=5") || !strings.Contains(l, "op=7") {
			t.Errorf("warning %q", l)
		}
	}
}

func TestThrottle(t *testing.T) {
	var th throttle
	steps := []struct {
		at         time.Duration
		ok         bool
		suppressed uint64
	}{{0, true, 0}, {time.Second, false, 0}, {30 * time.Second, false, 0}, {60 * time.Second, true, 2}, {61 * time.Second, false, 0}, {3 * time.Minute, true, 1}}
	for _, s := range steps {
		ok, n := th.allow(base.Add(s.at))
		if ok != s.ok || n != s.suppressed {
			t.Errorf("at %v: %v %d, want %v %d", s.at, ok, n, s.ok, s.suppressed)
		}
	}
}
