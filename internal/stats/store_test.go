package stats

import (
	"math"
	"math/rand/v2"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
)

var (
	t0     = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	target = netip.MustParseAddr("10.100.1.11")
)

// opCfg returns an operation with the Cisco defaults that matter here.
func opCfg(id int) *config.Operation {
	return &config.Operation{
		ID:        id,
		Type:      config.ICMPEcho,
		Target:    target,
		Tag:       "tag",
		Threshold: 5 * time.Second,
		Stats:     config.StatsConfig{HoursKept: 2, DistBuckets: 1, DistInterval: 20 * time.Millisecond},
		History:   config.HistoryConfig{Lives: 0, Buckets: 15, Filter: "none"},
	}
}

func newStore(t *testing.T, cfg *config.Operation) *Store {
	t.Helper()
	s := NewStore(clock.NewFake(t0))
	s.Add(cfg, t0)
	return s
}

// res returns a result of op 1 started at t0+at.
func res(seq uint32, at time.Duration, code op.ReturnCode, rtt time.Duration) op.Result {
	return op.Result{
		OpID:  1,
		Type:  config.ICMPEcho,
		Seq:   seq,
		Start: t0.Add(at),
		End:   t0.Add(at + rtt),
		RTT:   rtt,
		Code:  code,
	}
}

func snap(t *testing.T, s *Store, id int) *Snapshot {
	t.Helper()
	sn, ok := s.Snapshot(id, SnapshotOptions{Hours: true, History: true, Enhanced: true})
	if !ok {
		t.Fatalf("no snapshot for %d", id)
	}
	return sn
}

// TestAccountingTable checks every row of the accounting table in
// docs/statistics.md, "Accounting model".
func TestAccountingTable(t *testing.T) {
	const rtt = 12*time.Millisecond + 700*time.Microsecond
	for _, tc := range []struct {
		code   op.ReturnCode
		want   Counters
		latest bool
	}{
		{op.RCOK, Counters{Initiations: 1, Completions: 1, RTTSumMs: 12, RTTSum2Ms: 144, RTTMinMs: 12, RTTMaxMs: 12}, true},
		{op.RCOverThreshold, Counters{Initiations: 1, Completions: 1, OverThresholds: 1, RTTSumMs: 12, RTTSum2Ms: 144, RTTMinMs: 12, RTTMaxMs: 12}, true},
		{op.RCTimeout, Counters{Initiations: 1, Timeouts: 1}, true},
		{op.RCVerifyError, Counters{Initiations: 1, VerifyErrors: 1}, true},
		{op.RCDropped, Counters{Initiations: 1, Drops: 1}, true},
		{op.RCError, Counters{Initiations: 1, Drops: 1}, true},
		{op.RCBusy, Counters{Busies: 1}, false},
		{op.RCSequenceError, Counters{SequenceErrors: 1}, false},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			s := newStore(t, opCfg(1))
			r := res(7, time.Second, tc.code, rtt)
			r.Detail = "detail"
			s.Record(r)
			sn := snap(t, s, 1)
			if diff := cmp.Diff(tc.want, sn.Totals); diff != "" {
				t.Errorf("totals (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.want, sn.Hours[0].Counters); diff != "" {
				t.Errorf("hour group (-want +got):\n%s", diff)
			}
			if sn.Latest.Valid != tc.latest {
				t.Fatalf("latest valid = %v, want %v", sn.Latest.Valid, tc.latest)
			}
			if tc.latest {
				want := Latest{Valid: true, Seq: 7, Start: r.Start, End: r.End, Code: tc.code, Detail: "detail"}
				if isCompletion(tc.code) {
					want.RTT = rtt // nanoseconds kept
				}
				if diff := cmp.Diff(want, sn.Latest); diff != "" {
					t.Errorf("latest (-want +got):\n%s", diff)
				}
			} else if sn.Latest.Code != op.RCOther {
				t.Errorf("latest code = %v, want other", sn.Latest.Code)
			}
		})
	}
}

