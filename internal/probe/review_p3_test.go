// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"net/netip"
	"testing"
	"time"
)

// B4: a late reply to an earlier attempt with the same OpID and Seq (an
// operation that restarted its life) does not complete the new attempt:
// the payload's send time differs.
func TestEchoReplyFromEarlierLifeIgnored(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	req.Timeout = time.Second
	ch := h.start(req)
	old := c.nextSent(t)
	h.waitTimer(1)
	h.clk.Advance(time.Second)
	if r := h.wait(ch); r.Outcome != OutcomeTimeout {
		t.Fatalf("first attempt = %+v", r)
	}

	// New life: Seq starts at 1 again while the old entry is in its grace
	// period.
	ch = h.start(req)
	cur := c.nextSent(t)
	c.deliver(echoReply(old, nil), target4, time.Time{})
	c.deliver(icmpError(old, icmp4DestUnreach, 1, router4, -1), router4, time.Time{})
	waitCount(t, &h.e.stats.foreign, 2)
	h.expectNoLate()

	h.clk.Advance(3 * time.Millisecond)
	c.deliver(echoReply(cur, nil), target4, time.Time{})
	if r := h.wait(ch); r.Outcome != OutcomeReply || r.RTT != 3*time.Millisecond || !r.SentAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("second attempt = %+v", r)
	}
}

// S1 (echo side): a reply that arrives at or after the timeout is late even
// when the receive loop sees it before the waiting Echo's timer fires.
func TestEchoReplyAtDeadlineIsLate(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration // receive time after sending
		late bool
	}{
		{"just before", time.Second - time.Nanosecond, false},
		{"at the deadline", time.Second, true},
		{"after", time.Second + time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			c := h.conn(false, "")
			req := baseReq(target4)
			req.Timeout = time.Second
			ch := h.start(req)
			p := c.nextSent(t)
			h.waitTimer(1)
			// Run the receive path directly at the given time, while the
			// fake clock (and the Echo's timer) is still at the send time.
			s, _ := h.e.socketFor(sockKey{})
			h.e.handle(s, echoReply(p, nil), target4, time.Time{}, t0.Add(tc.at))
			if !tc.late {
				if r := h.wait(ch); r.Outcome != OutcomeReply || r.RTT != tc.at {
					t.Fatalf("reply = %+v", r)
				}
				h.expectNoLate()
				return
			}
			h.expectLate(req.OpID, req.Seq)
			h.clk.Advance(time.Second)
			if r := h.wait(ch); r.Outcome != OutcomeTimeout {
				t.Fatalf("reply = %+v", r)
			}
		})
	}
}

// S2 (echo side): a matching reply from another address is not ours.
func TestEchoReplyFromOtherAddressIgnored(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	req.Timeout = time.Second
	ch := h.start(req)
	p := c.nextSent(t)
	c.deliver(echoReply(p, nil), netip.MustParseAddr("10.100.1.12"), time.Time{})
	waitCount(t, &h.e.stats.foreign, 1)
	c.deliver(echoReply(p, nil), netip.MustParseAddr("::ffff:10.100.1.11"), time.Time{}) // v4-mapped form of the target
	if r := h.wait(ch); r.Outcome != OutcomeReply {
		t.Fatalf("reply = %+v", r)
	}
}

// S1: a Timestamp Reply at or after its packet's timeout is late, even if
// the receive loop handles it before the sender loop expires the packet.
func TestTimestampReplyAtDeadlineIsLate(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration
		late bool
	}{
		{"just before", 100*time.Millisecond - time.Nanosecond, false},
		{"at the deadline", 100 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			c := h.conn(false, "")
			req := baseJitterReq()
			req.NumPackets = 1
			req.Timeout = 100 * time.Millisecond
			ch := h.startJitter(t.Context(), req)
			p := c.nextSent(t)
			h.waitParked()
			s, _ := h.e.socketFor(sockKey{})
			h.e.handle(s, tsReply(p, 7, 8), target4, time.Time{}, t0.Add(tc.at))
			if !tc.late {
				pk := h.waitJitter(ch).Packets[0]
				if pk.Outcome != OutcomeReply || pk.Late || pk.RTT != tc.at {
					t.Fatalf("packet = %+v", pk)
				}
				return
			}
			// The late event completes the packet as a timeout by itself.
			pk := h.waitJitter(ch).Packets[0]
			if pk.Outcome != OutcomeTimeout || !pk.Late {
				t.Fatalf("packet = %+v", pk)
			}
			h.expectNoLate()
		})
	}
}

// S2: a Timestamp Reply from another address is not ours.
func TestTimestampReplyFromOtherAddressIgnored(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseJitterReq()
	req.NumPackets = 1
	ch := h.startJitter(t.Context(), req)
	p := c.nextSent(t)
	c.deliver(tsReply(p, 1, 1), netip.MustParseAddr("10.100.2.12"), time.Time{})
	waitCount(t, &h.e.stats.foreign, 1)
	c.deliver(tsReply(p, 1, 1), target4, time.Time{})
	if pk := h.waitJitter(ch).Packets[0]; pk.Outcome != OutcomeReply {
		t.Fatalf("packet = %+v", pk)
	}
}
