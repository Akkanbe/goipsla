// main.go is goipslad itself, the package's API as a command: its flags,
// the order it starts and stops its parts, its signals and its result log.
// The other files are parts it wires (provider.go, view.go, sink.go).
//
//declscope:core

// Command goipslad is the IP SLA (ICMP) measurement daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"goipsla/internal/api"
	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/manager"
	"goipsla/internal/metrics"
	"goipsla/internal/op"
	"goipsla/internal/probe"
	"goipsla/internal/react"
	"goipsla/internal/snmp"
	"goipsla/internal/stats"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const defaultConfigPath = "/etc/goipslad/config.yaml"

// newEngine builds the ICMP engine; tests replace it, since the real one
// needs CAP_NET_RAW.
var newEngine = probe.New

// options holds the command-line flags.
type options struct {
	configPath  string
	logFormat   string
	logLevel    string
	logResults  bool
	showVersion bool

	// logFormatSet and logLevelSet report whether the flag was given
	// explicitly. An explicit flag takes precedence over global.log in the
	// configuration file.
	logFormatSet bool
	logLevelSet  bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "goipslad: %v\n", err)
		return 2
	}
	if opts.showVersion {
		fmt.Fprintf(stdout, "goipslad %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		printConfigError(stderr, opts.configPath, err)
		return 1
	}

	logger, err := newLogger(stderr, logSetting(opts.logFormat, opts.logFormatSet, cfg.Global.Log.Format),
		logSetting(opts.logLevel, opts.logLevelSet, cfg.Global.Log.Level))
	if err != nil {
		fmt.Fprintf(stderr, "goipslad: %v\n", err)
		return 2
	}
	slog.SetDefault(logger)

	startedAt := time.Now()
	logger.Info("goipslad starting",
		"version", version,
		"go", runtime.Version(),
		"pid", os.Getpid(),
		"config", opts.configPath)
	// Settings that are accepted but have no effect (config.Config.Warnings).
	for _, w := range cfg.Warnings {
		logger.Warn("config warning", "warning", w)
	}

	// fatal reports a startup failure after the logger exists: on stderr
	// (one line, for systemctl status) and as an error log.
	fatal := func(msg string, err error) int {
		fmt.Fprintf(stderr, "goipslad: %s: %v\n", msg, err)
		logger.Error(msg, "err", err)
		return 1
	}

	clk := clock.Real()
	store := stats.NewStore(clk)

	// Events: the bus with its sinks, the reaction engine and the tracker.
	// The sinks are subscribed below, once the provider that counts their
	// deliveries exists; nothing is published before the manager runs.
	bus := event.NewBus(logger)
	reactions := react.NewEngine(clk, bus, logger)
	tracker := react.NewTracker(clk, bus, logger)

	// The engine reports late replies to the manager, which needs the engine
	// to be built: close the loop through a pointer set once both exist.
	var mgrRef atomic.Pointer[manager.Manager]
	eng, err := newEngine(probe.Options{
		Logger: logger,
		OnLateReply: func(opID, seq uint32, sentAt time.Time) {
			if m := mgrRef.Load(); m != nil {
				m.LateReply(opID, seq, sentAt)
			}
		},
	})
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			return fatal("cannot start the ICMP engine (need CAP_NET_RAW)", err)
		}
		return fatal("cannot start the ICMP engine", err)
	}
	// Targets follow reloads, so the result log asks the manager.
	target := func(id int) string {
		if m := mgrRef.Load(); m != nil {
			if c, ok := m.Config(id); ok {
				return c.Target.String()
			}
		}
		return ""
	}
	// Every result goes to the statistics, then the reactions and the
	// tracks (which ignore busy and sequenceError), then the log.
	observe := func(r op.Result) {
		store.Record(r)
		reactions.Observe(r)
		tracker.Observe(r)
	}
	mgr := manager.New(cfg, eng, clk, newResultSink(logger, target, observe, opts.logResults, clk.Now), logger)
	mgr.SetStore(store)
	mgr.SetReactions(reactions, tracker)
	mgrRef.Store(mgr)
	n := mgr.Counts()
	logger.Info("configuration loaded", "operations", n.Total, "skipped", n.Skipped)

	// Register every operation and its statistics row before the API is
	// published: a client must never see /v1/health count an operation
	// that /v1/operations does not list.
	mgr.Start()
	prov := newProvider(version, store, mgr, opts.configPath, startedAt, logger)
	prov.SetEvents(bus, reactions, tracker)
	prov.SetProbe(eng)
	snmpOpts := snmp.Options{Version: version, Start: startedAt, Logger: logger}
	syslogSink, err := subscribeSinks(bus, prov.CountSink, cfg, logger)
	if err != nil {
		eng.Close()
		return fatal("cannot start the event sinks", err)
	}
	if cfg.Global.SNMP != nil {
		// rttMonNotificationV2 for the reactions whose action is trap or
		// trap-and-syslog; the sink filters the rest.
		bus.Subscribe(prov.CountSink(snmp.NewTrapSink(cfg.Global.SNMP, prov.SNMPView(), snmpOpts)))
	}
	// global.api-socket-group, if set, becomes the group of the socket so
	// that its members can run goipsla (mode 0660).
	srv, err := api.NewServer(cfg.Global.APISocket, apiSocketMode, cfg.Global.APISocketGroup, prov, logger)
	if err != nil {
		eng.Close()
		return fatal("cannot start the API", err)
	}
	logger.Info("api listening", "socket", srv.Addr())

	// The long-running parts, in start order. Each runs until its context is
	// canceled; one that returns before the daemon stops is a failure,
	// except the metrics exporter without metrics-listen, which returns nil
	// at once.
	busPart := &part{name: "event bus", run: bus.Run}
	parts := []*part{
		busPart,
		{name: "tracker", run: tracker.Run},
		{name: "api server", run: srv.Serve, after: func() { srv.Close() }},
		{name: "metrics exporter", run: func(ctx context.Context) error {
			return metrics.Serve(ctx, cfg.Global.MetricsListen, prov.MetricsView(), logger)
		}, nilReturnOK: cfg.Global.MetricsListen == ""},
	}
	if cfg.Global.SNMP != nil {
		parts = append(parts, &part{name: "snmp subagent", run: func(ctx context.Context) error {
			return snmp.Run(ctx, cfg.Global.SNMP, prov.SNMPView(), snmpOpts)
		}})
	}
	parts = append(parts, &part{name: "manager", run: mgr.Run})
	// The bus delivers what is queued (up to busDrainTimeout) before it
	// stops, and the syslog connection closes after it.
	busPart.before = func() { warnUndrained(logger, bus, busDrainTimeout) }
	busPart.after = func() {
		if syslogSink != nil {
			syslogSink.Close()
		}
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// SIGHUP reloads on its own goroutine (coalesce), so that a stop signal
	// is never kept waiting behind a reload; the provider logs the reload.
	go handleSignals(sigs, cancel, coalesce(ctx, prov.ReloadOnSignal), logger, os.Exit)

	exited := make(chan *part, len(parts))
	for _, p := range parts {
		p.start(exited)
	}
	logStarted(logger, cfg, srv.Addr(), mgr.Counts(), opts.logResults)

	// Wait for a signal, or for a part that stops by itself.
	var failed *part
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case p := <-exited:
			if p.err == nil && p.nilReturnOK {
				continue
			}
			if p.err == nil {
				p.err = errors.New("stopped unexpectedly")
			}
			failed = p
			break wait
		}
	}
	stopping := time.Now()
	if failed != nil {
		logger.Error(failed.name+" failed; shutting down", "err", failed.err)
	}

	// Stop order: the API first (no request sees a half-stopped daemon),
	// then the metrics exporter and the SNMP subagent, then the operations,
	// then the tracker and the event bus (which delivers what is queued),
	// then the engine the operations used.
	status := 0
	if failed != nil {
		status = 1
	}
	for _, name := range []string{"api server", "metrics exporter", "snmp subagent", "manager", "tracker", "event bus"} {
		for _, p := range parts {
			if p.name != name {
				continue
			}
			if err := p.stop(logger); err != nil && p != failed {
				logger.Error(p.name+" stopped with an error", "err", err)
				status = 1
			}
		}
	}
	t := time.Now()
	logger.Info("stopping ICMP engine")
	if err := eng.Close(); err != nil {
		logger.Warn("closing the ICMP engine", "err", err)
	}
	logger.Info("ICMP engine stopped", "elapsed", time.Since(t).Round(time.Millisecond).String())
	logger.Info("goipslad stopped", "elapsed", time.Since(stopping).Round(time.Millisecond).String(), "status", status)
	return status
}

