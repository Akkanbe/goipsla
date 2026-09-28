//declscope:namespace new

package api

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/stats"
)

func TestRTTMs(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want float64
	}{
		{0, 0},
		{1234567 * time.Nanosecond, 1.235}, // rounds half up at the microsecond
		{1234499 * time.Nanosecond, 1.234},
		{999 * time.Nanosecond, 0.001},
		{499 * time.Nanosecond, 0},
		{5 * time.Second, 5000},
		{time.Millisecond + 500*time.Nanosecond, 1.001},
	}
	for _, tt := range tests {
		if got := op.RTTMillis(tt.in); got != tt.want {
			t.Errorf("RTTMs(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNewLatestJSON(t *testing.T) {
	loc := time.FixedZone("JST", 9*3600)
	start := time.Date(2026, 9, 27, 21, 0, 0, 123, loc)
	end := start.Add(1500 * time.Microsecond)
	tests := []struct {
		name string
		in   stats.Latest
		want string
	}{
		{
			name: "never attempted",
			in:   stats.Latest{Code: op.RCOther},
			want: `{"valid":false,"code":"other"}`,
		},
		{
			name: "ok",
			in:   stats.Latest{Valid: true, Seq: 7, Start: start, End: end, RTT: 1499600 * time.Nanosecond, Code: op.RCOK},
			want: `{"valid":true,"seq":7,"start":"2026-09-27T12:00:00.000000123Z","end":"2026-09-27T12:00:00.001500123Z","rtt_ms":1.5,"code":"ok"}`,
		},
		{
			name: "overThreshold keeps rtt",
			in:   stats.Latest{Valid: true, Seq: 1, Start: start, End: end, RTT: 2 * time.Second, Code: op.RCOverThreshold},
			want: `{"valid":true,"seq":1,"start":"2026-09-27T12:00:00.000000123Z","end":"2026-09-27T12:00:00.001500123Z","rtt_ms":2000,"code":"overThreshold"}`,
		},
		{
			name: "timeout has no rtt but a detail",
			in:   stats.Latest{Valid: true, Seq: 2, Start: start, End: end, RTT: time.Second, Code: op.RCTimeout, Detail: "destination unreachable: host, from 10.100.1.254"},
			want: `{"valid":true,"seq":2,"start":"2026-09-27T12:00:00.000000123Z","end":"2026-09-27T12:00:00.001500123Z","code":"timeout","detail":"destination unreachable: host, from 10.100.1.254"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(newLatestJSON(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Errorf("got  %s\nwant %s", b, tt.want)
			}
		})
	}
}

func TestNewCountersJSON(t *testing.T) {
	c := stats.Counters{
		Initiations: 10, Completions: 8, OverThresholds: 2, Timeouts: 1, Drops: 1, Busies: 3, SequenceErrors: 4,
		RTTSumMs: 16, RTTSum2Ms: 40, RTTMinMs: 1, RTTMaxMs: 4,
	}
	got := NewCountersJSON(c)
	want := CountersJSON{
		Initiations: 10, Completions: 8, OverThresholds: 2, Timeouts: 1, Drops: 1, Busies: 3, SequenceErrors: 4,
		Successes: 6, Failures: 4, RTTSumMs: 16, RTTSum2Ms: 40, RTTMinMs: 1, RTTMaxMs: 4,
		RTTAvgMs: 2, RTTStdDevMs: 1, // sqrt(40/8 - 2^2) = 1
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Error(diff)
	}
}

func TestNewHourGroupJSON(t *testing.T) {
	h := stats.HourGroup{
		Index:    3,
		Start:    time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
		Counters: stats.Counters{Initiations: 5, Completions: 4, RTTSumMs: 70, RTTSum2Ms: 1700, RTTMinMs: 5, RTTMaxMs: 30},
		Dist: []stats.DistBucket{
			{Index: 1, LowerMs: 0, UpperMs: 20, Completions: 3, RTTSumMs: 40, RTTSum2Ms: 600, RTTMinMs: 5, RTTMaxMs: 20},
			{Index: 2, LowerMs: 20, UpperMs: 0, Completions: 1, OverThresholds: 1, RTTSumMs: 30, RTTSum2Ms: 900, RTTMinMs: 30, RTTMaxMs: 30},
		},
	}
	got := NewHourGroupJSON(h)
	b, _ := json.Marshal(got.Dist)
	want := `[{"index":1,"lower_ms":0,"upper_ms":20,"completions":3,"over_thresholds":0,"rtt_sum_ms":40,"rtt_sum2_ms":600,"rtt_min_ms":5,"rtt_max_ms":20,"rtt_avg_ms":13.333333333333334,"percent":75},` +
		`{"index":2,"lower_ms":20,"upper_ms":null,"completions":1,"over_thresholds":1,"rtt_sum_ms":30,"rtt_sum2_ms":900,"rtt_min_ms":30,"rtt_max_ms":30,"rtt_avg_ms":30,"percent":25}]`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}

	// No completions: percent and average are 0, dist is [] not null.
	empty := NewHourGroupJSON(stats.HourGroup{Index: 1, Dist: []stats.DistBucket{{Index: 1}}})
	if empty.Dist[0].Percent != 0 || empty.Dist[0].RTTAvgMs != 0 || empty.Dist[0].UpperMs != nil {
		t.Errorf("empty: %+v", empty.Dist[0])
	}
	b, _ = json.Marshal(NewHourGroupJSON(stats.HourGroup{Index: 1}))
	if !strings.Contains(string(b), `"dist":[]`) {
		t.Errorf("dist should be []: %s", b)
	}
}

func TestNewOperationDetail(t *testing.T) {
	cfg := &config.Operation{
		ID: 101, Type: config.ICMPEcho, Target: netip.MustParseAddr("10.100.1.11"), TargetName: "10.100.1.11",
		VRF: "blue", Frequency: 10 * time.Second, Timeout: 2 * time.Second, Threshold: 300 * time.Millisecond,
		RequestDataSize: 28, DataPattern: 0xABCDABCD, Tag: "wan",
		Stats:   config.StatsConfig{HoursKept: 2, DistBuckets: 1, DistInterval: 20 * time.Millisecond},
		History: config.HistoryConfig{Buckets: 15, Filter: config.FilterNone},
	}
	lifeStart := time.Date(2026, 9, 27, 21, 0, 0, 0, time.FixedZone("JST", 9*3600))
	s := &stats.Snapshot{
		ID: 101, Type: config.ICMPEcho, Target: cfg.Target, Tag: "wan", LifeStart: lifeStart, LifeIndex: 2,
		Latest: stats.Latest{Code: op.RCOther},
		History: []stats.HistoryBucket{
			{Life: 2, Bucket: 1, Sample: 1, Start: lifeStart, RTTMs: 3, Code: op.RCOK, Target: cfg.Target},
		},
		Enhanced: []stats.EnhancedBucket{{Index: 1, Start: lifeStart, Counters: stats.Counters{Initiations: 1}}},
	}
	d, err := NewOperationDetail(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d.VRF != "blue" || d.State != "" || d.Target != "10.100.1.11" || d.LifeIndex != 2 {
		t.Errorf("row %+v", d.OperationRow)
	}
	if d.LifeStart.Location() != time.UTC || d.History[0].Start.Location() != time.UTC {
		t.Error("times must be UTC")
	}
	if d.Hours != nil {
		t.Error("hours must be omitted when the snapshot has none")
	}
	var c map[string]any
	if err := json.Unmarshal(d.Config, &c); err != nil || c["frequency"] != "10s" || c["vrf"] != "blue" {
		t.Errorf("config %s (%v)", d.Config, err)
	}
	b, _ := json.Marshal(d)
	for _, want := range []string{`"history":[{"life":2,"bucket":1,"sample":1,"start":"2026-09-27T12:00:00Z","rtt_ms":3,"code":"ok","target":"10.100.1.11"}]`, `"life_start":"2026-09-27T12:00:00Z"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
	if strings.Contains(string(b), `"hours"`) {
		t.Errorf("hours should be omitted:\n%s", b)
	}
}