func TestLatestNotUpdatedByBusyOrSequenceError(t *testing.T) {
	s := newStore(t, opCfg(1))
	s.Record(res(1, 0, op.RCOK, 3*time.Millisecond))
	s.Record(res(0, time.Second, op.RCBusy, 0))
	s.Record(res(1, 2*time.Second, op.RCSequenceError, 0))
	sn := snap(t, s, 1)
	if sn.Latest.Seq != 1 || sn.Latest.Code != op.RCOK || sn.Latest.RTT != 3*time.Millisecond {
		t.Errorf("latest = %+v", sn.Latest)
	}
}

func TestSuccessesAndFailures(t *testing.T) {
	s := newStore(t, opCfg(1))
	// "Number of successes: 0 / Number of failures: 23 /
	//  Failed Operations due to ... TimeOut ...: 0/23/0/0"
	for i := range 23 {
		s.Record(res(uint32(i+1), time.Duration(i)*time.Minute, op.RCTimeout, 0))
	}
	c := snap(t, s, 1).Totals
	if c.Successes() != 0 || c.Failures() != 23 || c.Timeouts != 23 || c.Initiations != 23 || c.Completions != 0 {
		t.Errorf("timeouts: %+v successes %d failures %d", c, c.Successes(), c.Failures())
	}
	if c.AvgMs() != 0 || c.StdDevMs() != 0 || c.RTTMinMs != 0 || c.RTTMaxMs != 0 {
		t.Errorf("rtt stats without completions: %+v", c)
	}

	// "return code Over threshold / Number of successes: 0 / Number of
	//  failures: 6": over-threshold completions count as completions and
	//  failures, not successes.
	s = newStore(t, opCfg(1))
	for i := range 6 {
		s.Record(res(uint32(i+1), time.Duration(i)*10*time.Second, op.RCOverThreshold, 36*time.Millisecond))
	}
	c = snap(t, s, 1).Totals
	if c.Completions != 6 || c.OverThresholds != 6 || c.Successes() != 0 || c.Failures() != 6 || c.RTTSumMs != 216 {
		t.Errorf("over threshold: %+v successes %d failures %d", c, c.Successes(), c.Failures())
	}

	s.Record(res(7, time.Minute, op.RCOK, 10*time.Millisecond))
	c = snap(t, s, 1).Totals
	if c.Successes() != 1 || c.Failures() != 6 {
		t.Errorf("mixed: successes %d failures %d", c.Successes(), c.Failures())
	}
}

func TestRTTTruncation(t *testing.T) {
	s := newStore(t, opCfg(1))
	s.Record(res(1, 0, op.RCOK, 900*time.Microsecond))
	s.Record(res(2, time.Second, op.RCOK, 1900*time.Microsecond))
	sn := snap(t, s, 1)
	c := sn.Totals
	if c.RTTSumMs != 1 || c.RTTSum2Ms != 1 || c.RTTMinMs != 0 || c.RTTMaxMs != 1 || c.Completions != 2 {
		t.Errorf("counters = %+v", c)
	}
	if sn.Latest.RTT != 1900*time.Microsecond {
		t.Errorf("latest RTT = %v, want 1.9ms (nanoseconds kept)", sn.Latest.RTT)
	}
}