// part is one long-running part of the daemon: run blocks until its
// context is canceled (or it fails).
type part struct {
	name string
	run  func(ctx context.Context) error
	// nilReturnOK: returning nil before the daemon stops is normal (the
	// metrics exporter when metrics-listen is empty).
	nilReturnOK bool
	before      func() // at stop, before its context is canceled
	after       func() // at stop, after run returned

	cancel context.CancelFunc
	done   chan struct{}
	err    error // valid once done is closed
}

// start runs the part on its own goroutine; exited receives it when run
// returns.
func (p *part) start(exited chan<- *part) {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		p.err = p.run(ctx)
		close(p.done)
		exited <- p
	}()
}

// stop stops the part, logging "stopping <name>" and "<name> stopped" with
// the time it took, and returns the error run returned.
func (p *part) stop(logger *slog.Logger) error {
	t := time.Now()
	logger.Info("stopping " + p.name)
	if p.before != nil {
		p.before()
	}
	p.cancel()
	<-p.done
	if p.after != nil {
		p.after()
	}
	logger.Info(p.name+" stopped", "elapsed", time.Since(t).Round(time.Millisecond).String())
	return p.err
}

// logStarted logs the startup summary: what the daemon listens on, where
// its events go and how its operations stand.
func logStarted(logger *slog.Logger, cfg *config.Config, apiSocket string, n manager.Counts, logResults bool) {
	agentx, traps := "", 0
	if cfg.Global.SNMP != nil {
		agentx, traps = cfg.Global.SNMP.AgentX, len(cfg.Global.SNMP.Traps)
	}
	facility := ""
	if cfg.Global.Syslog != nil {
		facility = cfg.Global.Syslog.Facility
	}
	webhooks, execs := 0, 0
	for _, a := range cfg.Actions {
		if a.Webhook != "" {
			webhooks++
		}
		if a.Exec != "" {
			execs++
		}
	}
	logger.Info("goipslad started",
		"api_socket", apiSocket,
		"api_socket_group", cfg.Global.APISocketGroup,
		"metrics_listen", cfg.Global.MetricsListen,
		"snmp_agentx", agentx,
		"trap_targets", traps,
		"syslog_facility", facility,
		"webhook_sinks", webhooks,
		"exec_sinks", execs,
		"operations_active", n.Active,
		"operations_pending", n.Pending,
		"operations_inactive", n.Inactive,
		"log_results", logResults)
}

