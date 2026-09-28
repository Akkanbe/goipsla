// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func baseJitterReq() JitterRequest {
	return JitterRequest{
		OpID:       201,
		Seq:        5,
		Target:     target4,
		NumPackets: 3,
		Interval:   20 * time.Millisecond,
		Timeout:    time.Second,
	}
}

// tsReply turns a sent Timestamp Request into the target's reply with the
// given Receive and Transmit timestamps.
func tsReply(p sentPkt, recv, xmit uint32) []byte {
	icmp := append([]byte(nil), p.msg...)
	icmp[0] = icmp4TimestampReply
	binary.BigEndian.PutUint32(icmp[12:], recv)
	binary.BigEndian.PutUint32(icmp[16:], xmit)
	icmp[2], icmp[3] = 0, 0
	binary.BigEndian.PutUint16(icmp[2:], icmpChecksum(icmp))
	return ipv4Packet(p.dst, local4, icmp)
}

func (h *harness) startJitter(ctx context.Context, req JitterRequest) <-chan JitterReply {
	ch := make(chan JitterReply, 1)
	go func() { ch <- h.e.Jitter(ctx, req) }()
	return ch
}

func (h *harness) waitJitter(ch <-chan JitterReply) JitterReply {
	h.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		h.t.Fatal("Jitter did not return")
		return JitterReply{}
	}
}

