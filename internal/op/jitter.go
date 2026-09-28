package op

import (
	"context"
	"fmt"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/probe"
)

// NewJitter returns the Runner of an icmp-jitter operation. cfg must not be
// modified while the Runner is in use.
func NewJitter(cfg *config.Operation, eng probe.Engine, clk clock.Clock) Runner {
	return &jitterRunner{cfg: cfg, eng: eng, clk: clk}
}

type jitterRunner struct {
	cfg *config.Operation
	eng probe.Engine
	clk clock.Clock
}

func (r *jitterRunner) Run(ctx context.Context, seq uint32, start time.Time) Result {
	rep := r.eng.Jitter(ctx, jitterRequest(r.cfg, seq))
	res := classifyJitter(r.cfg, rep)
	res.OpID = r.cfg.ID
	res.Type = r.cfg.Type
	res.Seq = seq
	res.Start = start
	res.End = r.clk.Now()
	return res
}

// jitterRequest builds the probe request of burst seq.
func jitterRequest(cfg *config.Operation, seq uint32) probe.JitterRequest {
	return probe.JitterRequest{
		OpID:       uint32(cfg.ID),
		Seq:        seq,
		Target:     cfg.Target,
		Source:     cfg.SourceIP,
		Interface:  cfg.SourceInterface,
		VRF:        cfg.VRF,
		TOS:        cfg.TOS,
		NumPackets: cfg.NumPackets,
		Interval:   cfg.Interval,
		Timeout:    cfg.Timeout,
	}
}

// jitterOneWay reports whether one-way delays are accumulated for cfg (the
// one-way-delay setting, false by default).
func jitterOneWay(cfg *config.Operation) bool { return cfg.OneWayDelay }

// jitterParams are the settings computeJitter depends on.
type jitterParams struct {
	threshold time.Duration
	oneWay    bool
}

// classifyJitter computes the burst statistics and the burst's return code
// (docs/statistics.md, "Per-burst accounting"):
//
//   - error when no packet could be sent because of a local failure,
//   - timeout when no packet was answered,
//   - otherwise ok, or overThreshold when the mean RTT exceeds the threshold.
//
// Result.RTT is the mean RTT of the replies ("Latest RTT value is equal to
// the average RTT value").
func classifyJitter(cfg *config.Operation, rep probe.JitterReply) Result {
	jr, rttSum := computeJitter(jitterParams{threshold: cfg.Threshold, oneWay: jitterOneWay(cfg)}, rep)
	res := Result{Jitter: &jr}
	switch {
	case jr.Sent == 0:
		res.Code = RCError
		if rep.Err != nil {
			res.Detail = rep.Err.Error()
		} else {
			res.Detail = "no packet sent"
		}
		return res
	case jr.NumRTT == 0:
		res.Code = RCTimeout
	default:
		res.RTT = rttSum / time.Duration(jr.NumRTT)
		res.Code = RCOK
		if res.RTT > cfg.Threshold {
			res.Code = RCOverThreshold
		}
	}
	res.Detail = jitterDetail(&jr, rep)
	return res
}

// jitterDetail summarizes losses, e.g. "loss 3/10" or
// "loss 10/10; destination unreachable: host unreachable, from 10.100.1.254".
func jitterDetail(jr *JitterResult, rep probe.JitterReply) string {
	if jr.PktLoss == 0 {
		return ""
	}
	d := fmt.Sprintf("loss %d/%d", jr.PktLoss, jr.Sent)
	for _, pk := range rep.Packets {
		if pk.Outcome == probe.OutcomeUnreachable && pk.Detail != "" {
			return d + "; " + pk.Detail
		}
	}
	return d
}

// computeJitter derives the burst statistics from the packets
// (docs/statistics.md, "Per-packet values"). It also returns the sum of the reply
// RTTs in nanoseconds for the mean.
//
// Packets that were not sent (OutcomeError: skipped or failed) are excluded
// from every count except Skipped: they neither lose nor break a run of
// losses, and a jitter sample needs two packets with adjacent indexes that
// were both answered.
func computeJitter(p jitterParams, rep probe.JitterReply) (JitterResult, time.Duration) {
	var (
		jr      = JitterResult{NumPackets: len(rep.Packets)}
		rttSum  time.Duration
		run     uint64 // current run of successive losses
		prev    *probe.JitterPacket
		prevIdx = -2 // index of prev
	)
	endRun := func() {
		if run == 0 {
			return
		}
		if jr.MinSucPktLoss == 0 || run < jr.MinSucPktLoss {
			jr.MinSucPktLoss = run
		}
		jr.MaxSucPktLoss = max(jr.MaxSucPktLoss, run)
		run = 0
	}
	var lastReply *probe.JitterPacket // previous reply in index order, for reordering
	for i := range rep.Packets {
		pk := &rep.Packets[i]
		if pk.Outcome == probe.OutcomeError {
			jr.Skipped++
			continue
		}
		jr.Sent++
		if pk.Late {
			jr.PktLateArrival++
		}
		if pk.Outcome != probe.OutcomeReply {
			jr.PktLoss++
			run++
			continue
		}
		endRun()

		ms := uint64(0)
		if pk.RTT > 0 {
			ms = uint64(pk.RTT / time.Millisecond)
		}
		rttSum += pk.RTT
		if jr.NumRTT == 0 || ms < jr.RTTMinMs {
			jr.RTTMinMs = ms
		}
		jr.RTTMaxMs = max(jr.RTTMaxMs, ms)
		jr.NumRTT++
		jr.RTTSumMs += ms
		jr.RTTSum2Ms += ms * ms
		if pk.RTT > p.threshold {
			jr.NumOverThreshold++
		}

		// Jitter needs the previous packet (adjacent index) to be answered.
		if prev != nil && prevIdx == pk.Index-1 {
			sd := timestampDiff(pk.Receive, prev.Receive) - timestampDiff(pk.Originate, prev.Originate)
			addJitter(&jr.PosSD, &jr.NegSD, sd)
			ds := (pk.ReceivedAt.UnixMilli() - prev.ReceivedAt.UnixMilli()) - timestampDiff(pk.Transmit, prev.Transmit)
			addJitter(&jr.PosDS, &jr.NegDS, ds)
		}
		prev, prevIdx = pk, pk.Index

		// Reordering against the previous reply in send order.
		if lastReply != nil {
			rRev := timestampDiff(pk.Receive, lastReply.Receive) < 0
			aRev := pk.ArrivalPos < lastReply.ArrivalPos
			switch {
			case rRev && aRev:
				jr.PktOutSeqBoth++
			case rRev:
				jr.PktOutSeqSD++
			case aRev:
				jr.PktOutSeqDS++
			}
		}
		lastReply = pk

		if p.oneWay && pk.Receive&timestampNonStandard == 0 && pk.Transmit&timestampNonStandard == 0 {
			sd := timestampDiff(pk.Receive, pk.Originate)
			ds := timestampDiff(timestampOf(pk.ReceivedAt), pk.Transmit)
			// A negative delay means the clocks are not synchronized; such a
			// reply is not a one-way sample.
			if sd >= 0 && ds >= 0 {
				jr.NumOW++
				jr.OWSD.Add(uint64(sd))
				jr.OWDS.Add(uint64(ds))
			}
		}
	}
	endRun()
	jr.OneWay = jr.NumOW > 0
	return jr, rttSum
}

// addJitter files a jitter sample: zero and positive values as positive,
// negative values as their absolute value.
func addJitter(pos, neg *JitterSide, v int64) {
	if v >= 0 {
		pos.Add(uint64(v))
	} else {
		neg.Add(uint64(-v))
	}
}