// subscribeSinks subscribes the event sinks, each wrapped by count (the
// provider counts their deliveries): the log always, syslog when global.syslog is set,
// and one webhook or exec sink per action target. The sinks are fixed at
// startup: a reload that changes actions or syslog asks for a restart.
func subscribeSinks(bus *event.Bus, count func(event.Sink) event.Sink, cfg *config.Config, logger *slog.Logger) (*event.SyslogSink, error) {
	bus.Subscribe(count(event.NewLogSink(logger)))
	var sl *event.SyslogSink
	if cfg.Global.Syslog != nil {
		s, err := event.NewSyslogSink(cfg.Global.Syslog.Facility, logger)
		if err != nil {
			return nil, fmt.Errorf("syslog: %w", err)
		}
		sl = s
		bus.Subscribe(count(s))
	}
	for i, a := range cfg.Actions {
		// The sink names are webhook#N / exec#N (N: the action's position,
		// from 1), so that two actions to the same place stay apart and a
		// webhook URL's userinfo and query never reach a log or a label.
		if a.Webhook != "" {
			bus.Subscribe(count(event.NewWebhookSink(a, i+1, logger)))
		}
		if a.Exec != "" {
			bus.Subscribe(count(event.NewExecSink(a, i+1, logger)))
		}
	}
	return sl, nil
}

// busDrainTimeout bounds how long shutdown waits for queued events.
const busDrainTimeout = 5 * time.Second