// waitParked waits until the Jitter loop has armed its timer and waits in
// its select, so that advancing the fake clock is seen by that timer.
func (h *harness) waitParked() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !h.e.burstParked.Load() {
		if time.Now().After(deadline) {
			h.t.Fatal("jitter loop did not park")
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// waitPendingState waits until the Timestamp entry sent as p reaches state.
func (h *harness) waitPendingState(p sentPkt, state pendState) {
	h.t.Helper()
	s, _ := h.e.socketFor(sockKey{})
	key := pendKey{kind: kindTimestamp, id: binary.BigEndian.Uint16(p.msg[4:]), seq: binary.BigEndian.Uint16(p.msg[6:])}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		e := s.pending[key]
		ok := e != nil && e.state == state
		s.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("entry %+v did not reach state %d", key, state)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestJitterBurst(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseJitterReq()
	ch := h.startJitter(t.Context(), req)

	var sent []sentPkt
	for i := range 3 {
		if i > 0 {
			h.waitParked()
			h.clk.Advance(20 * time.Millisecond)
		}
		p := c.nextSent(t)
		sent = append(sent, p)
		if len(p.msg) != icmpTimestampLen || p.msg[0] != icmp4TimestampRequest || icmpChecksum(p.msg) != 0 {
			t.Fatalf("packet %d = % x", i, p.msg)
		}
		if got, want := binary.BigEndian.Uint32(p.msg[8:]), icmpTimestampMs(t0.Add(time.Duration(i)*20*time.Millisecond)); got != want {
			t.Errorf("packet %d originate = %d, want %d", i, got, want)
		}
		if binary.BigEndian.Uint16(p.msg[6:]) != uint16(i) {
			t.Errorf("packet %d sequence = %d", i, binary.BigEndian.Uint16(p.msg[6:]))
		}
		if p.ctl.Deadline != t0.Add(time.Duration(i)*20*time.Millisecond+time.Second) {
			t.Errorf("packet %d deadline = %v", i, p.ctl.Deadline)
		}
	}
	// Replies arrive out of order: 2, 0, 1.
	for _, i := range []int{2, 0, 1} {
		c.deliver(tsReply(sent[i], uint32(1000+i), uint32(2000+i)), target4, time.Time{})
	}
	r := h.waitJitter(ch)
	if r.Err != nil || len(r.Packets) != 3 {
		t.Fatalf("reply = %+v", r)
	}
	wantPos := []int{1, 2, 0}
	for i, pk := range r.Packets {
		if pk.Index != i || pk.Outcome != OutcomeReply || pk.Receive != uint32(1000+i) || pk.Transmit != uint32(2000+i) {
			t.Errorf("packet %d = %+v", i, pk)
		}
		if pk.ArrivalPos != wantPos[i] {
			t.Errorf("packet %d arrival = %d, want %d", i, pk.ArrivalPos, wantPos[i])
		}
		wantSent := t0.Add(time.Duration(i) * 20 * time.Millisecond)
		if !pk.SentAt.Equal(wantSent) || pk.Originate != icmpTimestampMs(wantSent) {
			t.Errorf("packet %d sent = %v originate %d", i, pk.SentAt, pk.Originate)
		}
		if pk.RTT != 40*time.Millisecond-time.Duration(i)*20*time.Millisecond || !pk.ReceivedAt.Equal(t0.Add(40*time.Millisecond)) {
			t.Errorf("packet %d rtt = %v received %v", i, pk.RTT, pk.ReceivedAt)
		}
	}
}

func TestJitterTimeoutLateAndAfterBurst(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseJitterReq()
	req.NumPackets = 2
	req.Interval = 10 * time.Millisecond
	req.Timeout = 100 * time.Millisecond
	ch := h.startJitter(t.Context(), req)
	p0 := c.nextSent(t)
	h.waitParked()
	h.clk.Advance(10 * time.Millisecond)
	p1 := c.nextSent(t)
	h.waitParked()
	h.clk.Advance(90 * time.Millisecond) // p0 times out at 100 ms; p1 waits until 110 ms
	h.waitPendingState(p0, stateExpired)

	c.deliver(tsReply(p0, 1, 1), target4, time.Time{}) // late, within the burst
	c.deliver(tsReply(p0, 1, 1), target4, time.Time{}) // duplicate late: ignored
	c.deliver(tsReply(p1, 2, 2), target4, time.Time{})
	r := h.waitJitter(ch)
	if pk := r.Packets[0]; pk.Outcome != OutcomeTimeout || !pk.Late {
		t.Errorf("packet 0 = %+v", pk)
	}
	if pk := r.Packets[1]; pk.Outcome != OutcomeReply || pk.Late || pk.ArrivalPos != 0 {
		t.Errorf("packet 1 = %+v", pk)
	}
	h.expectNoLate()

	// After the burst: OnLateReply with the burst number.
	c.deliver(tsReply(p0, 1, 1), target4, time.Time{})
	if sentAt := h.expectLate(req.OpID, req.Seq); !sentAt.Equal(r.Packets[0].SentAt) {
		t.Errorf("sentAt = %v, want packet 0's %v", sentAt, r.Packets[0].SentAt)
	}
	c.deliver(tsReply(p1, 2, 2), target4, time.Time{})
	if sentAt := h.expectLate(req.OpID, req.Seq); !sentAt.Equal(r.Packets[1].SentAt) {
		t.Errorf("sentAt = %v, want packet 1's %v", sentAt, r.Packets[1].SentAt)
	}
}

func TestJitterAllTimeout(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseJitterReq()
	req.NumPackets = 2
	req.Interval = 0 // back to back
	req.Timeout = 50 * time.Millisecond
	ch := h.startJitter(t.Context(), req)
	c.nextSent(t)
	c.nextSent(t)
	h.waitParked()
	h.clk.Advance(50 * time.Millisecond)
	r := h.waitJitter(ch)
	if r.Err != nil {
		t.Fatalf("err = %v (packets were sent)", r.Err)
	}
	for _, pk := range r.Packets {
		if pk.Outcome != OutcomeTimeout || !pk.SentAt.Equal(t0) {
			t.Errorf("packet = %+v", pk)
		}
	}
}

func TestJitterSkipped(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseJitterReq()
	req.Interval = 10 * time.Millisecond
	req.Timeout = 100 * time.Millisecond
	ch := h.startJitter(t.Context(), req)
	p0 := c.nextSent(t)
	// Jump 25 ms: slot 1 (10 ms) is 15 ms late, a full interval: skipped.
	// Slot 2 (20 ms) is 5 ms late: sent at 25 ms, back on the grid.
	h.waitParked()
	h.clk.Advance(25 * time.Millisecond)
	p2 := c.nextSent(t)
	c.deliver(tsReply(p0, 1, 1), target4, time.Time{})
	c.deliver(tsReply(p2, 3, 3), target4, time.Time{})
	r := h.waitJitter(ch)
	if pk := r.Packets[1]; pk.Outcome != OutcomeError || !errors.Is(pk.Err, ErrSkipped) || !pk.SentAt.IsZero() {
		t.Errorf("packet 1 = %+v", pk)
	}
	if pk := r.Packets[2]; pk.Outcome != OutcomeReply || !pk.SentAt.Equal(t0.Add(25*time.Millisecond)) {
		t.Errorf("packet 2 = %+v", pk)
	}
	if r.Err != nil {
		t.Errorf("err = %v", r.Err)
	}
}

func TestJitterUnreachable(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseJitterReq()
	req.NumPackets = 1
	ch := h.startJitter(t.Context(), req)
	p := c.nextSent(t)
	for _, quote := range []int{-1, 8} { // the second is a duplicate error: ignored
		c.deliver(icmpError(p, icmp4DestUnreach, 1, router4, quote), router4, time.Time{})
	}
	r := h.waitJitter(ch)
	want := "destination unreachable: host unreachable, from 10.100.1.254"
	if pk := r.Packets[0]; pk.Outcome != OutcomeUnreachable || pk.Detail != want {
		t.Fatalf("packet = %+v", pk)
	}
}

func TestJitterForeignReplies(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseJitterReq()
	req.NumPackets = 1
	req.Timeout = 100 * time.Millisecond
	ch := h.startJitter(t.Context(), req)
	p := c.nextSent(t)
	bad := append([]byte(nil), p.msg...)
	binary.BigEndian.PutUint32(bad[8:], binary.BigEndian.Uint32(bad[8:])+1) // another originate
	c.deliver(tsReply(sentPkt{msg: bad, dst: p.dst}, 1, 1), target4, time.Time{})
	// An Echo Reply with the same identifier and sequence is not a
	// Timestamp Reply.
	echo := sentPkt{msg: buildICMPEcho(false, binary.BigEndian.Uint16(p.msg[4:]), 0, buildPayload(28, req.OpID, 0, DefaultPattern, t0)), dst: target4}
	c.deliver(echoReply(echo, nil), target4, time.Time{})
	waitCount(t, &h.e.stats.foreign, 2)
	h.waitParked()
	h.clk.Advance(100 * time.Millisecond)
	if pk := h.waitJitter(ch).Packets[0]; pk.Outcome != OutcomeTimeout || pk.Late {
		t.Fatalf("packet = %+v", pk)
	}
}

func TestJitterSharesIdentifierWithEcho(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	ereq := baseReq(target4)
	ereq.OpID, ereq.Seq = 201, 0
	ech := h.start(ereq)
	ep := c.nextSent(t)
	jreq := baseJitterReq()
	jreq.NumPackets = 1
	jch := h.startJitter(t.Context(), jreq)
	jp := c.nextSent(t)
	if binary.BigEndian.Uint32(ep.msg[4:]) != binary.BigEndian.Uint32(jp.msg[4:]) {
		t.Fatalf("echo and timestamp use different id/seq: % x / % x", ep.msg[4:8], jp.msg[4:8])
	}
	c.deliver(tsReply(jp, 1, 1), target4, time.Time{})
	c.deliver(echoReply(ep, nil), target4, time.Time{})
	if r := h.wait(ech); r.Outcome != OutcomeReply {
		t.Errorf("echo = %+v", r)
	}
	if r := h.waitJitter(jch); r.Packets[0].Outcome != OutcomeReply {
		t.Errorf("jitter = %+v", r)
	}
}

func TestJitterCancel(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	ctx, cancel := context.WithCancel(t.Context())
	req := baseJitterReq()
	ch := h.startJitter(ctx, req)
	p0 := c.nextSent(t)
	c.deliver(tsReply(p0, 1, 1), target4, time.Time{})
	h.waitPendingState(p0, stateAnswered)
	cancel()
	r := h.waitJitter(ch)
	if r.Packets[0].Outcome != OutcomeReply {
		t.Errorf("packet 0 = %+v", r.Packets[0])
	}
	for _, pk := range r.Packets[1:] {
		if pk.Outcome != OutcomeError || !errors.Is(pk.Err, context.Canceled) {
			t.Errorf("packet = %+v", pk)
		}
	}
}

func TestJitterClose(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	ch := h.startJitter(t.Context(), baseJitterReq())
	c.nextSent(t)
	h.e.Close()
	r := h.waitJitter(ch)
	for _, pk := range r.Packets {
		if pk.Outcome != OutcomeError || !errors.Is(pk.Err, ErrClosed) {
			t.Errorf("packet = %+v", pk)
		}
	}
}

func TestJitterErrors(t *testing.T) {
	h := newHarness(t, false)
	for _, tc := range []struct {
		name   string
		mutate func(*JitterRequest)
		want   error
		text   string
	}{
		{"ipv6", func(r *JitterRequest) { r.Target = target6 }, ErrUnsupportedFamily, ""},
		{"no target", func(r *JitterRequest) { r.Target = netip.Addr{} }, nil, "invalid target"},
		{"packets", func(r *JitterRequest) { r.NumPackets = 0 }, nil, "at least 1"},
		{"timeout", func(r *JitterRequest) { r.Timeout = 0 }, nil, "timeout must be positive"},
		{"interval", func(r *JitterRequest) { r.Interval = -1 }, nil, "interval"},
		{"source", func(r *JitterRequest) { r.Source = target6 }, nil, "not an ipv4 address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseJitterReq()
			tc.mutate(&req)
			r := h.e.Jitter(t.Context(), req)
			if r.Err == nil || (tc.want != nil && !errors.Is(r.Err, tc.want)) || (tc.text != "" && !strings.Contains(r.Err.Error(), tc.text)) {
				t.Fatalf("err = %v", r.Err)
			}
			for _, pk := range r.Packets {
				if pk.Outcome != OutcomeError || pk.Err == nil {
					t.Errorf("packet = %+v", pk)
				}
			}
		})
	}

	c := h.conn(false, "")
	c.writeErr = errors.New("network is unreachable")
	req := baseJitterReq()
	req.Interval = 0
	r := h.e.Jitter(t.Context(), req)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "network is unreachable") {
		t.Fatalf("send failure: %+v", r)
	}
	for _, pk := range r.Packets {
		if pk.Outcome != OutcomeError {
			t.Errorf("packet = %+v", pk)
		}
	}
}

func TestMsSinceMidnightUT(t *testing.T) {
	for _, tc := range []struct {
		t    time.Time
		want uint32
	}{
		{time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), 0},
		{time.Date(2026, 9, 27, 9, 55, 25, 95e6, time.UTC), 35725095},
		{time.Date(2026, 9, 27, 23, 59, 59, 999e6, time.UTC), 86399999},
		{time.Date(2026, 9, 28, 9, 0, 0, 0, time.FixedZone("JST", 9*3600)), 0},
		{time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC), 86399000},
	} {
		if got := icmpTimestampMs(tc.t); got != tc.want {
			t.Errorf("msSinceMidnightUT(%v) = %d, want %d", tc.t, got, tc.want)
		}
	}
}
