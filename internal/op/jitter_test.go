package op

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/probe"
)

// jitterStart is 10:00 UTC, far from midnight.
var jitterStart = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

// jitterBurst returns n answered packets sent every 20 ms with an RTT of 2 ms, a
// source-to-destination delay of 1 ms (R = O + 1) and no processing time on
// the target (T = R), arriving in order.
func jitterBurst(n int) []probe.JitterPacket {
	pks := make([]probe.JitterPacket, n)
	for i := range pks {
		s := jitterStart.Add(time.Duration(i) * 20 * time.Millisecond)
		o := timestampOf(s)
		pks[i] = probe.JitterPacket{
			Index:      i,
			Outcome:    probe.OutcomeReply,
			SentAt:     s,
			ReceivedAt: s.Add(2 * time.Millisecond),
			RTT:        2 * time.Millisecond,
			Originate:  o,
			Receive:    o + 1,
			Transmit:   o + 1,
			ArrivalPos: i,
		}
	}
	return pks
}

func loseJitterPackets(pks []probe.JitterPacket, idx ...int) []probe.JitterPacket {
	for _, i := range idx {
		pks[i] = probe.JitterPacket{Index: i, Outcome: probe.OutcomeTimeout, SentAt: pks[i].SentAt, Originate: pks[i].Originate}
	}
	// Renumber the arrival order of the remaining replies.
	pos := 0
	for i := range pks {
		if pks[i].Outcome == probe.OutcomeReply {
			pks[i].ArrivalPos = pos
			pos++
		}
	}
	return pks
}

// zeroJitterSamples is a JitterSide of n zero samples.
func zeroJitterSamples(n uint64) JitterSide { return JitterSide{Num: n} }

