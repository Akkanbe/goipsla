package snmp

import (
	"io"
	"log/slog"
	"net/netip"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/react"
	"goipsla/internal/stats"
)

// base is the daemon start of the fixtures.
//
//declscope:package // shared test fixture
var base = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// quiet is a logger that drops everything.
//
//declscope:package // shared test fixture
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSource serves fixed operations.
type fakeSource struct {
	ids    []int
	cfgs   map[int]*config.Operation
	states map[int]string
	snaps  map[int]*stats.Snapshot
	reacts map[int][]react.ReactionState
}

func (f *fakeSource) IDs() []int { return f.ids }
func (f *fakeSource) Config(id int) (*config.Operation, bool) {
	c, ok := f.cfgs[id]
	return c, ok
}
func (f *fakeSource) State(id int) (string, time.Duration, bool, bool) {
	s, ok := f.states[id]
	if id == 12 {
		return s, 90 * time.Minute, false, ok
	}
	return s, 0, true, ok
}
func (f *fakeSource) Snapshot(id int, _ stats.SnapshotOptions) (*stats.Snapshot, bool) {
	s, ok := f.snaps[id]
	return s, ok
}
func (f *fakeSource) Reactions(id int) []react.ReactionState { return f.reacts[id] }

// echoCfg is an icmp-echo configuration with non-default values.
func echoCfg(id int, target string) *config.Operation {
	return &config.Operation{
		ID: id, Type: config.ICMPEcho, Target: netip.MustParseAddr(target), TargetName: target,
		Frequency: 10 * time.Second, Timeout: 2 * time.Second, Threshold: 300 * time.Millisecond,
		RequestDataSize: 28, TOS: 0xB8, Tag: "wan-uplink-primary-01", Owner: "noc", VRF: "blue",
		Stats:   config.StatsConfig{HoursKept: 2, DistBuckets: 2, DistInterval: 5 * time.Millisecond},
		History: config.HistoryConfig{Lives: 1, Buckets: 15, Filter: config.FilterAll},
	}
}