func TestStdDev(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Counters
		avg  float64
		sd   float64
	}{
		{"n=0", Counters{}, 0, 0},
		{"n=1", Counters{Completions: 1, RTTSumMs: 7, RTTSum2Ms: 49}, 7, 0},
		{"2,4,4,4,5,5,7,9", Counters{Completions: 8, RTTSumMs: 40, RTTSum2Ms: 232}, 5, 2},
		// Sum² beyond 32 bits: 1000 samples of 60000 ms sum to 3.6e12.
		{"large", Counters{Completions: 1000, RTTSumMs: 60_000_000, RTTSum2Ms: 3_600_000_000_000}, 60000, 0},
	} {
		if got := tc.c.AvgMs(); got != tc.avg {
			t.Errorf("%s: avg = %v, want %v", tc.name, got, tc.avg)
		}
		if got := tc.c.StdDevMs(); math.Abs(got-tc.sd) > 1e-9 {
			t.Errorf("%s: stddev = %v, want %v", tc.name, got, tc.sd)
		}
	}

	// Accumulated through Record, with RTTs whose squares exceed 32 bits.
	s := newStore(t, opCfg(1))
	for i, ms := range []int{70000, 80000} {
		s.Record(res(uint32(i+1), time.Duration(i)*time.Minute, op.RCOverThreshold, time.Duration(ms)*time.Millisecond))
	}
	c := snap(t, s, 1).Totals
	if c.RTTSum2Ms != 70000*70000+80000*80000 || c.StdDevMs() != 5000 || c.AvgMs() != 75000 {
		t.Errorf("counters = %+v stddev %v", c, c.StdDevMs())
	}
}

func TestDistributionBuckets(t *testing.T) {
	// "5 distributions × 10 ms interval": 0–9, 10–19, 20–29, 30–39, 40–∞.
	cfg := opCfg(1)
	cfg.Stats.DistBuckets = 5
	cfg.Stats.DistInterval = 10 * time.Millisecond
	s := newStore(t, cfg)
	for i, ms := range []int{0, 9, 10, 19, 25, 39, 40, 400} {
		s.Record(res(uint32(i+1), time.Duration(i)*time.Second, op.RCOK, time.Duration(ms)*time.Millisecond+500*time.Microsecond))
	}
	s.Record(res(9, 9*time.Second, op.RCTimeout, 0)) // not distributed
	s.Record(res(10, 10*time.Second, op.RCOverThreshold, 45*time.Millisecond))
	got := snap(t, s, 1).Hours[0].Dist
	want := []DistBucket{
		{Index: 1, LowerMs: 0, UpperMs: 10, Completions: 2, RTTSumMs: 9, RTTSum2Ms: 81, RTTMinMs: 0, RTTMaxMs: 9},
		{Index: 2, LowerMs: 10, UpperMs: 20, Completions: 2, RTTSumMs: 29, RTTSum2Ms: 100 + 361, RTTMinMs: 10, RTTMaxMs: 19},
		{Index: 3, LowerMs: 20, UpperMs: 30, Completions: 1, RTTSumMs: 25, RTTSum2Ms: 625, RTTMinMs: 25, RTTMaxMs: 25},
		{Index: 4, LowerMs: 30, UpperMs: 40, Completions: 1, RTTSumMs: 39, RTTSum2Ms: 1521, RTTMinMs: 39, RTTMaxMs: 39},
		// The overThreshold completion (45 ms) is distributed and counted in
		// the bucket's OverThresholds.
		{Index: 5, LowerMs: 40, UpperMs: 0, Completions: 3, OverThresholds: 1, RTTSumMs: 485, RTTSum2Ms: 1600 + 160000 + 2025, RTTMinMs: 40, RTTMaxMs: 400},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dist (-want +got):\n%s", diff)
	}
}

func TestSingleDistributionBucketIgnoresInterval(t *testing.T) {
	cfg := opCfg(1)
	cfg.Stats.DistBuckets = 1
	cfg.Stats.DistInterval = 10 * time.Millisecond
	s := newStore(t, cfg)
	for i, ms := range []int{1, 15, 3000} {
		s.Record(res(uint32(i+1), time.Duration(i)*time.Second, op.RCOK, time.Duration(ms)*time.Millisecond))
	}
	got := snap(t, s, 1).Hours[0].Dist
	want := []DistBucket{{Index: 1, LowerMs: 0, UpperMs: 0, Completions: 3, RTTSumMs: 3016, RTTSum2Ms: 1 + 225 + 9_000_000, RTTMinMs: 1, RTTMaxMs: 3000}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dist (-want +got):\n%s", diff)
	}
}

