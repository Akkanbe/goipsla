package api_test

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/stats"
)

func TestJitterJSON(t *testing.T) {
	if api.NewJitterResultJSON(nil) != nil || api.NewJitterCountersJSON(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	jr := apitest.LatestJitter
	j := api.NewJitterResultJSON(&jr)
	if j.NumPackets != 10 || j.NumRTT != 9 || j.RTTAvgMs != 4.0/9 || j.PktLoss != 1 || j.MaxSucPktLoss != 1 {
		t.Errorf("result = %+v", j)
	}
	if j.PosSD != (api.JitterSideJSON{Num: 6, SumMs: 1, Sum2Ms: 1, MinMs: 0, MaxMs: 1, AvgMs: 1.0 / 6}) {
		t.Errorf("pos_sd = %+v", j.PosSD)
	}
	if j.AvgJitterMs != jr.AvgJitterMs() || j.AvgSDJitterMs != 2.0/7 || j.AvgDSJitterMs != 0 {
		t.Errorf("averages = %v %v %v", j.AvgJitterMs, j.AvgSDJitterMs, j.AvgDSJitterMs)
	}
	c := apitest.TotalsJitter
	jc := api.NewJitterCountersJSON(&c)
	if jc.NumRTT != 396 || jc.PktOutSeqDS != 2 || jc.PktLateArrival != 1 || jc.MaxSucPktLoss != 2 || jc.AvgDSJitterMs != c.AvgDSJitterMs() {
		t.Errorf("counters = %+v", jc)
	}

	// The JSON field names are snake case.
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"num_packets", "sent", "skipped", "num_rtt", "rtt_sum_ms", "rtt_sum2_ms", "rtt_min_ms", "rtt_max_ms",
		"rtt_avg_ms", "num_over_threshold", "pos_sd", "neg_sd", "pos_ds", "neg_ds", "avg_jitter_ms", "avg_sd_jitter_ms",
		"avg_ds_jitter_ms", "pkt_loss", "pkt_late_arrival", "pkt_out_seq_sd", "pkt_out_seq_ds", "pkt_out_seq_both",
		"min_suc_pkt_loss", "max_suc_pkt_loss", "one_way", "num_ow", "ow_sd", "ow_ds"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q in %s", k, b)
		}
	}
	if len(m) != 28 {
		t.Errorf("%d fields in %s", len(m), b)
	}
}

func TestJitterSnapshotToJSON(t *testing.T) {
	jr := apitest.LatestJitter
	jc := apitest.TotalsJitter
	hj := jc
	s := &stats.Snapshot{
		ID: 1, Type: config.ICMPJitter, Target: netip.MustParseAddr("10.100.2.11"),
		Latest:       stats.Latest{Valid: true, Seq: 1, Code: op.RCOK, RTT: time.Millisecond, Jitter: &jr},
		TotalsJitter: &jc,
		Hours:        []stats.HourGroup{{Index: 1, Jitter: &hj}},
	}
	cfg := &config.Operation{ID: 1, Type: config.ICMPJitter, Target: s.Target}
	d, err := api.NewOperationDetail(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d.Latest.Jitter == nil || d.Latest.Jitter.NumRTT != 9 || d.TotalsJitter == nil || d.TotalsJitter.NumRTT != 396 ||
		len(d.Hours) != 1 || d.Hours[0].Jitter == nil || len(d.Hours[0].Dist) != 0 {
		t.Errorf("detail = %+v", d)
	}

	// icmp-echo: no jitter keys at all.
	e, err := api.NewOperationDetail(&stats.Snapshot{ID: 2, Type: config.ICMPEcho, Target: s.Target, Hours: []stats.HourGroup{{Index: 1}}},
		&config.Operation{ID: 2, Type: config.ICMPEcho, Target: s.Target})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	for _, k := range []string{`"jitter"`, `"totals_jitter"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("echo JSON contains %s: %s", k, b)
		}
	}
}
