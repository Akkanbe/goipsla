// Package metrics exposes the operations of goipslad to Prometheus on
// /metrics (docs/metrics.md).
//
// A single custom Collector builds every sample at scrape time from a Source,
// so operations added or removed by a reload appear and disappear without
// bookkeeping. Go runtime (go_*) and process (process_*) metrics are
// exported as well. goipslad serves it with Serve on global.metrics-listen,
// which is not reloadable.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/probe"
	"goipsla/internal/stats"
)

// Source provides the operations to export. The daemon's provider
// implements it.
type Source interface {
	Rows() []Row // sorted by ID

	// P5: tracks, reactions and event delivery counters.
	Tracks() []TrackRow       // sorted by track ID
	Reactions() []ReactionRow // sorted by operation ID, then configuration order
	EventStats() []EventCount
}

// Diagnostics are the daemon's diagnostic counters.
type Diagnostics struct {
	Probe            probe.Stats // the ICMP engine's (probe.Engine.Stats)
	ResultsDiscarded uint64      // results dropped as belonging to an earlier life (stats.Store.Discarded)
}

// DiagnosticsSource is implemented by a Source that also reports the
// diagnostic counters; without it the goipsla_probe_* and
// goipsla_results_discarded_total metrics are omitted.
type DiagnosticsSource interface {
	Diagnostics() Diagnostics
}

// TrackRow is one track ("track N ip sla OP state|reachability").
type TrackRow struct {
	ID        int
	Operation int
	Mode      string // state | reachability
	State     string // up | down | unknown
}

// ReactionRow is one reaction row of an operation.
type ReactionRow struct {
	OpID     int
	Element  string
	Occurred bool
}

// EventCount is how many events of one kind one sink handled with one
// result: delivered, failed or dropped (the sink's queue was full). Kind
// is empty when the counter does not tell kinds apart (dropped).
type EventCount struct {
	Kind   string
	Sink   string
	Result string
	Count  uint64
}

// trackStateValues are the goipsla_track_state values.
var trackStateValues = map[string]float64{"up": 1, "down": 0, "unknown": -1}

// Row is one operation.
type Row struct {
	ID     int
	Type   config.OpType
	Target string
	Tag    string
	VRF    string
	State  string // pending | inactive | active
	Latest stats.Latest
	Totals stats.Counters
	// LifeStart is the start of the current life. The zero value omits
	// goipsla_life_start_timestamp_seconds.
	LifeStart time.Time
	// TotalsJitter accumulates the bursts of the current life of an
	// icmp-jitter operation (stats.Snapshot.TotalsJitter). When nil, the
	// jitter packet counters are omitted.
	TotalsJitter *stats.JitterCounters
}

// States and their goipsla_operation_state values.
var stateValues = map[string]float64{"pending": 0, "inactive": 1, "active": 2}

var stateOrder = []string{"pending", "inactive", "active"}

// results lists the goipsla_results_total "result" labels in output order.
var results = []string{"ok", "overThreshold", "timeout", "busy", "dropped", "sequenceError", "verifyError"}

func resultCount(c stats.Counters, result string) uint64 {
	switch result {
	case "ok":
		return c.Successes()
	case "overThreshold":
		return c.OverThresholds
	case "timeout":
		return c.Timeouts
	case "busy":
		return c.Busies
	case "dropped":
		return c.Drops
	case "sequenceError":
		return c.SequenceErrors
	case "verifyError":
		return c.VerifyErrors
	}
	return 0
}

const ns = "goipsla"

var opLabels = []string{"id", "type", "target", "tag", "vrf"}

func desc(name, help string, extra ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(ns, "", name), help, append(append([]string{}, opLabels...), extra...), nil)
}

// Collector is the Prometheus collector for a Source.
type Collector struct {
	src Source
	clk clock.Clock

	info, state, latestRTT, latestRC, latestSuccess, latestEnd *prometheus.Desc
	attempts, completions, results, rttSum, rttMin, rttMax     *prometheus.Desc
	lifeStart, operations, scrapeDuration                      *prometheus.Desc

	jitterAvg, jitterLoss, jitterLate, jitterOOS, jitterSkipped, jitterOverTh *prometheus.Desc

	trackState, reactionOccurred, eventsTotal *prometheus.Desc

	probeDropped, probeLate, probeRecvErrors, resultsDiscarded *prometheus.Desc
}