func TestDistIndex(t *testing.T) {
	for _, tc := range []struct {
		ms   uint64
		n    int
		w    uint64
		want int
	}{
		{0, 5, 10, 0}, {9, 5, 10, 0}, {10, 5, 10, 1}, {39, 5, 10, 3}, {40, 5, 10, 4}, {1 << 40, 5, 10, 4},
		{100, 1, 10, 0}, {5, 20, 1, 5}, {19, 20, 1, 19}, {1000, 20, 100, 10},
	} {
		if got := distIndex(tc.ms, tc.n, tc.w); got != tc.want {
			t.Errorf("distIndex(%d, %d, %d) = %d, want %d", tc.ms, tc.n, tc.w, got, tc.want)
		}
	}
}

func TestHourGroupRotation(t *testing.T) {
	s := newStore(t, opCfg(1)) // HoursKept 2
	s.Record(res(1, 10*time.Minute, op.RCOK, time.Millisecond))
	s.Record(res(2, 59*time.Minute+59*time.Second, op.RCOK, 2*time.Millisecond))
	s.Record(res(3, 60*time.Minute, op.RCTimeout, 0)) // hour 2 starts exactly at 60 min
	hours := snap(t, s, 1).Hours
	if len(hours) != 2 || hours[0].Index != 1 || hours[1].Index != 2 {
		t.Fatalf("hours = %+v", hours)
	}
	if hours[0].Counters.Completions != 2 || hours[1].Counters.Timeouts != 1 {
		t.Errorf("counters = %+v / %+v", hours[0].Counters, hours[1].Counters)
	}
	if !hours[0].Start.Equal(t0) || !hours[1].Start.Equal(t0.Add(time.Hour)) {
		t.Errorf("starts = %v, %v", hours[0].Start, hours[1].Start)
	}

	// The third hour discards the oldest; indexes keep increasing.
	s.Record(res(4, 2*time.Hour+time.Minute, op.RCOK, 3*time.Millisecond))
	hours = snap(t, s, 1).Hours
	if len(hours) != 2 || hours[0].Index != 2 || hours[1].Index != 3 {
		t.Fatalf("after rotation: %+v", hours)
	}
	// Totals cover the whole life.
	if tot := snap(t, s, 1).Totals; tot.Initiations != 4 {
		t.Errorf("totals = %+v", tot)
	}
}

func TestHourGroupGap(t *testing.T) {
	cfg := opCfg(1)
	cfg.Stats.HoursKept = 5
	s := newStore(t, cfg)
	// First result 2.5 hours after the life started: no empty groups, index 3.
	s.Record(res(1, 150*time.Minute, op.RCOK, time.Millisecond))
	hours := snap(t, s, 1).Hours
	if len(hours) != 1 || hours[0].Index != 3 || !hours[0].Start.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("hours = %+v", hours)
	}
	s.Record(res(2, 6*time.Hour, op.RCOK, time.Millisecond))
	hours = snap(t, s, 1).Hours
	if len(hours) != 2 || hours[1].Index != 7 {
		t.Fatalf("hours = %+v", hours)
	}
	// A late-arriving result for a kept older group goes there.
	s.Record(res(0, 2*time.Hour+time.Minute, op.RCSequenceError, 0))
	if hours = snap(t, s, 1).Hours; hours[0].Counters.SequenceErrors != 1 {
		t.Errorf("hours = %+v", hours)
	}
	// One for a discarded group only reaches the totals.
	s.Record(res(0, 30*time.Minute, op.RCSequenceError, 0))
	sn := snap(t, s, 1)
	if sn.Totals.SequenceErrors != 2 || len(sn.Hours) != 2 {
		t.Errorf("snapshot = %+v", sn)
	}
}