// newFake returns three operations: 11 (echo, IPv4, full statistics),
// 12 (echo, IPv6, pending, never attempted) and 31 (icmp-jitter).
//
//declscope:package // shared test fixture
func newFake() *fakeSource {
	f := &fakeSource{
		ids:    []int{11, 12, 31},
		cfgs:   map[int]*config.Operation{},
		states: map[int]string{11: "active", 12: "pending", 31: "active"},
		snaps:  map[int]*stats.Snapshot{},
		reacts: map[int][]react.ReactionState{},
	}
	f.cfgs[11] = echoCfg(11, "10.100.1.11")
	f.cfgs[12] = echoCfg(12, "fd00:100:2::11")
	f.cfgs[12].TrafficClass = 0x20
	f.cfgs[12].Schedule = &config.Schedule{Life: time.Hour, Start: config.StartPending}
	f.cfgs[31] = &config.Operation{
		ID: 31, Type: config.ICMPJitter, Target: netip.MustParseAddr("10.100.1.11"), Frequency: 10 * time.Second,
		Timeout: 2 * time.Second, Threshold: 100 * time.Millisecond, Interval: 20 * time.Millisecond, NumPackets: 10,
		Stats:   config.StatsConfig{HoursKept: 2, DistBuckets: 1, DistInterval: 20 * time.Millisecond},
		History: config.HistoryConfig{Buckets: 15, Filter: config.FilterNone},
	}
	lifeStart := base.Add(time.Minute) // agent start is base: 6000 ticks
	f.snaps[11] = &stats.Snapshot{
		ID: 11, Type: config.ICMPEcho, LifeStart: lifeStart, LifeIndex: 1,
		Latest: stats.Latest{Valid: true, Seq: 9, Start: base.Add(2 * time.Minute), End: base.Add(2*time.Minute + 3*time.Millisecond),
			RTT: 3700 * time.Microsecond, Code: op.RCOK},
		Totals: stats.Counters{Initiations: 9, Completions: 8, Timeouts: 1, RTTSumMs: 24, RTTSum2Ms: 80, RTTMinMs: 1, RTTMaxMs: 5},
		Hours: []stats.HourGroup{{
			Index: 1, Start: lifeStart,
			Counters: stats.Counters{Initiations: 9, Completions: 8, OverThresholds: 1, Timeouts: 1, Busies: 2, SequenceErrors: 1, RTTSumMs: 24, RTTSum2Ms: 80, RTTMinMs: 1, RTTMaxMs: 5},
			Dist: []stats.DistBucket{
				{Index: 1, LowerMs: 0, UpperMs: 5, Completions: 7, RTTSumMs: 19, RTTSum2Ms: 1<<32 + 55, RTTMinMs: 1, RTTMaxMs: 4},
				{Index: 2, LowerMs: 5, Completions: 1, OverThresholds: 1, RTTSumMs: 5, RTTSum2Ms: 25, RTTMinMs: 5, RTTMaxMs: 5},
			},
		}},
		History: []stats.HistoryBucket{
			{Life: 1, Bucket: 8, Sample: 1, Start: base.Add(110 * time.Second), RTTMs: 0, Code: op.RCTimeout, Target: netip.MustParseAddr("10.100.1.11")},
			{Life: 1, Bucket: 9, Sample: 1, Start: base.Add(120 * time.Second), RTTMs: 3, Code: op.RCOK, Target: netip.MustParseAddr("10.100.1.11")},
		},
	}
	f.snaps[12] = &stats.Snapshot{ID: 12, LifeIndex: 1, Latest: stats.Latest{Code: op.RCOther}}
	j := &op.JitterResult{
		NumPackets: 10, Sent: 10, NumRTT: 10, RTTSumMs: 30, RTTSum2Ms: 100, RTTMinMs: 2, RTTMaxMs: 5,
		PosSD: op.JitterSide{Num: 4, SumMs: 8, Sum2Ms: 20, MinMs: 1, MaxMs: 3}, NegDS: op.JitterSide{Num: 2, SumMs: 2, Sum2Ms: 2, MinMs: 1, MaxMs: 1},
		PktLoss: 1, PktLateArrival: 2, PktOutSeqSD: 1, MaxSucPktLoss: 1, MinSucPktLoss: 1,
	}
	f.snaps[31] = &stats.Snapshot{
		ID: 31, Type: config.ICMPJitter, LifeStart: lifeStart, LifeIndex: 1,
		Latest: stats.Latest{Valid: true, Seq: 3, End: base.Add(3 * time.Minute), RTT: 3 * time.Millisecond, Code: op.RCOK, Jitter: j},
		Totals: stats.Counters{Initiations: 3, Completions: 3},
		Hours: []stats.HourGroup{{
			Index: 1, Start: lifeStart, Counters: stats.Counters{Initiations: 3, Completions: 3, Busies: 1},
			Jitter: &stats.JitterCounters{NumRTT: 30, RTTSumMs: 90, RTTSum2Ms: 1<<33 + 7, RTTMinMs: 2, RTTMaxMs: 6, PosSD: op.JitterSide{Num: 12, SumMs: 24, MinMs: 1, MaxMs: 3}, PktLoss: 3, PktLateArrival: 4},
		}},
	}
	f.reacts[11] = []react.ReactionState{
		{Element: "rtt", ThresholdType: "consecutive", Upper: 300, Lower: 200, Count: 3, X: 5, Y: 5, Action: "trap", Occurred: true, Value: 350},
		{Element: "timeout", ThresholdType: "xofy", Count: 5, X: 2, Y: 4, Action: "syslog"},
	}
	return f
}

// newTestMIB is a tree over src whose daemon started at base.
//
//declscope:package // shared test fixture
func newTestMIB(src Source, clk clock.Clock) *mib {
	return newMIB(src, Options{Version: "1.2.3", Start: base, Logger: quiet(), Clock: clk})
}