// drainBus waits until every sink has handled every published event
// (delivered, failed or dropped), or until timeout. It reports whether the
// bus drained.
func drainBus(bus *event.Bus, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		st := bus.Stats()
		want := st.Published - st.Dropped
		done := true
		for _, s := range st.Sinks {
			if s.Delivered+s.Failed+s.Dropped < want {
				done = false
				break
			}
		}
		if done {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// warnUndrained waits for the bus to drain (drainBus) and, if it does not
// within timeout, warns with the events each sink has not handled.
func warnUndrained(logger *slog.Logger, bus *event.Bus, timeout time.Duration) {
	if drainBus(bus, timeout) {
		return
	}
	st := bus.Stats()
	want := st.Published - st.Dropped
	var sinks []string
	var most uint64
	for _, s := range st.Sinks {
		if done := s.Delivered + s.Failed + s.Dropped; done < want {
			sinks = append(sinks, s.Name)
			most = max(most, want-done)
		}
	}
	logger.Warn("event bus not drained; queued events are lost", "timeout", timeout.String(), "undelivered", most, "sinks", sinks)
}

// apiSocketMode is the permission of the API socket file.
const apiSocketMode = 0o660

// printConfigError reports a configuration error, one validation error per
// line.
func printConfigError(w io.Writer, path string, err error) {
	var verr *config.ValidationError
	if errors.As(err, &verr) && len(verr.Errors) > 0 {
		fmt.Fprintf(w, "goipslad: invalid configuration %s:\n", path)
		for _, fe := range verr.Errors {
			if fe.Path != "" {
				fmt.Fprintf(w, "  %s: %s\n", fe.Path, fe.Msg)
			} else {
				fmt.Fprintf(w, "  %s\n", fe.Msg)
			}
		}
		return
	}
	fmt.Fprintf(w, "goipslad: %v\n", err)
}

// logSetting picks a log setting: an explicit flag wins over the
// configuration file, which wins over the flag's default.
func logSetting(flagValue string, flagSet bool, fileValue string) string {
	if flagSet || fileValue == "" {
		return flagValue
	}
	return fileValue
}

// newResultSink returns the sink of the results of attempts: it hands every
// result to observe (statistics, reactions, tracks) and then logs it with
// resultLog.
func newResultSink(logger *slog.Logger, target func(id int) string, observe func(op.Result), logAll bool, now func() time.Time) func(op.Result) {
	rl := &resultLog{logger: logger, target: target, all: logAll, now: now, ops: map[int]*resultState{}}
	return func(r op.Result) {
		if observe != nil {
			observe(r)
		}
		rl.log(r)
	}
}

// resultRepeatInterval is how often a result that keeps repeating is
// summarized at info level.
const resultRepeatInterval = 60 * time.Second

// resultLog logs results without flooding the log when many operations fail
// the same way for a long time (1,000 operations at 1 s against a dead
// segment would otherwise log 1,000 lines a second):
//
//   - with --log-results, every result at info ("result");
//   - otherwise the first attempt result of each life of an operation is
//     logged at info ("result first", with life), whatever its code, so
//     that a healthy operation shows up at least once per life;
//   - later, an attempt's result is logged at info when its return code
//     differs from the previous one ("result changed", with from);
//   - the same return code again is logged at debug, and a non-ok code that
//     keeps repeating is summarized at info every 60 s ("result repeated",
//     with count); a repeated ok is not summarized (--log-results shows it);
//   - busy and sequenceError, which are not attempts, are logged at info at
//     most once per 60 s per operation and code, with the number of those
//     logged at debug meanwhile (suppressed).
//
// The state of an operation is discarded when a new life starts: its
// Result.Life changes (restart, reset, a reload that rebuilds it), or an
// attempt's Seq does not grow (removal and re-creation by a reload, whose
// life index starts again at 1).
//
// The sink is serialized by the manager, so resultLog needs no lock.
type resultLog struct {
	logger *slog.Logger
	target func(id int) string
	all    bool
	now    func() time.Time
	ops    map[int]*resultState
}

type resultState struct {
	life   int    // Result.Life of the results this state is about
	seq    uint32 // Seq of the last attempt result
	seen   bool
	rc     op.ReturnCode
	repeat int       // repeats of rc not summarized yet
	since  time.Time // when rc was last logged at info
	extra  map[op.ReturnCode]*extraState
}

type extraState struct {
	last       time.Time
	suppressed int
}

func (rl *resultLog) attrs(r op.Result) []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("op", r.OpID),
		slog.String("type", string(r.Type)),
		slog.String("target", rl.target(r.OpID)),
		slog.Uint64("seq", uint64(r.Seq)),
		slog.String("rc", r.Code.String()),
	}
	if r.Code.HasRTT() {
		attrs = append(attrs, slog.Float64("rtt_ms", op.RTTMillis(r.RTT)))
	}
	if r.Detail != "" {
		attrs = append(attrs, slog.String("detail", r.Detail))
	}
	return attrs
}

func (rl *resultLog) emit(level slog.Level, msg string, attrs ...slog.Attr) {
	rl.logger.LogAttrs(context.Background(), level, msg, attrs...)
}