func TestHoursKeptZero(t *testing.T) {
	cfg := opCfg(1)
	cfg.Stats.HoursKept = 0
	s := newStore(t, cfg)
	s.Record(res(1, 0, op.RCOK, time.Millisecond))
	sn := snap(t, s, 1)
	if len(sn.Hours) != 0 || sn.Totals.Completions != 1 {
		t.Errorf("snapshot = %+v", sn)
	}
}

func historyCfg(lives, buckets int, filter string) *config.Operation {
	cfg := opCfg(1)
	cfg.History = config.HistoryConfig{Lives: lives, Buckets: buckets, Filter: config.HistoryFilter(filter)}
	return cfg
}

// mixed records ok, overThreshold, timeout, verifyError, busy, sequenceError
// and error.
func recordMixed(s *Store) {
	s.Record(res(1, 0, op.RCOK, 5*time.Millisecond))
	s.Record(res(2, time.Minute, op.RCOverThreshold, 6*time.Second))
	s.Record(res(3, 2*time.Minute, op.RCTimeout, 0))
	s.Record(res(4, 3*time.Minute, op.RCVerifyError, 0))
	s.Record(res(0, 4*time.Minute, op.RCBusy, 0))
	s.Record(res(3, 4*time.Minute, op.RCSequenceError, 0))
	s.Record(res(5, 5*time.Minute, op.RCError, 0))
}

