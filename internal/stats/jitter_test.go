// A test of the store; it shares the store's namespace.
//
//declscope:namespace store

package stats

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/config"
	"goipsla/internal/op"
)

func jitterOpCfg() *config.Operation {
	cfg := opCfg(1)
	cfg.Type = config.ICMPJitter
	cfg.Stats.DistBuckets = 5
	cfg.Stats.DistInterval = 10 * time.Millisecond
	cfg.History = config.HistoryConfig{Lives: 2, Buckets: 15, Filter: "all"} // ignored for jitter
	return cfg
}

// burst returns a jitter result of 10 packets with the given losses and
// successive-loss run lengths.
func burst(rttMs uint64, loss, minSuc, maxSuc uint64) *op.JitterResult {
	n := 10 - loss
	return &op.JitterResult{
		NumPackets: 10, Sent: 10,
		NumRTT: n, RTTSumMs: n * rttMs, RTTSum2Ms: n * rttMs * rttMs, RTTMinMs: rttMs, RTTMaxMs: rttMs,
		PosSD:   op.JitterSide{Num: 5, SumMs: 5, Sum2Ms: 5, MinMs: 1, MaxMs: 1},
		NegSD:   op.JitterSide{Num: 4, SumMs: 8, Sum2Ms: 16, MinMs: 2, MaxMs: 2},
		PosDS:   op.JitterSide{Num: 9},
		PktLoss: loss, MinSucPktLoss: minSuc, MaxSucPktLoss: maxSuc,
	}
}

func jitterRes(seq uint32, at time.Duration, code op.ReturnCode, rtt time.Duration, jr *op.JitterResult) op.Result {
	r := res(seq, at, code, rtt)
	r.Type = config.ICMPJitter
	r.Jitter = jr
	return r
}

