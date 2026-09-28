//declscope:namespace event

package event

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"log/syslog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"goipsla/internal/clock"
	"goipsla/internal/config"
)

// ---- log ----

// LogSink writes every event to slog at info level. The attribute names are
// the JSON names of Event, except "op" (op_id: the log convention for an
// operation ID) and "event_time" (time: the log line has its own time);
// empty optional fields are left out.
type LogSink struct{ logger *slog.Logger }

// NewLogSink returns a log sink; a nil logger means slog.Default().
func NewLogSink(logger *slog.Logger) *LogSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogSink{logger: logger}
}

// Name implements Sink.
func (*LogSink) Name() string { return "log" }

// Deliver implements Sink.
func (s *LogSink) Deliver(ctx context.Context, ev Event) error {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "event", ev.Attrs()...)
	return nil
}

// Attrs returns the fields of ev as slog attributes named like the JSON
// fields (but "op" and "event_time", see LogSink), leaving out empty
// optional ones.
func (ev Event) Attrs() []slog.Attr {
	a := []slog.Attr{
		slog.Time("event_time", ev.Time),
		slog.String("kind", string(ev.Kind)),
		slog.Int("op", ev.OpID),
		slog.String("type", string(ev.Type)),
		slog.String("target", ev.Target),
	}
	str := func(k, v string) {
		if v != "" {
			a = append(a, slog.String(k, v))
		}
	}
	num := func(k string, v int64) {
		if v != 0 {
			a = append(a, slog.Int64(k, v))
		}
	}
	str("tag", ev.Tag)
	str("vrf", ev.VRF)
	str("element", ev.Element)
	str("threshold_type", ev.ThresholdType)
	num("value", ev.Value)
	num("upper", int64(ev.Upper))
	num("lower", int64(ev.Lower))
	str("action", ev.Action)
	num("reaction_index", int64(ev.ReactionIndex))
	num("track_id", int64(ev.TrackID))
	str("track_mode", ev.TrackMode)
	str("track_state", ev.TrackState)
	str("latest_rc", ev.LatestRC)
	if ev.LatestRTTMs != nil {
		a = append(a, slog.Float64("latest_rtt_ms", *ev.LatestRTTMs))
	}
	return append(a, slog.String("message", ev.Message))
}

// ---- syslog ----

var facilities = map[string]syslog.Priority{
	"kern": syslog.LOG_KERN, "user": syslog.LOG_USER, "mail": syslog.LOG_MAIL, "daemon": syslog.LOG_DAEMON,
	"auth": syslog.LOG_AUTH, "syslog": syslog.LOG_SYSLOG, "lpr": syslog.LOG_LPR, "news": syslog.LOG_NEWS,
	"uucp": syslog.LOG_UUCP, "cron": syslog.LOG_CRON, "authpriv": syslog.LOG_AUTHPRIV, "ftp": syslog.LOG_FTP,
	"local0": syslog.LOG_LOCAL0, "local1": syslog.LOG_LOCAL1, "local2": syslog.LOG_LOCAL2, "local3": syslog.LOG_LOCAL3,
	"local4": syslog.LOG_LOCAL4, "local5": syslog.LOG_LOCAL5, "local6": syslog.LOG_LOCAL6, "local7": syslog.LOG_LOCAL7,
}

// SyslogSink sends Message to the local syslog (/dev/log) with severity info
// and tag "goipslad". threshold-* events go out only when the reaction's
// action is syslog or trap-and-syslog (Cisco's action type); track-* events
// always do.
type SyslogSink struct {
	priority syslog.Priority
	logger   *slog.Logger
	network  string // "" and "" mean the local syslog; tests set a socket
	raddr    string

	mu sync.Mutex
	w  *syslog.Writer
}

// NewSyslogSink returns a sink for facility (a config facility name). A
// failure to connect is only logged; Deliver connects again.
func NewSyslogSink(facility string, logger *slog.Logger) (*SyslogSink, error) {
	return newSyslogSink(facility, "", "", logger)
}

