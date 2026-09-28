// A test of the collector; it shares the collector's namespace.
//
//declscope:namespace metrics

package metrics

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/stats"
)

// jitterRows mixes an icmp-jitter operation with jitter data, one without
// (never run), and an echo operation.
func jitterRows() fakeSource {
	latest := &op.JitterResult{
		NumPackets: 10, Sent: 10, NumRTT: 9,
		PosSD: op.JitterSide{Num: 6, SumMs: 3}, NegSD: op.JitterSide{Num: 1, SumMs: 4},
		PosDS:   op.JitterSide{Num: 7, SumMs: 14},
		PktLoss: 1,
	}
	totals := &stats.JitterCounters{
		PktLoss: 7, PktLateArrival: 2, PktOutSeqSD: 1, PktOutSeqDS: 3, PktOutSeqBoth: 4, Skipped: 5, NumOverThreshold: 6,
	}
	rows := threeRows()
	rows = append(rows, Row{
		ID: 2, Type: config.ICMPJitter, Target: "10.100.2.12", Tag: "voice", State: "active",
		Latest:       stats.Latest{Valid: true, Seq: 3, End: base, RTT: 2 * time.Millisecond, Code: op.RCOK, Jitter: latest},
		Totals:       stats.Counters{Initiations: 3, Completions: 3},
		TotalsJitter: totals,
	})
	return rows
}

const l2 = "id=2,tag=voice,target=10.100.2.12,type=icmp-jitter,vrf="

func TestJitterMetrics(t *testing.T) {
	mfs, _ := scrape(t, jitterRows())
	tests := []struct {
		name string
		typ  dto.MetricType
		want map[string]float64
	}{
		// Latest burst: SD (3+4)/7 = 1 ms, DS 14/7 = 2 ms, both 21/14 = 1.5 ms.
		{"goipsla_jitter_avg_seconds", dto.MetricType_GAUGE, map[string]float64{
			"direction=sd," + l2: 0.001, "direction=ds," + l2: 0.002, "direction=both," + l2: 0.0015,
		}},
		{"goipsla_jitter_packet_loss_total", dto.MetricType_COUNTER, map[string]float64{l2: 7}},
		{"goipsla_jitter_packets_late_total", dto.MetricType_COUNTER, map[string]float64{l2: 2}},
		{"goipsla_jitter_packets_out_of_sequence_total", dto.MetricType_COUNTER, map[string]float64{
			"direction=sd," + l2: 1, "direction=ds," + l2: 3, "direction=both," + l2: 4,
		}},
		{"goipsla_jitter_packets_skipped_total", dto.MetricType_COUNTER, map[string]float64{l2: 5}},
		{"goipsla_jitter_rtt_over_threshold_total", dto.MetricType_COUNTER, map[string]float64{l2: 6}},
		// The common series still cover every operation.
		{"goipsla_attempts_total", dto.MetricType_COUNTER, map[string]float64{l101: 120, l102: 12, l1: 0, l2: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf := mfs[tt.name]
			if mf == nil {
				t.Fatal("missing")
			}
			if mf.GetType() != tt.typ {
				t.Errorf("type %v, want %v", mf.GetType(), tt.typ)
			}
			if diff := cmp.Diff(tt.want, samples(mf)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// TestJitterMetricsAbsent: echo operations have no jitter series.
func TestJitterMetricsAbsent(t *testing.T) {
	mfs, body := scrape(t, threeRows())
	for name := range mfs {
		if len(name) > 14 && name[:14] == "goipsla_jitter" {
			t.Errorf("%s present:\n%s", name, body)
		}
	}
}

// TestJitterBeforeFirstBurst: a jitter operation whose life has started but
// has no burst yet (TotalsJitter empty, Latest.Jitter nil) exports its
// packet counters at 0 and no average jitter (docs/metrics.md).
func TestJitterBeforeFirstBurst(t *testing.T) {
	rows := jitterRows()
	rows[3].Latest = stats.Latest{}
	rows[3].TotalsJitter = &stats.JitterCounters{}
	mfs, body := scrape(t, rows)
	if _, ok := samples(mfs["goipsla_jitter_avg_seconds"])["direction=sd,"+l2]; ok {
		t.Errorf("average jitter before the first burst:\n%s", body)
	}
	if got, ok := samples(mfs["goipsla_jitter_packet_loss_total"])[l2]; !ok || got != 0 {
		t.Errorf("packet loss = %v, %v; want 0 present", got, ok)
	}
}

func TestJitterLint(t *testing.T) {
	problems, err := testutil.CollectAndLint(newCollector(jitterRows(), nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("%s: %s", p.Metric, p.Text)
	}
}
