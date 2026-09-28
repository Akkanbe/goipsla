// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNewLifeSupersedesWaitingRequest reproduces audit A3: the manager
// restarts a life (ip sla restart, goipsla reset) by canceling the ctx of
// the attempt in flight and starting Seq 1 of the new life at once, without
// waiting for the old Echo to return. If the old attempt was Seq 1 too and
// its goroutine has not yet run its ctx.Done branch, its entry is still
// waiting under the same (Identifier, Sequence) when the new attempt
// registers. The test holds that window open by starting the new attempt
// before canceling the old one.
//
// The new attempt must go out and complete normally; the old one ends with
// ErrSuperseded.
func TestNewLifeSupersedesWaitingRequest(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)

	oldCtx, cancelOld := context.WithCancel(t.Context())
	defer cancelOld()
	oldCh := make(chan Reply, 1)
	go func() { oldCh <- h.e.Echo(oldCtx, req) }()
	c.nextSent(t)
	h.waitTimer(1)

	// The new life's first attempt, 1 ms later, same OpID and Seq.
	h.clk.Advance(time.Millisecond)
	newCh := h.start(req)
	old := h.wait(oldCh)
	if old.Outcome != OutcomeError || !errors.Is(old.Err, ErrSuperseded) {
		t.Fatalf("old attempt = %+v, want OutcomeError with ErrSuperseded", old)
	}
	cancelOld()

	p := c.nextSent(t)
	hdr, _ := parsePayloadHeader(p.msg[icmpHeaderLen:])
	if hdr.SentNs != t0.Add(time.Millisecond).UnixNano() {
		t.Fatalf("new request carries send time %d, want the new attempt's", hdr.SentNs)
	}
	h.clk.Advance(2 * time.Millisecond)
	c.deliver(echoReply(p, nil), target4, time.Time{})
	r := h.wait(newCh)
	if r.Outcome != OutcomeReply || r.RTT != 2*time.Millisecond {
		t.Fatalf("new attempt = %+v, want a 2ms reply", r)
	}
	h.expectNoLate()
}