func TestJitterAccounting(t *testing.T) {
	s := newStore(t, jitterOpCfg())
	b1 := burst(2, 0, 0, 0)
	b2 := burst(6, 3, 1, 2)
	b2.PktLateArrival, b2.PktOutSeqSD, b2.PktOutSeqDS, b2.PktOutSeqBoth, b2.Skipped, b2.NumOverThreshold = 1, 2, 3, 4, 1, 7
	b2.NumOW = 7
	b2.OWSD = op.JitterSide{Num: 7, SumMs: 14, Sum2Ms: 28, MinMs: 2, MaxMs: 2}
	b3 := burst(1, 4, 4, 4)
	s.Record(jitterRes(1, 0, op.RCOK, 2*time.Millisecond, b1))
	s.Record(jitterRes(2, time.Minute, op.RCOverThreshold, 6*time.Millisecond, b2))
	s.Record(jitterRes(3, 2*time.Minute, op.RCOK, time.Millisecond, b3))
	s.Record(jitterRes(0, 3*time.Minute, op.RCBusy, 0, nil))
	s.Record(jitterRes(3, 3*time.Minute, op.RCSequenceError, 0, nil))

	sn := snap(t, s, 1)
	want := &JitterCounters{
		NumRTT: 10 + 7 + 6, RTTSumMs: 20 + 42 + 6, RTTSum2Ms: 40 + 252 + 6, RTTMinMs: 1, RTTMaxMs: 6,
		NumOverThreshold: 7,
		PosSD:            op.JitterSide{Num: 15, SumMs: 15, Sum2Ms: 15, MinMs: 1, MaxMs: 1},
		NegSD:            op.JitterSide{Num: 12, SumMs: 24, Sum2Ms: 48, MinMs: 2, MaxMs: 2},
		PosDS:            op.JitterSide{Num: 27},
		PktLoss:          7, PktLateArrival: 1, PktOutSeqSD: 2, PktOutSeqDS: 3, PktOutSeqBoth: 4,
		MinSucPktLoss: 1, MaxSucPktLoss: 4, // the loss-free burst does not pull the minimum to 0
		Skipped: 1,
		NumOW:   7,
		OWSD:    op.JitterSide{Num: 7, SumMs: 14, Sum2Ms: 28, MinMs: 2, MaxMs: 2},
	}
	if diff := cmp.Diff(want, sn.TotalsJitter); diff != "" {
		t.Errorf("totals jitter (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want, sn.Hours[0].Jitter); diff != "" {
		t.Errorf("hour group jitter (-want +got):\n%s", diff)
	}
	if got := sn.TotalsJitter.AvgSDJitterMs(); got != 39.0/27 {
		t.Errorf("avg SD = %v", got)
	}
	// Burst-level counters: 3 initiations, 3 completions, 1 over threshold.
	if sn.Totals.Initiations != 3 || sn.Totals.Completions != 3 || sn.Totals.OverThresholds != 1 ||
		sn.Totals.Busies != 1 || sn.Totals.SequenceErrors != 1 || sn.Totals.RTTSumMs != 9 {
		t.Errorf("totals = %+v", sn.Totals)
	}
	// Latest holds a copy of the last burst.
	if sn.Latest.Jitter == nil || *sn.Latest.Jitter != *b3 || sn.Latest.Jitter == b3 {
		t.Errorf("latest jitter = %+v", sn.Latest.Jitter)
	}
	// No distribution and no history for jitter.
	if sn.Hours[0].Dist != nil {
		t.Errorf("dist = %+v", sn.Hours[0].Dist)
	}
	if len(sn.History) != 0 {
		t.Errorf("history = %+v", sn.History)
	}
}

func TestJitterTimeoutBurst(t *testing.T) {
	s := newStore(t, jitterOpCfg())
	b := &op.JitterResult{NumPackets: 10, Sent: 10, PktLoss: 10, MinSucPktLoss: 10, MaxSucPktLoss: 10}
	s.Record(jitterRes(1, 0, op.RCTimeout, 0, b))
	sn := snap(t, s, 1)
	if sn.Totals.Timeouts != 1 || sn.Totals.Failures() != 1 {
		t.Errorf("totals = %+v", sn.Totals)
	}
	j := sn.TotalsJitter
	if j.PktLoss != 10 || j.NumRTT != 0 || j.RTTMinMs != 0 || j.MinSucPktLoss != 10 {
		t.Errorf("jitter = %+v", j)
	}
}

func TestJitterHourRotationAndReset(t *testing.T) {
	s := newStore(t, jitterOpCfg())
	s.Record(jitterRes(1, 0, op.RCOK, 2*time.Millisecond, burst(2, 0, 0, 0)))
	s.Record(jitterRes(2, time.Hour, op.RCOK, 2*time.Millisecond, burst(2, 1, 1, 1)))
	sn := snap(t, s, 1)
	if len(sn.Hours) != 2 || sn.Hours[0].Jitter.PktLoss != 0 || sn.Hours[1].Jitter.PktLoss != 1 || sn.TotalsJitter.PktLoss != 1 {
		t.Fatalf("hours = %+v", sn.Hours)
	}
	s.Reset(1, t0.Add(2*time.Hour))
	sn = snap(t, s, 1)
	if sn.TotalsJitter == nil || *sn.TotalsJitter != (JitterCounters{}) || sn.Latest.Jitter != nil || len(sn.Hours) != 0 {
		t.Errorf("after reset: %+v", sn)
	}
}

func TestJitterCopies(t *testing.T) {
	s := newStore(t, jitterOpCfg())
	s.Record(jitterRes(1, 0, op.RCOK, 2*time.Millisecond, burst(2, 0, 0, 0)))
	sn := snap(t, s, 1)
	sn.TotalsJitter.NumRTT = 99
	sn.Hours[0].Jitter.NumRTT = 99
	sn.Latest.Jitter.NumRTT = 99
	rows := s.Summary()
	rows[0].Latest.Jitter.NumRTT = 99
	again := snap(t, s, 1)
	if again.TotalsJitter.NumRTT != 10 || again.Hours[0].Jitter.NumRTT != 10 || again.Latest.Jitter.NumRTT != 10 {
		t.Errorf("snapshot shares memory: %+v", again)
	}
	if s.Summary()[0].Latest.Jitter.NumRTT != 10 {
		t.Error("summary shares memory")
	}
}

func TestEchoHasNoJitter(t *testing.T) {
	s := newStore(t, opCfg(1))
	r := res(1, 0, op.RCOK, time.Millisecond)
	r.Jitter = burst(1, 0, 0, 0) // ignored for echo
	s.Record(r)
	sn := snap(t, s, 1)
	if sn.TotalsJitter != nil || sn.Hours[0].Jitter != nil || sn.Latest.Jitter != nil || sn.Hours[0].Dist == nil {
		t.Errorf("echo snapshot = %+v", sn)
	}
}