func (rl *resultLog) log(r op.Result) {
	if rl.all {
		rl.emit(slog.LevelInfo, "result", rl.attrs(r)...)
		return
	}
	attempt := r.Code != op.RCBusy && r.Code != op.RCSequenceError
	st := rl.ops[r.OpID]
	if st == nil || st.life != r.Life || (attempt && r.Seq <= st.seq) {
		st = &resultState{life: r.Life}
		rl.ops[r.OpID] = st
	}
	now := rl.now()
	if !attempt {
		if st.extra == nil {
			st.extra = map[op.ReturnCode]*extraState{}
		}
		x := st.extra[r.Code]
		if x == nil {
			x = &extraState{}
			st.extra[r.Code] = x
		}
		if !x.last.IsZero() && now.Sub(x.last) < resultRepeatInterval {
			x.suppressed++
			rl.emit(slog.LevelDebug, "result", rl.attrs(r)...)
			return
		}
		attrs := rl.attrs(r)
		if x.suppressed > 0 {
			attrs = append(attrs, slog.Int("suppressed", x.suppressed))
		}
		rl.emit(slog.LevelInfo, "result", attrs...)
		x.last, x.suppressed = now, 0
		return
	}
	st.seq = r.Seq
	switch {
	case !st.seen:
		rl.emit(slog.LevelInfo, "result first", append(rl.attrs(r), slog.Int("life", r.Life))...)
	case st.rc != r.Code:
		if st.repeat > 0 && st.rc != op.RCOK {
			rl.emit(slog.LevelInfo, "result repeated", slog.Int("op", r.OpID), slog.String("target", rl.target(r.OpID)),
				slog.String("rc", st.rc.String()), slog.Int("count", st.repeat))
		}
		rl.emit(slog.LevelInfo, "result changed", append(rl.attrs(r), slog.String("from", st.rc.String()))...)
	default:
		st.repeat++
		rl.emit(slog.LevelDebug, "result", rl.attrs(r)...)
		if r.Code != op.RCOK && now.Sub(st.since) >= resultRepeatInterval {
			rl.emit(slog.LevelInfo, "result repeated", slog.Int("op", r.OpID), slog.String("target", rl.target(r.OpID)),
				slog.String("rc", r.Code.String()), slog.Int("count", st.repeat))
			st.repeat, st.since = 0, now
		}
		return
	}
	st.seen, st.rc, st.repeat, st.since = true, r.Code, 0, now
}

// handleSignals cancels the daemon's context on SIGINT or SIGTERM and asks
// for a reload on SIGHUP. It never waits for the reload itself (reload only
// queues it, see coalesce), so a stop signal takes effect at once even while
// a reload runs. A second SIGINT or SIGTERM while the daemon is stopping
// forces the exit (status 1), for an operator whose shutdown hangs on a slow
// sink.
func handleSignals(sigs <-chan os.Signal, cancel context.CancelFunc, reload func() bool, logger *slog.Logger, exit func(int)) {
	stopping := false
	for sig := range sigs {
		switch {
		case sig == syscall.SIGHUP && !stopping:
			if reload() {
				logger.Info("reload requested", "signal", sig.String())
			} else {
				logger.Info("reload requested while one is already pending; merged into it", "signal", sig.String())
			}
		case sig == syscall.SIGHUP:
			logger.Warn("ignoring SIGHUP while stopping", "signal", sig.String())
		case !stopping:
			logger.Info("shutting down", "signal", sig.String())
			stopping = true
			cancel()
		default:
			logger.Error("forcing exit", "signal", sig.String())
			exit(1)
			return
		}
	}
}

// coalesce runs f on its own goroutine, once per request, merging requests:
// the returned function asks for a run and reports whether the request was
// queued (false: one was already waiting, and this one is merged into it).
// A request made while f runs makes f run once more after it, however many
// arrive meanwhile. Nothing runs after ctx is done.
func coalesce(ctx context.Context, f func()) func() bool {
	reqs := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reqs:
				if ctx.Err() == nil {
					f()
				}
			}
		}
	}()
	return func() bool {
		select {
		case reqs <- struct{}{}:
			return true
		default:
			return false
		}
	}
}

func parseFlags(args []string, stderr io.Writer) (*options, error) {
	opts := &options{}
	fs := flag.NewFlagSet("goipslad", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: goipslad [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.configPath, "config", defaultConfigPath, "path of the configuration `file`")
	fs.StringVar(&opts.logFormat, "log-format", "text", "log `format`: text or json")
	fs.StringVar(&opts.logLevel, "log-level", "info", "log `level`: debug, info, warn or error")
	fs.BoolVar(&opts.logResults, "log-results", false, "log every result at info level (by default ok results are logged at debug)")
	fs.BoolVar(&opts.showVersion, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "log-format":
			opts.logFormatSet = true
		case "log-level":
			opts.logLevelSet = true
		}
	})
	return opts, nil
}

// newLogger returns a slog logger writing to w in the given format ("text"
// or "json") at the given level ("debug", "info", "warn" or "error").
func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q: must be debug, info, warn or error", level)
	}
	hopts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, hopts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, hopts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q: must be text or json", format)
	}
}