// newSyslogSink connects to network/raddr; "" and "" is the local syslog.
func newSyslogSink(facility, network, raddr string, logger *slog.Logger) (*SyslogSink, error) {
	f, ok := facilities[facility]
	if !ok {
		return nil, fmt.Errorf("unknown syslog facility %q", facility)
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &SyslogSink{priority: f | syslog.LOG_INFO, logger: logger, network: network, raddr: raddr}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.connectLocked(); err != nil {
		logger.Warn("syslog not reachable; will retry on the next event", "facility", facility, "err", err)
	}
	return s, nil
}

func (s *SyslogSink) connectLocked() error {
	w, err := syslog.Dial(s.network, s.raddr, s.priority, "goipslad")
	if err != nil {
		return err
	}
	s.w = w
	return nil
}

// Name implements Sink.
func (*SyslogSink) Name() string { return "syslog" }

// Wants reports whether the sink sends ev.
func (*SyslogSink) Wants(ev Event) bool {
	if ev.Kind.IsThreshold() {
		return ev.Action == config.ActionSyslog || ev.Action == config.ActionTrapAndSyslog
	}
	return true
}

// Deliver implements Sink.
func (s *SyslogSink) Deliver(_ context.Context, ev Event) error {
	if !s.Wants(ev) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		if err := s.connectLocked(); err != nil {
			return fmt.Errorf("syslog: %w", err)
		}
	}
	if err := s.w.Info(ev.Message); err != nil {
		// The writer reconnects once by itself; drop it so that the next
		// event dials afresh.
		s.w.Close()
		s.w = nil
		return fmt.Errorf("syslog: %w", err)
	}
	return nil
}

// Close closes the connection.
func (s *SyslogSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return nil
	}
	err := s.w.Close()
	s.w = nil
	return err
}

// ---- action filter ----

// actionFilter selects the events of one config.Action: the kind must be in
// On, and for track-* events Track (if set) must match. Track does not
// restrict threshold-* events, which belong to no track.
type actionFilter struct {
	on    map[Kind]bool
	track int
}

func newActionFilter(a config.Action) actionFilter {
	f := actionFilter{on: map[Kind]bool{}, track: a.Track}
	for _, k := range a.On {
		f.on[Kind(k)] = true
	}
	return f
}

func (f actionFilter) wants(ev Event) bool {
	if !f.on[ev.Kind] {
		return false
	}
	return !ev.Kind.IsTrack() || f.track == 0 || ev.TrackID == f.track
}

// ---- webhook ----

// Webhook timing.
const (
	webhookTimeout = 10 * time.Second
)

// webhookRetryDelays are the waits before the 2nd, 3rd and 4th attempts.
var webhookRetryDelays = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

// WebhookSink POSTs the JSON of each event to the action's URL. A non-2xx
// status or a transport error is retried after 1 s, 4 s and 16 s.
type WebhookSink struct {
	url    string
	name   string // "webhook#N scheme://host": no path, userinfo, query or fragment
	filter actionFilter
	client *http.Client
	delays []time.Duration
	clk    clock.Clock
	logger *slog.Logger
}

// NewWebhookSink returns the sink of a (a.Webhook must be set), the index-th
// action of the configuration (1-based), which names the sink.
func NewWebhookSink(a config.Action, index int, logger *slog.Logger) *WebhookSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookSink{
		url:    a.Webhook,
		name:   webhookName(index, a.Webhook),
		filter: newActionFilter(a),
		client: &http.Client{
			Timeout: webhookTimeout,
			// A redirect is a failure: following it would turn the POST
			// into a GET without the event and count it as delivered.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		delays: webhookRetryDelays,
		clk:    clock.Real(),
		logger: logger,
	}
}

// Name implements Sink: "webhook#N scheme://host". Everything after the
// host (path, userinfo, query, fragment) is left out, as any of it may carry
// a token (Slack-style https://hooks.example/services/TOKEN): the name goes
// into the logs, the errors and the metrics labels. N keeps it unique.
func (s *WebhookSink) Name() string { return s.name }

// webhookName is the name of the webhook sink of the index-th action.
func webhookName(index int, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "webhook#" + strconv.Itoa(index)
	}
	return "webhook#" + strconv.Itoa(index) + " " + u.Scheme + "://" + u.Host
}

// Deliver implements Sink. Filtered-out events return nil.
func (s *WebhookSink) Deliver(ctx context.Context, ev Event) error {
	if !s.filter.wants(ev) {
		return nil
	}
	body, err := ev.JSON()
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; ; attempt++ {
		if last = s.post(ctx, body); last == nil {
			return nil
		}
		if attempt >= len(s.delays) || ctx.Err() != nil {
			return fmt.Errorf("%s: %w (after %d attempts)", s.name, last, attempt+1)
		}
		s.logger.Debug("webhook failed; retrying", "sink", s.name, "attempt", attempt+1, "err", last, "retry_in", s.delays[attempt].String())
		t := s.clk.NewTimer(s.delays[attempt])
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%s: %w", s.name, ctx.Err())
		case <-t.C():
		}
	}
}