// newCollector returns a collector reading src at every scrape. clk may be
// nil (real clock); it only times the scrape.
func newCollector(src Source, clk clock.Clock) *Collector {
	if clk == nil {
		clk = clock.Real()
	}
	return &Collector{
		src:           src,
		clk:           clk,
		info:          desc("operation_info", "Operation metadata; always 1.", "state"),
		state:         desc("operation_state", "Operational state: 0 pending, 1 inactive, 2 active."),
		latestRTT:     desc("latest_rtt_seconds", "Round-trip time of the latest attempt, when it completed (ok or overThreshold)."),
		latestRC:      desc("latest_return_code", "Return code of the latest attempt (RttResponseSense number); the rc label is its MIB name.", "rc"),
		latestSuccess: desc("latest_success", "1 if the latest attempt returned ok, else 0."),
		latestEnd:     desc("latest_end_timestamp_seconds", "Unix time the latest attempt completed."),
		attempts:      desc("attempts_total", "Attempts (initiations) in the current life."),
		completions:   desc("completions_total", "Completed attempts (ok and overThreshold) in the current life."),
		results:       desc("results_total", "Attempts by result in the current life; ok counts successes (completions without overThreshold).", "result"),
		rttSum:        desc("rtt_sum_seconds_total", "Sum of the RTTs of the completions in the current life, in seconds (each RTT truncated to whole milliseconds, as in the MIB)."),
		rttMin:        desc("rtt_min_seconds", "Smallest completion RTT in the current life, in seconds (truncated to whole milliseconds)."),
		rttMax:        desc("rtt_max_seconds", "Largest completion RTT in the current life, in seconds (truncated to whole milliseconds)."),
		lifeStart:     desc("life_start_timestamp_seconds", "Unix time the current life started."),
		jitterAvg: desc("jitter_avg_seconds",
			"icmp-jitter: mean absolute jitter of the latest burst, in seconds (whole milliseconds); direction sd, ds or both.", "direction"),
		jitterLoss:    desc("jitter_packet_loss_total", "icmp-jitter: packets without a reply within the timeout in the current life."),
		jitterLate:    desc("jitter_packets_late_total", "icmp-jitter: replies after their timeout but within the burst in the current life."),
		jitterOOS:     desc("jitter_packets_out_of_sequence_total", "icmp-jitter: packets out of sequence in the current life; direction sd, ds or both (exclusive).", "direction"),
		jitterSkipped: desc("jitter_packets_skipped_total", "icmp-jitter: packets not sent in the current life."),
		jitterOverTh:  desc("jitter_rtt_over_threshold_total", "icmp-jitter: packets whose RTT exceeded the threshold in the current life."),
		operations: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "operations"),
			"Number of operations by state.", []string{"state"}, nil),
		scrapeDuration: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "scrape_duration_seconds"),
			"Time taken to collect the goipsla metrics.", nil, nil),
		trackState: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "track_state"),
			"Track state: 1 up, 0 down, -1 unknown (before the first attempt, or when the operation is gone).",
			[]string{"track", "op", "mode"}, nil),
		reactionOccurred: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "reaction_occurred"),
			"1 while the threshold condition of a reaction row is violated (occurred), else 0.",
			[]string{"id", "element"}, nil),
		eventsTotal: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "events_total"),
			"Events handled by each sink: result delivered, failed or dropped (kind is empty for dropped).",
			[]string{"kind", "sink", "result"}, nil),
		probeDropped: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "probe_packets_dropped_total"),
			"ICMP packets the engine dropped: reason foreign (not a reply to our requests), malformed or other (another ICMP type).",
			[]string{"reason"}, nil),
		probeLate: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "probe_late_replies_total"),
			"Replies to our requests that arrived after their timeout, and duplicates.", nil, nil),
		probeRecvErrors: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "probe_receive_errors_total"),
			"Failed reads from the raw sockets.", nil, nil),
		resultsDiscarded: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "results_discarded_total"),
			"Results dropped because their attempt started before the current life (restart, reset, reload).", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.info, c.state, c.latestRTT, c.latestRC, c.latestSuccess, c.latestEnd,
		c.attempts, c.completions, c.results, c.rttSum, c.rttMin, c.rttMax,
		c.lifeStart, c.operations, c.scrapeDuration,
		c.jitterAvg, c.jitterLoss, c.jitterLate, c.jitterOOS, c.jitterSkipped, c.jitterOverTh,
		c.trackState, c.reactionOccurred, c.eventsTotal,
		c.probeDropped, c.probeLate, c.probeRecvErrors, c.resultsDiscarded,
	} {
		ch <- d
	}
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// msToSeconds converts the integer-millisecond counters of stats to seconds.
func msToSeconds(ms uint64) float64 { return float64(ms) / 1000 }

