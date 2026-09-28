package op

import (
	"context"
	"sync"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/probe"
)

// fakeEngine records requests and returns a canned reply, advancing the fake
// clock by the reply's RTT to mimic the time spent waiting.
//
//declscope:package // the fake engine of every op test (echo and jitter)
type fakeEngine struct {
	clk    *clock.Fake
	reply  probe.Reply           // what Echo returns
	reqs   []probe.Request       // the Echo requests received
	jitter probe.JitterReply     // the burst Jitter returns
	jreqs  []probe.JitterRequest // the Jitter requests received

	//declscope:private
	mu sync.Mutex
}

func (e *fakeEngine) Echo(_ context.Context, req probe.Request) probe.Reply {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, req)
	if e.clk != nil {
		e.clk.Advance(e.reply.RTT)
	}
	return e.reply
}

func (e *fakeEngine) Jitter(_ context.Context, req probe.JitterRequest) probe.JitterReply {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.jreqs = append(e.jreqs, req)
	if e.clk != nil {
		e.clk.Advance(req.Interval * time.Duration(max(req.NumPackets-1, 0)))
	}
	return e.jitter
}

func (e *fakeEngine) Close() error { return nil }

func (e *fakeEngine) Stats() probe.Stats { return probe.Stats{} }

func (e *fakeEngine) Forget(uint32) {}