func (s *WebhookSink) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "goipslad")
	resp, err := s.client.Do(req)
	if err != nil {
		// *url.Error prints the whole URL, query tokens included: keep only
		// what went wrong.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s: %w", strings.ToLower(ue.Op), ue.Err)
		}
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // drain for connection reuse
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

// ---- exec ----

// Exec limits.
const (
	execTimeout    = 30 * time.Second
	execMaxRunning = 4 // across every exec sink
	execOutputMax  = 4 << 10
)

// execSlots limits the programs running at once, over all exec sinks.
var execSlots = make(chan struct{}, execMaxRunning)

// ExecSink runs the action's program for each event, with the event JSON on
// standard input and GOIPSLA_* variables in the environment. It is killed
// after 30 s; a non-zero exit is a failure (not retried).
type ExecSink struct {
	path    string
	name    string // "exec#N path"
	filter  actionFilter
	timeout time.Duration
	logger  *slog.Logger
}

// NewExecSink returns the sink of a (a.Exec must be set), the index-th
// action of the configuration (1-based), which names the sink.
func NewExecSink(a config.Action, index int, logger *slog.Logger) *ExecSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &ExecSink{path: a.Exec, name: "exec#" + strconv.Itoa(index) + " " + a.Exec, filter: newActionFilter(a), timeout: execTimeout, logger: logger}
}

// Name implements Sink: "exec#N path".
func (s *ExecSink) Name() string { return s.name }

// Env returns the GOIPSLA_* variables for ev.
func (ev Event) Env() []string {
	v := func(k, val string) string { return k + "=" + val }
	return []string{
		v("GOIPSLA_EVENT_KIND", string(ev.Kind)),
		v("GOIPSLA_OP_ID", strconv.Itoa(ev.OpID)),
		v("GOIPSLA_TARGET", ev.Target),
		v("GOIPSLA_TAG", ev.Tag),
		v("GOIPSLA_ELEMENT", ev.Element),
		v("GOIPSLA_VALUE", strconv.FormatInt(ev.Value, 10)),
		v("GOIPSLA_TRACK_ID", strconv.Itoa(ev.TrackID)),
		v("GOIPSLA_TRACK_STATE", ev.TrackState),
	}
}

// killGroup kills the process group of cmd (its pid, from Setpgid). A group
// with no member left is not an error.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

// Deliver implements Sink. Filtered-out events return nil.
func (s *ExecSink) Deliver(ctx context.Context, ev Event) error {
	if !s.filter.wants(ev) {
		return nil
	}
	body, err := ev.JSON()
	if err != nil {
		return err
	}
	select {
	case execSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-execSlots }()

	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.path)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), ev.Env()...)
	cmd.Stdin = bytes.NewReader(append(body, '\n'))
	stdout := &limitedBuffer{max: execOutputMax}
	stderr := &limitedBuffer{max: execOutputMax}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second // do not hang on pipes held by grandchildren
	// The program runs in its own process group so that the timeout, and the
	// limit of execMaxRunning, cover what it starts: on the deadline the
	// whole group is killed, and so is whatever is left of it afterwards.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	err = cmd.Run()
	_ = killGroup(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // exited 0; a background child held the output pipes until killed
	}
	s.logger.Debug("exec finished", "sink", s.name, "kind", string(ev.Kind), "op", ev.OpID,
		"stdout", stdout.buf.String(), "stderr", stderr.buf.String(), "err", err)
	if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("%s: killed after %s", s.Name(), s.timeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := execExcerpt(stderr.buf.Bytes()); msg != "" {
			return fmt.Errorf("%s: %s: stderr: %s", s.Name(), ee.ProcessState, msg)
		}
		return fmt.Errorf("%s: %s", s.Name(), ee.ProcessState)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}

// execStderrExcerpt is how much of a failed program's stderr goes into the
// error (and so into the "event delivery failed" warning).
const execStderrExcerpt = 256

// execExcerpt is the start of a program's stderr on one line: at most 256
// bytes, control characters dropped.
func execExcerpt(b []byte) string {
	b = bytes.TrimSpace(b)
	cut := len(b) > execStderrExcerpt
	if cut {
		b = b[:execStderrExcerpt]
	}
	s := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7F || r == utf8.RuneError:
			return -1
		}
		return r
	}, string(b))
	if cut {
		s += "..."
	}
	return s
}
