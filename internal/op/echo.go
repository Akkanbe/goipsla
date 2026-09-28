package op

import (
	"context"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/probe"
)

// NewEcho returns the Runner of an icmp-echo operation. cfg must not be
// modified while the Runner is in use.
func NewEcho(cfg *config.Operation, eng probe.Engine, clk clock.Clock) Runner {
	return &echoRunner{cfg: cfg, eng: eng, clk: clk}
}

type echoRunner struct {
	cfg *config.Operation
	eng probe.Engine
	clk clock.Clock
}

func (r *echoRunner) Run(ctx context.Context, seq uint32, start time.Time) Result {
	reply := r.eng.Echo(ctx, echoRequest(r.cfg, seq))
	res := classifyEcho(r.cfg, reply)
	res.OpID = r.cfg.ID
	res.Type = r.cfg.Type
	res.Seq = seq
	res.Start = start
	res.End = r.clk.Now()
	return res
}

// echoRequest builds the probe request of attempt seq.
func echoRequest(cfg *config.Operation, seq uint32) probe.Request {
	tos := cfg.TOS
	if cfg.Target.Is6() && !cfg.Target.Is4In6() {
		tos = cfg.TrafficClass
	}
	return probe.Request{
		OpID:      uint32(cfg.ID),
		Seq:       seq,
		Target:    cfg.Target,
		Source:    cfg.SourceIP,
		Interface: cfg.SourceInterface,
		VRF:       cfg.VRF,
		TOS:       tos,
		FlowLabel: cfg.FlowLabel,
		DataSize:  cfg.RequestDataSize,
		Pattern:   cfg.DataPattern,
		Verify:    cfg.VerifyData,
		Timeout:   cfg.Timeout,
	}
}

// classifyEcho maps an engine reply to a return code, RTT and detail.
//
//   - A reply is ok, or overThreshold when its RTT is strictly greater than
//     the threshold (implementation plan section 7).
//   - An ICMP error ends the attempt early and is reported as timeout with
//     the error in Detail (implementation plan section 7).
//   - A local failure is error(16), "socket failures or some other errors
//     not relevant to the actual probe" in CISCO-RTTMON-TC-MIB.
func classifyEcho(cfg *config.Operation, reply probe.Reply) Result {
	res := Result{Detail: reply.Detail}
	switch reply.Outcome {
	case probe.OutcomeReply:
		res.RTT = reply.RTT
		res.Code = RCOK
		if reply.RTT > cfg.Threshold {
			res.Code = RCOverThreshold
		}
	case probe.OutcomeTimeout, probe.OutcomeUnreachable:
		res.Code = RCTimeout
	case probe.OutcomeVerifyError:
		res.Code = RCVerifyError
	case probe.OutcomeError:
		res.Code = RCError
		if reply.Err != nil {
			res.Detail = reply.Err.Error()
		}
	default:
		res.Code = RCOther
	}
	return res
}