func TestComputeJitter(t *testing.T) {
	params := jitterParams{threshold: 5 * time.Second}
	for _, tc := range []struct {
		name   string
		params jitterParams
		pks    func() []probe.JitterPacket
		check  func(t *testing.T, jr JitterResult)
	}{
		{
			// Cisco SAA example: (82 − 20) − (60 − 0) = 2 ms positive SD jitter.
			name: "saa example",
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(2)
				pks[0].Originate, pks[1].Originate = 1_000_000, 1_000_060
				pks[0].Receive, pks[1].Receive = 1_000_020, 1_000_082
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PosSD != (JitterSide{Num: 1, SumMs: 2, Sum2Ms: 4, MinMs: 2, MaxMs: 2}) || jr.NegSD.Num != 0 {
					t.Errorf("SD = %+v / %+v", jr.PosSD, jr.NegSD)
				}
			},
		},
		{
			// The IOS XE output: 10 packets, all answered, 9 SD and 9 DS samples
			// with Min 0.
			name: "ten packets all answered",
			pks:  func() []probe.JitterPacket { return jitterBurst(10) },
			check: func(t *testing.T, jr JitterResult) {
				want := JitterResult{
					NumPackets: 10, Sent: 10, NumRTT: 10, RTTSumMs: 20, RTTSum2Ms: 40, RTTMinMs: 2, RTTMaxMs: 2,
					PosSD: zeroJitterSamples(9), PosDS: zeroJitterSamples(9),
				}
				if diff := cmp.Diff(want, jr); diff != "" {
					t.Errorf("(-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "one loss",
			pks:  func() []probe.JitterPacket { return loseJitterPackets(jitterBurst(10), 4) },
			check: func(t *testing.T, jr JitterResult) {
				// Pairs (0,1) (1,2) (2,3) (5,6) (6,7) (7,8) (8,9).
				if jr.PktLoss != 1 || jr.MinSucPktLoss != 1 || jr.MaxSucPktLoss != 1 || jr.PosSD.Num != 7 || jr.PosDS.Num != 7 || jr.NumRTT != 9 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "three successive losses",
			pks:  func() []probe.JitterPacket { return loseJitterPackets(jitterBurst(10), 3, 4, 5) },
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktLoss != 3 || jr.MinSucPktLoss != 3 || jr.MaxSucPktLoss != 3 || jr.PosSD.Num != 5 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "runs of one and three",
			pks:  func() []probe.JitterPacket { return loseJitterPackets(jitterBurst(10), 1, 5, 6, 7) },
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktLoss != 4 || jr.MinSucPktLoss != 1 || jr.MaxSucPktLoss != 3 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "tail loss",
			pks:  func() []probe.JitterPacket { return loseJitterPackets(jitterBurst(10), 9) },
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktLoss != 1 || jr.MinSucPktLoss != 1 || jr.MaxSucPktLoss != 1 || jr.PosSD.Num != 8 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "head loss",
			pks:  func() []probe.JitterPacket { return loseJitterPackets(jitterBurst(10), 0) },
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktLoss != 1 || jr.MinSucPktLoss != 1 || jr.PosSD.Num != 8 || jr.NumRTT != 9 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "all lost",
			pks:  func() []probe.JitterPacket { return loseJitterPackets(jitterBurst(10), 0, 1, 2, 3, 4, 5, 6, 7, 8, 9) },
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktLoss != 10 || jr.NumRTT != 0 || jr.MinSucPktLoss != 10 || jr.MaxSucPktLoss != 10 || jr.PosSD.Num != 0 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			// R and O wrap at midnight UT: 86,399,990 → 10 and 86,399,995 → 15.
			name:   "midnight wrap",
			params: jitterParams{threshold: 5 * time.Second, oneWay: true},
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(2)
				pks[0].Originate, pks[1].Originate = 86_399_985, 5
				pks[0].Receive, pks[1].Receive = 86_399_990, 10
				pks[0].Transmit, pks[1].Transmit = 86_399_990, 10
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PosSD != (JitterSide{Num: 1}) || jr.NegSD.Num != 0 {
					t.Errorf("SD = %+v / %+v", jr.PosSD, jr.NegSD)
				}
				if jr.PktOutSeqSD != 0 || jr.PktOutSeqBoth != 0 {
					t.Errorf("wrap taken as reordering: %+v", jr)
				}
				if jr.OWSD.Num != 2 || jr.OWSD.SumMs != 10 {
					t.Errorf("one-way SD = %+v", jr.OWSD)
				}
			},
		},
		{
			// A non-standard Receive (high bit set) still yields jitter from
			// the masked value but no one-way sample.
			name:   "non-standard flag",
			params: jitterParams{threshold: 5 * time.Second, oneWay: true},
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(3)
				pks[1].Receive |= timestampNonStandard
				pks[1].Transmit |= timestampNonStandard
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PosSD.Num != 2 || jr.PosSD.SumMs != 0 || jr.PosDS.Num != 2 || jr.PosDS.SumMs != 0 {
					t.Errorf("jitter = %+v / %+v", jr.PosSD, jr.PosDS)
				}
				if jr.NumOW != 2 || !jr.OneWay {
					t.Errorf("one-way samples = %d", jr.NumOW)
				}
			},
		},
		{
			name:   "one-way delays",
			params: jitterParams{threshold: 5 * time.Second, oneWay: true},
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(3)
				// Target clock 10 s behind: negative SD delay, not a sample.
				pks[2].Receive -= 10_000
				pks[2].Transmit -= 10_000
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				// SD = R − O = 1 ms; DS = A − T = (S + 2) − (O + 1) = 1 ms.
				want1 := JitterSide{Num: 2, SumMs: 2, Sum2Ms: 2, MinMs: 1, MaxMs: 1}
				if !jr.OneWay || jr.NumOW != 2 || jr.OWSD != want1 || jr.OWDS != want1 {
					t.Errorf("one-way = %v %d %+v %+v", jr.OneWay, jr.NumOW, jr.OWSD, jr.OWDS)
				}
			},
		},
		{
			name: "one-way disabled",
			pks:  func() []probe.JitterPacket { return jitterBurst(3) },
			check: func(t *testing.T, jr JitterResult) {
				if jr.OneWay || jr.NumOW != 0 || jr.OWSD.Num != 0 {
					t.Errorf("one-way accumulated: %+v", jr)
				}
			},
		},
		{
			// Packet 1 reached the target after packet 2 (R reversed), but the
			// replies came back in send order.
			name: "reorder source to destination",
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(3)
				pks[1].Receive, pks[2].Receive = pks[2].Receive+2, pks[1].Receive+20
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktOutSeqSD != 1 || jr.PktOutSeqDS != 0 || jr.PktOutSeqBoth != 0 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "reorder destination to source",
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(3)
				pks[1].ArrivalPos, pks[2].ArrivalPos = 2, 1
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktOutSeqSD != 0 || jr.PktOutSeqDS != 1 || jr.PktOutSeqBoth != 0 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "reorder both",
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(3)
				pks[1].Receive, pks[2].Receive = pks[2].Receive+2, pks[1].Receive+20
				pks[1].ArrivalPos, pks[2].ArrivalPos = 2, 1
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktOutSeqSD != 0 || jr.PktOutSeqDS != 0 || jr.PktOutSeqBoth != 1 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "skipped packets are excluded",
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(10)
				pks[2] = probe.JitterPacket{Index: 2, Outcome: probe.OutcomeError, Err: probe.ErrSkipped}
				pks[6] = probe.JitterPacket{Index: 6, Outcome: probe.OutcomeTimeout}
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				// Skipped 2 breaks pairs (1,2) and (2,3); loss 6 breaks (5,6) and (6,7).
				if jr.Skipped != 1 || jr.Sent != 9 || jr.PktLoss != 1 || jr.NumRTT != 8 || jr.PosSD.Num != 5 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "skipped does not break a loss run",
			pks: func() []probe.JitterPacket {
				pks := loseJitterPackets(jitterBurst(5), 1, 3)
				pks[2] = probe.JitterPacket{Index: 2, Outcome: probe.OutcomeError, Err: probe.ErrSkipped}
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktLoss != 2 || jr.MinSucPktLoss != 2 || jr.MaxSucPktLoss != 2 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "late and unreachable",
			pks: func() []probe.JitterPacket {
				pks := loseJitterPackets(jitterBurst(4), 1, 2)
				pks[1].Late = true
				pks[2].Outcome = probe.OutcomeUnreachable
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PktLoss != 2 || jr.PktLateArrival != 1 || jr.MaxSucPktLoss != 2 {
					t.Errorf("%+v", jr)
				}
			},
		},
		{
			name: "jitter signs and averages",
			pks: func() []probe.JitterPacket {
				pks := jitterBurst(4)
				// SD: +3, −2 (R_2 − R_1 = 18), +4.
				pks[1].Receive += 3
				pks[2].Receive++
				pks[3].Receive += 5
				// DS: packet 2 arrives 6 ms later than the grid.
				pks[2].ReceivedAt = pks[2].ReceivedAt.Add(6 * time.Millisecond)
				return pks
			},
			check: func(t *testing.T, jr JitterResult) {
				if jr.PosSD != (JitterSide{Num: 2, SumMs: 7, Sum2Ms: 25, MinMs: 3, MaxMs: 4}) ||
					jr.NegSD != (JitterSide{Num: 1, SumMs: 2, Sum2Ms: 4, MinMs: 2, MaxMs: 2}) {
					t.Errorf("SD = %+v / %+v", jr.PosSD, jr.NegSD)
				}
				// T_i = O_i + 1 is unchanged, so T advances by 20 ms per packet:
				// DS_1 = 20 − 20 = 0, DS_2 = 26 − 20 = 6, DS_3 = 14 − 20 = −6.
				if jr.PosDS != (JitterSide{Num: 2, SumMs: 6, Sum2Ms: 36, MinMs: 0, MaxMs: 6}) ||
					jr.NegDS != (JitterSide{Num: 1, SumMs: 6, Sum2Ms: 36, MinMs: 6, MaxMs: 6}) {
					t.Errorf("DS = %+v / %+v", jr.PosDS, jr.NegDS)
				}
				if jr.AvgSDJitterMs() != 3 || jr.AvgDSJitterMs() != 4 || jr.AvgJitterMs() != 3.5 {
					t.Errorf("avg = %v %v %v", jr.AvgJitterMs(), jr.AvgSDJitterMs(), jr.AvgDSJitterMs())
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.params
			if p == (jitterParams{}) {
				p = params
			}
			jr, _ := computeJitter(p, probe.JitterReply{Packets: tc.pks()})
			tc.check(t, jr)
		})
	}
}

func jitterCfg() *config.Operation {
	return &config.Operation{
		ID:         301,
		Type:       config.ICMPJitter,
		Target:     netip.MustParseAddr("10.100.2.11"),
		SourceIP:   netip.MustParseAddr("10.100.1.10"),
		VRF:        "blue",
		TOS:        0xb8,
		Timeout:    5 * time.Second,
		Threshold:  3 * time.Millisecond,
		Frequency:  60 * time.Second,
		Interval:   20 * time.Millisecond,
		NumPackets: 10,
	}
}

func TestClassifyJitter(t *testing.T) {
	cfg := jitterCfg()
	for _, tc := range []struct {
		name   string
		rep    probe.JitterReply
		code   ReturnCode
		rtt    time.Duration
		detail string
	}{
		{"all answered", probe.JitterReply{Packets: jitterBurst(10)}, RCOK, 2 * time.Millisecond, ""},
		{"loss", probe.JitterReply{Packets: loseJitterPackets(jitterBurst(10), 3, 4, 5)}, RCOK, 2 * time.Millisecond, "loss 3/10"},
		{"all lost", probe.JitterReply{Packets: loseJitterPackets(jitterBurst(10), 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)}, RCTimeout, 0, "loss 10/10"},
		{
			"mean over threshold",
			probe.JitterReply{Packets: func() []probe.JitterPacket {
				pks := jitterBurst(2)
				pks[0].RTT, pks[1].RTT = 2*time.Millisecond, 5*time.Millisecond // mean 3.5 ms > 3 ms
				return pks
			}()},
			RCOverThreshold, 3500 * time.Microsecond, "",
		},
		{
			"mean at threshold",
			probe.JitterReply{Packets: func() []probe.JitterPacket {
				pks := jitterBurst(2)
				pks[0].RTT, pks[1].RTT = 2*time.Millisecond, 4*time.Millisecond
				return pks
			}()},
			RCOK, 3 * time.Millisecond, "",
		},
		{
			"unreachable",
			probe.JitterReply{Packets: func() []probe.JitterPacket {
				pks := loseJitterPackets(jitterBurst(2), 0, 1)
				for i := range pks {
					pks[i].Outcome = probe.OutcomeUnreachable
					pks[i].Detail = "destination unreachable: host unreachable, from 10.100.1.254"
				}
				return pks
			}()},
			RCTimeout, 0, "loss 2/2; destination unreachable: host unreachable, from 10.100.1.254",
		},
		{
			"nothing sent",
			probe.JitterReply{Err: errors.New("network is unreachable"), Packets: []probe.JitterPacket{{Outcome: probe.OutcomeError}}},
			RCError, 0, "network is unreachable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyJitter(cfg, tc.rep)
			if res.Code != tc.code || res.RTT != tc.rtt || res.Detail != tc.detail || res.Jitter == nil {
				t.Errorf("result = code %v rtt %v detail %q jitter %v", res.Code, res.RTT, res.Detail, res.Jitter)
			}
		})
	}
}

func TestJitterRunner(t *testing.T) {
	cfg := jitterCfg()
	clk := clock.NewFake(jitterStart)
	eng := &fakeEngine{clk: clk, jitter: probe.JitterReply{Packets: jitterBurst(10)}}
	res := NewJitter(cfg, eng, clk).Run(t.Context(), 7, jitterStart)
	want := probe.JitterRequest{
		OpID: 301, Seq: 7, Target: cfg.Target, Source: cfg.SourceIP, VRF: "blue", TOS: 0xb8,
		NumPackets: 10, Interval: 20 * time.Millisecond, Timeout: 5 * time.Second,
	}
	if len(eng.jreqs) != 1 || eng.jreqs[0] != want {
		t.Fatalf("requests = %+v", eng.jreqs)
	}
	if res.OpID != 301 || res.Type != config.ICMPJitter || res.Seq != 7 || !res.Start.Equal(jitterStart) ||
		!res.End.Equal(jitterStart.Add(180*time.Millisecond)) || res.Code != RCOK || res.Jitter == nil || res.Jitter.NumRTT != 10 {
		t.Errorf("result = %+v", res)
	}
}

func TestOneWayDelaySetting(t *testing.T) {
	cfg := jitterCfg()
	if res := classifyJitter(cfg, probe.JitterReply{Packets: jitterBurst(3)}); res.Jitter.OneWay || res.Jitter.NumOW != 0 {
		t.Errorf("disabled: %+v", res.Jitter)
	}
	cfg.OneWayDelay = true
	res := classifyJitter(cfg, probe.JitterReply{Packets: jitterBurst(3)})
	want := JitterSide{Num: 3, SumMs: 3, Sum2Ms: 3, MinMs: 1, MaxMs: 1}
	if !res.Jitter.OneWay || res.Jitter.NumOW != 3 || res.Jitter.OWSD != want || res.Jitter.OWDS != want {
		t.Errorf("enabled: %+v", res.Jitter)
	}
}