// sample is a prometheus.Metric with precomputed, sorted label pairs. The
// pairs of one operation are built once per scrape and shared by all its
// samples, which is several times cheaper than prometheus.MustNewConstMetric
// (that one builds and sorts the pairs for every sample). The pairs are
// never modified after construction.
type sample struct {
	desc    *prometheus.Desc
	labels  []*dto.LabelPair
	counter bool
	value   float64
}

func (s *sample) Desc() *prometheus.Desc { return s.desc }

func (s *sample) Write(m *dto.Metric) error {
	m.Label = s.labels
	v := s.value
	if s.counter {
		m.Counter = &dto.Counter{Value: &v}
	} else {
		m.Gauge = &dto.Gauge{Value: &v}
	}
	return nil
}

func pair(name, value string) *dto.LabelPair { return &dto.LabelPair{Name: &name, Value: &value} }

// opLabelPairs holds the label pairs of one operation, sorted by name as
// the exposition requires: id, tag, target, type, vrf. The extra labels
// (rc, result, state) all sort between id and tag.
type opLabelPairs struct {
	base  []*dto.LabelPair
	id    *dto.LabelPair
	after []*dto.LabelPair // tag, target, type, vrf
}

func newOpLabelPairs(r *Row) opLabelPairs {
	id := pair("id", strconv.Itoa(r.ID))
	after := []*dto.LabelPair{pair("tag", r.Tag), pair("target", r.Target), pair("type", string(r.Type)), pair("vrf", r.VRF)}
	return opLabelPairs{base: append([]*dto.LabelPair{id}, after...), id: id, after: after}
}

// withFirst returns the pairs plus one extra label that sorts before id
// (direction).
func (o opLabelPairs) withFirst(name, value string) []*dto.LabelPair {
	ls := make([]*dto.LabelPair, 0, 6)
	ls = append(ls, pair(name, value))
	return append(ls, o.base...)
}