func TestHistoryFilters(t *testing.T) {
	type b struct {
		bucket int
		code   op.ReturnCode
		rtt    uint64
	}
	for _, tc := range []struct {
		filter string
		want   []b
	}{
		{"none", nil},
		{"all", []b{{1, op.RCOK, 5}, {2, op.RCOverThreshold, 0}, {3, op.RCTimeout, 0}, {4, op.RCVerifyError, 0}, {5, op.RCError, 0}}},
		{"overThreshold", []b{{2, op.RCOverThreshold, 0}}},
		{"failures", []b{{3, op.RCTimeout, 0}, {4, op.RCVerifyError, 0}, {5, op.RCError, 0}}},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			s := newStore(t, historyCfg(1, 15, tc.filter))
			recordMixed(s)
			var got []b
			for _, h := range snap(t, s, 1).History {
				if h.Life != 1 || h.Sample != 1 || h.Target != target {
					t.Errorf("bucket = %+v", h)
				}
				got = append(got, b{h.Bucket, h.Code, h.RTTMs})
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(b{})); diff != "" {
				t.Errorf("history (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHistoryLivesZero(t *testing.T) {
	s := newStore(t, historyCfg(0, 15, "all"))
	recordMixed(s)
	if h := snap(t, s, 1).History; len(h) != 0 {
		t.Errorf("history = %+v", h)
	}
}

func TestHistoryBucketsLimit(t *testing.T) {
	s := newStore(t, historyCfg(1, 3, "all"))
	for i := range 5 {
		s.Record(res(uint32(i+1), time.Duration(i)*time.Minute, op.RCOK, time.Duration(i)*time.Millisecond))
	}
	h := snap(t, s, 1).History
	if len(h) != 3 || h[0].Bucket != 3 || h[2].Bucket != 5 || h[2].RTTMs != 4 {
		t.Fatalf("history = %+v", h)
	}
	if !h[0].Start.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("start = %v", h[0].Start)
	}
}

func TestHistoryLives(t *testing.T) {
	s := newStore(t, historyCfg(2, 15, "all"))
	s.Record(res(1, 0, op.RCOK, time.Millisecond))
	s.Record(res(2, time.Minute, op.RCOK, time.Millisecond))

	s.Reset(1, t0.Add(time.Hour))
	sn := snap(t, s, 1)
	if sn.LifeIndex != 2 || !sn.LifeStart.Equal(t0.Add(time.Hour)) {
		t.Fatalf("life = %d %v", sn.LifeIndex, sn.LifeStart)
	}
	s.Record(res(1, time.Hour, op.RCTimeout, 0))
	h := snap(t, s, 1).History
	want := [][2]int{{1, 1}, {1, 2}, {2, 1}}
	if len(h) != len(want) {
		t.Fatalf("history = %+v", h)
	}
	for i, w := range want {
		if h[i].Life != w[0] || h[i].Bucket != w[1] {
			t.Errorf("history[%d] = life %d bucket %d, want %v", i, h[i].Life, h[i].Bucket, w)
		}
	}

	// A third life discards the oldest.
	s.Reset(1, t0.Add(2*time.Hour))
	s.Record(res(1, 2*time.Hour, op.RCOK, time.Millisecond))
	h = snap(t, s, 1).History
	if len(h) != 2 || h[0].Life != 2 || h[1].Life != 3 || h[1].Bucket != 1 {
		t.Fatalf("history = %+v", h)
	}
}

func TestResetDiscardsStatistics(t *testing.T) {
	cfg := historyCfg(1, 15, "all")
	cfg.Enhanced = &config.EnhancedHistory{Interval: 15 * time.Minute, Buckets: 100}
	s := newStore(t, cfg)
	s.Record(res(1, 0, op.RCOK, time.Millisecond))
	s.Record(res(2, 2*time.Hour, op.RCOK, time.Millisecond))
	s.Reset(1, t0.Add(3*time.Hour))
	sn := snap(t, s, 1)
	if sn.Latest.Valid || sn.Totals != (Counters{}) || len(sn.Hours) != 0 || len(sn.Enhanced) != 0 || len(sn.History) != 0 {
		t.Fatalf("after reset: %+v", sn)
	}
	s.Record(res(1, 3*time.Hour+time.Minute, op.RCOK, time.Millisecond))
	sn = snap(t, s, 1)
	if sn.Hours[0].Index != 1 || sn.Enhanced[0].Index != 1 || sn.History[0].Life != 2 || sn.History[0].Bucket != 1 {
		t.Errorf("indexes after reset: hours %+v enhanced %+v history %+v", sn.Hours, sn.Enhanced, sn.History)
	}
	if sn.Type != config.ICMPEcho || sn.Tag != "tag" || sn.Target != target {
		t.Errorf("config lost: %+v", sn)
	}
}

func TestAddTwiceIsReset(t *testing.T) {
	s := newStore(t, opCfg(1))
	s.Record(res(1, 0, op.RCOK, time.Millisecond))
	cfg := opCfg(1)
	cfg.Tag = "new"
	s.Add(cfg, t0.Add(time.Hour))
	sn := snap(t, s, 1)
	if sn.LifeIndex != 2 || sn.Totals.Initiations != 0 || sn.Tag != "new" {
		t.Errorf("snapshot = %+v", sn)
	}
	if len(s.Summary()) != 1 {
		t.Error("duplicate summary rows")
	}
}

func TestEnhancedHistory(t *testing.T) {
	cfg := opCfg(1)
	cfg.Enhanced = &config.EnhancedHistory{Interval: 10 * time.Minute, Buckets: 3}
	s := newStore(t, cfg)
	s.Record(res(1, time.Minute, op.RCOK, 4*time.Millisecond))
	s.Record(res(2, 9*time.Minute, op.RCOverThreshold, 6*time.Second))
	s.Record(res(3, 10*time.Minute, op.RCTimeout, 0))
	s.Record(res(0, 35*time.Minute, op.RCBusy, 0)) // index 4; no bucket for 20–30 min
	e := snap(t, s, 1).Enhanced
	if len(e) != 3 || e[0].Index != 1 || e[1].Index != 2 || e[2].Index != 4 {
		t.Fatalf("enhanced = %+v", e)
	}
	if !e[2].Start.Equal(t0.Add(30 * time.Minute)) {
		t.Errorf("start = %v", e[2].Start)
	}
	want := Counters{Initiations: 2, Completions: 2, OverThresholds: 1, RTTSumMs: 6004, RTTSum2Ms: 16 + 36_000_000, RTTMinMs: 4, RTTMaxMs: 6000}
	if diff := cmp.Diff(want, e[0].Counters); diff != "" {
		t.Errorf("bucket 1 (-want +got):\n%s", diff)
	}
	// The bucket limit keeps the most recent; indexes keep increasing.
	s.Record(res(4, 50*time.Minute, op.RCOK, time.Millisecond))
	e = snap(t, s, 1).Enhanced
	if len(e) != 3 || e[0].Index != 2 || e[2].Index != 6 {
		t.Fatalf("enhanced = %+v", e)
	}
}

func TestEnhancedDisabled(t *testing.T) {
	s := newStore(t, opCfg(1))
	s.Record(res(1, 0, op.RCOK, time.Millisecond))
	if e := snap(t, s, 1).Enhanced; e != nil {
		t.Errorf("enhanced = %+v, want nil", e)
	}
}

func TestSnapshotOptionsAndCopies(t *testing.T) {
	cfg := historyCfg(1, 15, "all")
	cfg.Stats.DistBuckets = 3
	cfg.Enhanced = &config.EnhancedHistory{Interval: time.Minute, Buckets: 10}
	s := newStore(t, cfg)
	s.Record(res(1, 0, op.RCOK, time.Millisecond))

	sn, _ := s.Snapshot(1, SnapshotOptions{})
	if sn.Hours != nil || sn.History != nil || sn.Enhanced != nil {
		t.Errorf("optional parts without options: %+v", sn)
	}
	if _, ok := s.Snapshot(99, SnapshotOptions{}); ok {
		t.Error("snapshot of an unknown id")
	}

	sn = snap(t, s, 1)
	sn.Hours[0].Counters.Completions = 100
	sn.Hours[0].Dist[0].Completions = 100
	sn.History[0].RTTMs = 100
	sn.Enhanced[0].Counters.Completions = 100
	again := snap(t, s, 1)
	if again.Hours[0].Counters.Completions != 1 || again.Hours[0].Dist[0].Completions != 1 ||
		again.History[0].RTTMs != 1 || again.Enhanced[0].Counters.Completions != 1 {
		t.Errorf("snapshot shares memory with the store: %+v", again)
	}
}

func TestSummaryAndRemove(t *testing.T) {
	s := NewStore(clock.NewFake(t0))
	for _, id := range []int{30, 10, 20} {
		s.Add(opCfg(id), t0)
	}
	r := res(1, 0, op.RCOK, time.Millisecond)
	r.OpID = 20
	s.Record(r)
	r.OpID = 99 // unknown: ignored
	s.Record(r)
	rows := s.Summary()
	if len(rows) != 3 || rows[0].ID != 10 || rows[1].ID != 20 || rows[2].ID != 30 {
		t.Fatalf("rows = %+v", rows)
	}
	if !rows[1].Latest.Valid || rows[1].Totals.Completions != 1 || rows[1].Tag != "tag" || rows[1].Target != target {
		t.Errorf("row 20 = %+v", rows[1])
	}
	s.Remove(20)
	s.Remove(20)
	s.Reset(20, t0) // unknown: no-op
	rows = s.Summary()
	if len(rows) != 2 || rows[0].ID != 10 || rows[1].ID != 30 {
		t.Errorf("rows after remove = %+v", rows)
	}
}

func TestZeroStartUsesClock(t *testing.T) {
	clk := clock.NewFake(t0)
	s := NewStore(clk)
	s.Add(opCfg(1), t0)
	clk.Advance(61 * time.Minute)
	s.Record(op.Result{OpID: 1, Code: op.RCSequenceError})
	sn := snap(t, s, 1)
	if len(sn.Hours) != 1 || sn.Hours[0].Index != 2 {
		t.Errorf("hours = %+v", sn.Hours)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewStore(clock.NewFake(t0))
	const ops = 50
	for id := 1; id <= ops; id++ {
		cfg := historyCfg(2, 15, "all")
		cfg.ID = id
		cfg.Enhanced = &config.EnhancedHistory{Interval: time.Minute, Buckets: 10}
		s.Add(cfg, t0)
	}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 1))
			for i := range 2000 {
				r := res(uint32(i), time.Duration(i)*time.Second, op.ReturnCode(rng.IntN(10)), time.Duration(rng.IntN(100))*time.Millisecond)
				r.OpID = 1 + rng.IntN(ops)
				s.Record(r)
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				s.Summary()
				if sn, ok := s.Snapshot(1+i%ops, SnapshotOptions{Hours: true, History: true, Enhanced: true}); ok {
					_ = sn.Totals.StdDevMs()
				}
				if i%50 == 0 {
					s.Reset(1+i%ops, t0)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 100 {
			cfg := opCfg(1000 + i)
			s.Add(cfg, t0)
			s.Remove(1000 + i)
		}
	}()
	wg.Wait()
	if n := len(s.Summary()); n != ops {
		t.Errorf("rows = %d", n)
	}
}

func TestSetConfigKeepsStatistics(t *testing.T) {
	cfg := historyCfg(1, 15, "all")
	s := newStore(t, cfg)
	s.Record(res(1, 0, op.RCOK, time.Millisecond))
	before := snap(t, s, 1)

	upd := historyCfg(1, 15, "all")
	upd.Tag = "renamed"
	upd.Owner = "noc"
	upd.React = []config.Reaction{{Element: "rtt", ThresholdType: "immediate", Upper: 5, Lower: 3, Action: "trap"}}
	// Measurement settings in the new config are not applied by SetConfig.
	upd.Target = netip.MustParseAddr("10.100.1.99")
	upd.Stats.HoursKept = 0
	s.SetConfig(upd)
	s.SetConfig(opCfg(99)) // unknown: ignored

	after := snap(t, s, 1)
	if after.Tag != "renamed" || s.Summary()[0].Tag != "renamed" {
		t.Errorf("tag = %q / %q", after.Tag, s.Summary()[0].Tag)
	}
	after.Tag = before.Tag
	if diff := cmp.Diff(before, after, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Errorf("statistics changed (-before +after):\n%s", diff)
	}
	s.Record(res(2, time.Hour, op.RCOK, time.Millisecond))
	if sn := snap(t, s, 1); len(sn.Hours) != 2 || sn.Target != target || sn.LifeIndex != 1 {
		t.Errorf("after more results: %+v", sn)
	}
}

// TestDistOverThresholds: each bucket counts its overThreshold completions
// (rttMonStatsCaptureOverThresholds), and their sum is the hour group's.
func TestDistOverThresholds(t *testing.T) {
	cfg := opCfg(1)
	cfg.Stats.DistBuckets = 3
	cfg.Stats.DistInterval = 100 * time.Millisecond
	s := newStore(t, cfg)
	for i, r := range []struct {
		ms   int
		code op.ReturnCode
	}{
		{50, op.RCOK}, {150, op.RCOverThreshold}, {160, op.RCOK}, {250, op.RCOverThreshold}, {900, op.RCOverThreshold},
		{0, op.RCTimeout}, {0, op.RCVerifyError},
	} {
		s.Record(res(uint32(i+1), time.Duration(i)*time.Second, r.code, time.Duration(r.ms)*time.Millisecond))
	}
	g := snap(t, s, 1).Hours[0]
	got := []uint64{}
	var sum uint64
	for _, b := range g.Dist {
		got = append(got, b.OverThresholds)
		sum += b.OverThresholds
		if b.OverThresholds > b.Completions {
			t.Errorf("bucket %d: over thresholds %d > completions %d", b.Index, b.OverThresholds, b.Completions)
		}
	}
	if diff := cmp.Diff([]uint64{0, 1, 2}, got); diff != "" {
		t.Errorf("per-bucket over thresholds (-want +got):\n%s", diff)
	}
	if sum != g.Counters.OverThresholds || sum != 3 {
		t.Errorf("sum %d, hour group %d", sum, g.Counters.OverThresholds)
	}
}
