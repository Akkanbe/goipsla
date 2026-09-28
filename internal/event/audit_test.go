// Tests of the fixes of the 2026-09-27 audit (fix-audit.md B1, B18, B20).
//
//declscope:namespace event

package event

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goipsla/internal/config"
)

// flakySink fails while down is set.
type flakySink struct {
	down      atomic.Bool
	delivered atomic.Int64
}

func (*flakySink) Name() string { return "flaky" }

func (s *flakySink) Deliver(context.Context, Event) error {
	s.delivered.Add(1)
	if s.down.Load() {
		return errors.New("unreachable")
	}
	return nil
}

// The failure warning names the target and the element or track; a success
// after failures is logged as a recovery; the totals are logged at stop
// (B20).
func TestDeliveryLogs(t *testing.T) {
	var logs syncBuffer
	b := NewBus(slog.New(slog.NewTextHandler(&logs, nil)))
	s := &flakySink{}
	s.down.Store(true)
	b.Subscribe(s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	b.Publish(thresholdEvent(5, "trap"))
	b.Publish(trackEvent(2, "down"))
	waitFor(t, "2 failures", func() bool { return b.Stats().Sinks[0].Failed == 2 })
	s.down.Store(false)
	b.Publish(trackEvent(2, "up"))
	waitFor(t, "1 delivery", func() bool { return b.Stats().Sinks[0].Delivered == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	failed := logs.lines("event delivery failed")
	if len(failed) != 1 || !strings.Contains(failed[0], "op=5 target=10.100.1.11 element=rtt err=unreachable") {
		t.Errorf("failure warnings:\n%s", strings.Join(failed, "\n"))
	}
	recovered := logs.lines("event delivery recovered")
	if len(recovered) != 1 || !strings.Contains(recovered[0], "level=INFO") ||
		!strings.Contains(recovered[0], "sink=flaky failed=2 suppressed=1") {
		t.Errorf("recovery:\n%s", strings.Join(recovered, "\n"))
	}
	totals := logs.lines("event sink totals")
	if len(totals) != 1 || !strings.Contains(totals[0], "sink=flaky delivered=1 failed=2 dropped=0") {
		t.Errorf("totals:\n%s", strings.Join(totals, "\n"))
	}
}

// A track event names its track, not an element (B20).
func TestEventAttrsTrack(t *testing.T) {
	q := &sinkQueue{stats: SinkStats{Name: "x"}}
	got := eventAttrs(q, trackEvent(7, "down"))
	want := []any{"sink", "x", "kind", "track-down", "op", 101, "target", "10.100.1.11", "track_id", 7}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attr %d = %v, want %v (all %v)", i, got[i], want[i], got)
		}
	}
}

// A redirect is a failed delivery, not a GET that "succeeds" (B18).
func TestWebhookRedirectFails(t *testing.T) {
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	s := NewWebhookSink(config.Action{On: []string{"track-down"}, Webhook: srv.URL + "/hook"}, 1, quiet())
	s.delays = nil
	err := s.Deliver(context.Background(), trackEvent(1, "down"))
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("err = %v", err)
	}
	if gets.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

// A transport error does not carry the URL's query (B1).
func TestWebhookErrorHidesQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/hook?token=SECRETTOKEN"
	srv.Close() // connection refused from now on
	s := NewWebhookSink(config.Action{On: []string{"track-down"}, Webhook: u}, 1, quiet())
	err := s.post(context.Background(), []byte("{}"))
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("err = %v", err)
	}
}

// A failing program's stderr is in the error, on one line (B20).
func TestExecStderrInError(t *testing.T) {
	s := NewExecSink(config.Action{On: []string{"track-up"}, Exec: writeScript(t, "echo 'bad\tthing' >&2\necho second >&2\nexit 3\n")}, 1, quiet())
	err := s.Deliver(context.Background(), trackEvent(1, "up"))
	if err == nil || !strings.HasSuffix(err.Error(), "exit status 3: stderr: bad thing second") {
		t.Errorf("err = %v", err)
	}
	if got := execExcerpt([]byte(strings.Repeat("x", 300))); len(got) != 256+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("excerpt of 300 bytes: %d bytes", len(got))
	}
}

// The webhook and exec sink names carry the action number and no secret of
// the URL (B1, and the names are unique per action).
func TestSinkNames(t *testing.T) {
	w := NewWebhookSink(config.Action{Webhook: "https://user:pw@hooks.example:8443/services/T0/PATHTOKEN?token=SECRET#frag"}, 3, quiet())
	if got := w.Name(); got != "webhook#3 https://hooks.example:8443" { // nothing after the host (R1)
		t.Errorf("webhook name %q", got)
	}
	e := NewExecSink(config.Action{Exec: "/usr/local/bin/notify"}, 4, quiet())
	if got := e.Name(); got != "exec#4 /usr/local/bin/notify" {
		t.Errorf("exec name %q", got)
	}
	// The same URL in two actions: two names.
	a := config.Action{Webhook: "http://h/x"}
	if NewWebhookSink(a, 1, quiet()).Name() == NewWebhookSink(a, 2, quiet()).Name() {
		t.Error("two actions, one name")
	}
}

// No log line, error or Stats name of a failing webhook carries its
// secrets, wherever they are in the URL: the path (Slack-style
// /services/TOKEN), the userinfo or the query (B1, R1). Both a refused
// connection and a non-2xx answer are tried.
func TestWebhookFailureHidesToken(t *testing.T) {
	secrets := []string{"PATHTOKEN", "s3cretpw", "QUERYTOKEN"}
	const path = "/services/T0/PATHTOKEN?token=QUERYTOKEN"
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	refused := httptest.NewServer(http.NotFoundHandler())
	refused.Close()
	for name, base := range map[string]string{"refused": refused.URL, "404": notFound.URL} {
		t.Run(name, func(t *testing.T) {
			u := "http://user:s3cretpw@" + strings.TrimPrefix(base, "http://") + path
			var logs syncBuffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			b := NewBus(logger)
			s := NewWebhookSink(config.Action{On: []string{"track-down"}, Webhook: u}, 1, logger)
			s.delays = []time.Duration{time.Millisecond}
			b.Subscribe(s)
			runBus(t, b)
			b.Publish(trackEvent(1, "down"))
			waitFor(t, "the failure", func() bool { return b.Stats().Sinks[0].Failed == 1 })
			err := s.Deliver(context.Background(), trackEvent(1, "down")) // the error itself
			all := strings.Join(logs.lines(""), "\n")
			for _, secret := range secrets {
				if strings.Contains(all, secret) {
					t.Errorf("%s in the logs:\n%s", secret, all)
				}
				if err == nil || strings.Contains(err.Error(), secret) {
					t.Errorf("%s in the error %v", secret, err)
				}
				for _, st := range b.Stats().Sinks {
					if strings.Contains(st.Name, secret) {
						t.Errorf("%s in the sink name %q", secret, st.Name)
					}
				}
			}
			if !strings.Contains(all, "event delivery failed") || !strings.Contains(all, "sink=\"webhook#1 http://127.0.0.1:") {
				t.Errorf("logs:\n%s", all)
			}
		})
	}
}