// with returns the pairs plus one extra label that sorts after id.
func (o opLabelPairs) with(name, value string) []*dto.LabelPair {
	ls := make([]*dto.LabelPair, 0, 6)
	ls = append(ls, o.id, pair(name, value))
	return append(ls, o.after...)
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	start := c.clk.Now()
	counts := map[string]int{}
	emit := func(d *prometheus.Desc, labels []*dto.LabelPair, counter bool, v float64) {
		ch <- &sample{desc: d, labels: labels, counter: counter, value: v}
	}
	for _, r := range c.src.Rows() {
		lp := newOpLabelPairs(&r)
		base := lp.base
		counts[r.State]++

		emit(c.info, lp.with("state", r.State), false, 1)
		if v, ok := stateValues[r.State]; ok {
			emit(c.state, base, false, v)
		}
		if l := r.Latest; l.Valid {
			if l.Code.HasRTT() {
				emit(c.latestRTT, base, false, l.RTT.Seconds())
			}
			emit(c.latestRC, lp.with("rc", l.Code.String()), false, float64(l.Code))
			success := 0.0
			if l.Code == op.RCOK {
				success = 1
			}
			emit(c.latestSuccess, base, false, success)
			if !l.End.IsZero() {
				emit(c.latestEnd, base, false, unixSeconds(l.End))
			}
		}
		t := r.Totals
		emit(c.attempts, base, true, float64(t.Initiations))
		emit(c.completions, base, true, float64(t.Completions))
		for _, res := range results {
			emit(c.results, lp.with("result", res), true, float64(resultCount(t, res)))
		}
		emit(c.rttSum, base, true, msToSeconds(t.RTTSumMs))
		if t.Completions > 0 {
			emit(c.rttMin, base, false, msToSeconds(t.RTTMinMs))
			emit(c.rttMax, base, false, msToSeconds(t.RTTMaxMs))
		}
		if !r.LifeStart.IsZero() {
			emit(c.lifeStart, base, false, unixSeconds(r.LifeStart))
		}
		if lj := r.Latest.Jitter; lj != nil {
			emit(c.jitterAvg, lp.withFirst("direction", "sd"), false, lj.AvgSDJitterMs()/1000)
			emit(c.jitterAvg, lp.withFirst("direction", "ds"), false, lj.AvgDSJitterMs()/1000)
			emit(c.jitterAvg, lp.withFirst("direction", "both"), false, lj.AvgJitterMs()/1000)
		}
		if tj := r.TotalsJitter; tj != nil {
			emit(c.jitterLoss, base, true, float64(tj.PktLoss))
			emit(c.jitterLate, base, true, float64(tj.PktLateArrival))
			emit(c.jitterOOS, lp.withFirst("direction", "sd"), true, float64(tj.PktOutSeqSD))
			emit(c.jitterOOS, lp.withFirst("direction", "ds"), true, float64(tj.PktOutSeqDS))
			emit(c.jitterOOS, lp.withFirst("direction", "both"), true, float64(tj.PktOutSeqBoth))
			emit(c.jitterSkipped, base, true, float64(tj.Skipped))
			emit(c.jitterOverTh, base, true, float64(tj.NumOverThreshold))
		}
	}
	for _, s := range stateOrder {
		ch <- prometheus.MustNewConstMetric(c.operations, prometheus.GaugeValue, float64(counts[s]), s)
	}
	for _, t := range c.src.Tracks() {
		v, ok := trackStateValues[t.State]
		if !ok {
			v = -1
		}
		ch <- prometheus.MustNewConstMetric(c.trackState, prometheus.GaugeValue, v, strconv.Itoa(t.ID), strconv.Itoa(t.Operation), t.Mode)
	}
	for _, r := range c.src.Reactions() {
		v := 0.0
		if r.Occurred {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.reactionOccurred, prometheus.GaugeValue, v, strconv.Itoa(r.OpID), r.Element)
	}
	for _, e := range c.src.EventStats() {
		ch <- prometheus.MustNewConstMetric(c.eventsTotal, prometheus.CounterValue, float64(e.Count), e.Kind, e.Sink, e.Result)
	}
	if ds, ok := c.src.(DiagnosticsSource); ok {
		d := ds.Diagnostics()
		counter := func(desc *prometheus.Desc, v uint64, labels ...string) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), labels...)
		}
		counter(c.probeDropped, d.Probe.Foreign, "foreign")
		counter(c.probeDropped, d.Probe.Malformed, "malformed")
		counter(c.probeDropped, d.Probe.Other, "other")
		counter(c.probeLate, d.Probe.Late)
		counter(c.probeRecvErrors, d.Probe.RecvErrors)
		counter(c.resultsDiscarded, d.ResultsDiscarded)
	}
	ch <- prometheus.MustNewConstMetric(c.scrapeDuration, prometheus.GaugeValue, c.clk.Since(start).Seconds())
}

// newRegistry returns a registry with the goipsla collector and the Go
// runtime and process collectors.
func newRegistry(src Source, clk clock.Clock) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		newCollector(src, clk),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// handler serves /metrics for src; every other path is 404.
func handler(src Source, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	reg := newRegistry(src, nil)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ErrorHandling: promhttp.ContinueOnError,
	}))
	mux.HandleFunc("/", http.NotFound)
	return mux
}

const (
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// Serve serves /metrics on listen (host:port) until ctx is done. An empty
// listen disables the exporter: Serve returns nil at once. A listen error is
// returned immediately.
func Serve(ctx context.Context, listen string, src Source, logger *slog.Logger) error {
	if listen == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("metrics: listen on %s: %w", listen, err)
	}
	return serve(ctx, ln, src, logger)
}

func serve(ctx context.Context, ln net.Listener, src Source, logger *slog.Logger) error {
	srv := &http.Server{
		Handler:           handler(src, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	logger.Info("metrics exporter listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("metrics: %w", err)
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			logger.Warn("metrics exporter did not shut down in time; closing", "timeout", shutdownTimeout.String(), "err", err)
			srv.Close()
		}
		<-errc
		logger.Info("metrics exporter closed", "addr", ln.Addr().String())
		return nil
	}
}
