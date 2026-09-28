// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func baseReq(target netip.Addr) Request {
	return Request{
		OpID:     101,
		Seq:      1,
		Target:   target,
		DataSize: MinDataSize,
		Pattern:  DefaultPattern,
		Timeout:  5 * time.Second,
	}
}

// waitCount polls a counter until it reaches want.
func waitCount(t *testing.T, c *atomic.Uint64, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("counter = %d, want %d", c.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEchoReplyIPv4(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	ch := h.start(req)
	p := c.nextSent(t)

	if p.dst != target4 {
		t.Errorf("dst = %s, want %s", p.dst, target4)
	}
	// Cisco: 8 + request-data-size bytes of data, a 64-byte IPv4 packet by default.
	if len(p.msg) != icmpHeaderLen+8+MinDataSize {
		t.Errorf("icmp message length = %d, want %d", len(p.msg), icmpHeaderLen+8+MinDataSize)
	}
	if got := len(echoReply(p, nil)); got != 64 {
		t.Errorf("ipv4 packet length = %d, want 64", got)
	}
	if icmpChecksum(p.msg) != 0 {
		t.Error("icmpv4 checksum does not verify")
	}
	if p.msg[0] != icmp4EchoRequest || binary.BigEndian.Uint16(p.msg[4:]) != uint16(req.OpID) ||
		binary.BigEndian.Uint16(p.msg[6:]) != uint16(req.Seq) {
		t.Errorf("icmp header = % x", p.msg[:8])
	}
	hdr, ok := parsePayloadHeader(p.msg[icmpHeaderLen:])
	if !ok || hdr.OpID != req.OpID || hdr.Seq != req.Seq || hdr.SentNs != t0.UnixNano() {
		t.Errorf("payload header = %+v, %v", hdr, ok)
	}

	// Kernel timestamp 8 ms after sending, read by the loop at 10 ms: the
	// 2 ms user-space lag is subtracted.
	h.clk.Advance(10 * time.Millisecond)
	c.deliver(echoReply(p, nil), target4, t0.Add(8*time.Millisecond))
	r := h.wait(ch)
	if r.Outcome != OutcomeReply || r.Err != nil {
		t.Fatalf("reply = %+v", r)
	}
	if r.RTT != 8*time.Millisecond {
		t.Errorf("RTT = %v, want 8ms", r.RTT)
	}
	if !r.SentAt.Equal(t0) || !r.ReceivedAt.Equal(t0.Add(8*time.Millisecond)) {
		t.Errorf("SentAt = %v, ReceivedAt = %v", r.SentAt, r.ReceivedAt)
	}
}

func TestEchoReplyIPv6(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(true, "")
	req := baseReq(target6)
	req.DataSize = 100
	req.TOS = 0xb8
	ch := h.start(req)
	p := c.nextSent(t)
	if p.msg[0] != icmp6EchoRequest || binary.BigEndian.Uint16(p.msg[2:]) != 0 {
		t.Errorf("icmpv6 header = % x (checksum must be left to the kernel)", p.msg[:8])
	}
	if len(p.msg) != icmpHeaderLen+8+100 {
		t.Errorf("icmpv6 message length = %d, want %d", len(p.msg), icmpHeaderLen+8+100)
	}
	if p.ctl.TOS != 0xb8 {
		t.Errorf("traffic class = %#x", p.ctl.TOS)
	}
	h.clk.Advance(3 * time.Millisecond)
	c.deliver(echoReply(p, nil), target6, time.Time{}) // no kernel timestamp
	r := h.wait(ch)
	if r.Outcome != OutcomeReply || r.RTT != 3*time.Millisecond {
		t.Fatalf("reply = %+v", r)
	}
}

func TestTimeoutThenLateReply(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	req.Timeout = 2 * time.Second
	ch := h.start(req)
	p := c.nextSent(t)
	h.waitTimer(1)
	h.clk.Advance(2 * time.Second)
	r := h.wait(ch)
	if r.Outcome != OutcomeTimeout || !r.SentAt.Equal(t0) {
		t.Fatalf("reply = %+v", r)
	}

	// Within min(2*timeout, 60s): counted as late.
	c.deliver(echoReply(p, nil), target4, time.Time{})
	h.expectLate(req.OpID, req.Seq)

	// Still within the grace period at 2s+4s-1ns.
	h.e.sweep(t0.Add(6*time.Second - time.Nanosecond))
	c.deliver(echoReply(p, nil), target4, time.Time{})
	h.expectLate(req.OpID, req.Seq)

	// After the grace period the entry is purged: the reply is foreign.
	h.e.sweep(t0.Add(6 * time.Second))
	c.deliver(echoReply(p, nil), target4, time.Time{})
	waitCount(t, &h.e.stats.foreign, 1)
	h.expectNoLate()
}

func TestSweepTimerPurges(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	req.Timeout = 100 * time.Millisecond
	ch := h.start(req)
	c.nextSent(t)
	h.waitTimer(1)
	h.clk.Advance(100 * time.Millisecond)
	h.wait(ch)
	s, _ := h.e.socketFor(sockKey{})
	// The periodic sweeper runs every second; the grace period is 200 ms.
	h.clk.Advance(time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.pending)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired entry was not purged by the sweeper")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLateGraceCapped(t *testing.T) {
	h := newHarness(t, false)
	req := baseReq(target4)
	req.Timeout = 50 * time.Second
	ch := h.start(req)
	p := h.conn(false, "").nextSent(t)
	h.waitTimer(1)
	h.clk.Advance(50 * time.Second)
	h.wait(ch)
	h.e.sweep(t0.Add(50*time.Second + maxLateGrace - time.Nanosecond))
	h.conn(false, "").deliver(echoReply(p, nil), target4, time.Time{})
	h.expectLate(req.OpID, req.Seq)
	h.e.sweep(t0.Add(50*time.Second + maxLateGrace))
	s, _ := h.e.socketFor(sockKey{})
	if len(s.pending) != 0 {
		t.Errorf("pending = %d after 60s grace", len(s.pending))
	}
}

func TestDuplicateReply(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	ch := h.start(req)
	p := c.nextSent(t)
	c.deliver(echoReply(p, nil), target4, time.Time{})
	if r := h.wait(ch); r.Outcome != OutcomeReply {
		t.Fatalf("reply = %+v", r)
	}
	h.clk.Advance(time.Second) // OnLateReply reports the send time, not the arrival
	c.deliver(echoReply(p, nil), target4, time.Time{})
	if sentAt := h.expectLate(req.OpID, req.Seq); !sentAt.Equal(t0) {
		t.Errorf("OnLateReply sentAt = %v, want the request's send time %v", sentAt, t0)
	}
	if got := h.e.stats.late.Load(); got != 1 {
		t.Errorf("late counter = %d", got)
	}
}

func TestDestinationUnreachableIPv4(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typ      uint8
		code     uint8
		quoteLen int
		want     string
	}{
		{"host full quote", icmp4DestUnreach, 1, -1, "destination unreachable: host unreachable, from 10.100.1.254"},
		{"net rfc792 quote", icmp4DestUnreach, 0, 8, "destination unreachable: net unreachable, from 10.100.1.254"},
		{"admin prohibited", icmp4DestUnreach, 13, -1, "destination unreachable: communication administratively prohibited, from 10.100.1.254"},
		{"ttl", icmp4TimeExceeded, 0, -1, "time exceeded: ttl exceeded in transit, from 10.100.1.254"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			c := h.conn(false, "")
			ch := h.start(baseReq(target4))
			p := c.nextSent(t)
			h.clk.Advance(time.Millisecond)
			c.deliver(icmpError(p, tc.typ, tc.code, router4, tc.quoteLen), router4, time.Time{})
			r := h.wait(ch)
			if r.Outcome != OutcomeUnreachable || r.Detail != tc.want {
				t.Fatalf("reply = %+v, want detail %q", r, tc.want)
			}
			if !r.ReceivedAt.Equal(t0.Add(time.Millisecond)) {
				t.Errorf("ReceivedAt = %v", r.ReceivedAt)
			}
		})
	}
}

func TestDestinationUnreachableIPv6(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(true, "")
	ch := h.start(baseReq(target6))
	p := c.nextSent(t)
	c.deliver(icmpError(p, icmp6DestUnreach, 3, router6, -1), router6, time.Time{})
	r := h.wait(ch)
	want := "destination unreachable: address unreachable, from fd00:100:1::254"
	if r.Outcome != OutcomeUnreachable || r.Detail != want {
		t.Fatalf("reply = %+v", r)
	}
}

func TestUnreachableForOtherDestinationIgnored(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	req.Timeout = time.Second
	ch := h.start(req)
	p := c.nextSent(t)
	other := p
	other.dst = netip.MustParseAddr("10.100.1.99")
	c.deliver(icmpError(other, icmp4DestUnreach, 1, router4, -1), router4, time.Time{})
	waitCount(t, &h.e.stats.foreign, 1)
	h.waitTimer(1)
	h.clk.Advance(time.Second)
	if r := h.wait(ch); r.Outcome != OutcomeTimeout {
		t.Fatalf("reply = %+v", r)
	}
}

func TestForeignRepliesIgnored(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	req.Timeout = time.Second
	ch := h.start(req)
	p := c.nextSent(t)

	// Another process's ping with the same identifier and sequence.
	c.deliver(echoReply(p, func(icmp []byte) { binary.BigEndian.PutUint32(icmp[8:], 0xdeadbeef) }), target4, time.Time{})
	// Our magic, but another operation's ID.
	c.deliver(echoReply(p, func(icmp []byte) { binary.BigEndian.PutUint32(icmp[12:], 999) }), target4, time.Time{})
	// Right header, wrong full sequence (16-bit wrap).
	c.deliver(echoReply(p, func(icmp []byte) { binary.BigEndian.PutUint32(icmp[16:], req.Seq+65536) }), target4, time.Time{})
	// Unknown identifier.
	c.deliver(echoReply(p, func(icmp []byte) { icmp[4]++ }), target4, time.Time{})
	waitCount(t, &h.e.stats.foreign, 4)

	// Bad checksum and garbage are malformed.
	bad := echoReply(p, nil)
	bad[len(bad)-1] ^= 0xff
	c.deliver(bad, target4, time.Time{})
	c.deliver([]byte{0x45, 0}, target4, time.Time{})
	waitCount(t, &h.e.stats.malformed, 2)

	h.waitTimer(1)
	h.clk.Advance(time.Second)
	if r := h.wait(ch); r.Outcome != OutcomeTimeout {
		t.Fatalf("reply = %+v", r)
	}
	h.expectNoLate()
}

func TestVerify(t *testing.T) {
	corrupt := func(icmp []byte) { icmp[len(icmp)-1] ^= 0x01 }
	for _, tc := range []struct {
		name   string
		verify bool
		mutate func([]byte)
		cut    int
		want   Outcome
	}{
		{"verify ok", true, nil, 0, OutcomeReply},
		{"verify corrupt", true, corrupt, 0, OutcomeVerifyError},
		{"verify short", true, nil, 4, OutcomeVerifyError},
		{"no verify corrupt", false, corrupt, 0, OutcomeReply},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			c := h.conn(false, "")
			req := baseReq(target4)
			req.DataSize = 64
			req.Verify = tc.verify
			ch := h.start(req)
			p := c.nextSent(t)
			if tc.cut > 0 {
				p.msg = p.msg[:len(p.msg)-tc.cut]
			}
			c.deliver(echoReply(p, tc.mutate), target4, time.Time{})
			r := h.wait(ch)
			if r.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v (%+v)", r.Outcome, tc.want, r)
			}
			if r.Outcome == OutcomeVerifyError && r.Detail == "" {
				t.Error("verify error without detail")
			}
		})
	}
}

func TestContextCanceled(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	ctx, cancel := context.WithCancel(t.Context())
	req := baseReq(target4)
	ch := make(chan Reply, 1)
	go func() { ch <- h.e.Echo(ctx, req) }()
	p := c.nextSent(t)
	cancel()
	r := h.wait(ch)
	if r.Outcome != OutcomeError || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("reply = %+v", r)
	}
	// A reply after abandonment is late, not a completion.
	c.deliver(echoReply(p, nil), target4, time.Time{})
	h.expectLate(req.OpID, req.Seq)

	done, cancel2 := context.WithCancel(t.Context())
	cancel2()
	if r := h.e.Echo(done, req); r.Outcome != OutcomeError || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("echo with done ctx = %+v", r)
	}
}

func TestClose(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	ch := h.start(baseReq(target4))
	c.nextSent(t)
	if err := h.e.Close(); err != nil {
		t.Fatal(err)
	}
	r := h.wait(ch)
	if r.Outcome != OutcomeError || !errors.Is(r.Err, ErrClosed) {
		t.Fatalf("waiting echo = %+v", r)
	}
	if r := h.e.Echo(t.Context(), baseReq(target4)); r.Outcome != OutcomeError || !errors.Is(r.Err, ErrClosed) {
		t.Fatalf("echo after close = %+v", r)
	}
	if err := h.e.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	select {
	case <-c.closed:
	default:
		t.Error("socket not closed")
	}
}

func TestIPv6Unavailable(t *testing.T) {
	h := newHarness(t, true)
	r := h.e.Echo(t.Context(), baseReq(target6))
	if r.Outcome != OutcomeError || r.Err == nil || !strings.Contains(r.Err.Error(), "ipv6 unavailable") {
		t.Fatalf("reply = %+v", r)
	}
}

func TestNewFailsWithoutIPv4(t *testing.T) {
	_, err := newEngine(Options{}, func(bool, string) (packetConn, error) {
		return nil, errors.New("operation not permitted")
	}, interfaceIndex)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidation(t *testing.T) {
	h := newHarness(t, false)
	for _, tc := range []struct {
		name   string
		mutate func(*Request)
		want   string
	}{
		{"no target", func(r *Request) { r.Target = netip.Addr{} }, "invalid target"},
		{"small", func(r *Request) { r.DataSize = 27 }, "below the minimum 28"},
		{"large", func(r *Request) { r.DataSize = 65500 }, "exceeds the maximum 65499"},
		{"large v6", func(r *Request) { r.Target = target6; r.DataSize = 65520 }, "exceeds the maximum 65519"},
		{"timeout", func(r *Request) { r.Timeout = 0 }, "timeout must be positive"},
		{"family", func(r *Request) { r.Source = netip.MustParseAddr("fd00::1") }, "address family"},
		{"interface", func(r *Request) { r.Interface = "nope0" }, "interface nope0"},
		{"flow label", func(r *Request) { r.Target = target6; r.FlowLabel = 0x100000 }, "exceeds 20 bits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseReq(target4)
			tc.mutate(&req)
			r := h.e.Echo(t.Context(), req)
			if r.Outcome != OutcomeError || r.Err == nil || !strings.Contains(r.Err.Error(), tc.want) {
				t.Fatalf("reply = %+v, want error containing %q", r, tc.want)
			}
		})
	}
}

func TestMaxDataSizeAccepted(t *testing.T) {
	h := newHarness(t, false)
	for _, tc := range []struct {
		target netip.Addr
		size   int
	}{{target4, 65499}, {target6, 65519}} {
		c := h.conn(tc.target.Is6(), "")
		req := baseReq(tc.target)
		req.DataSize = tc.size
		ch := h.start(req)
		p := c.nextSent(t)
		if len(p.msg) != icmpHeaderLen+8+tc.size {
			t.Errorf("%s: icmp message length = %d", tc.target, len(p.msg))
		}
		c.deliver(echoReply(p, nil), tc.target, time.Time{})
		if r := h.wait(ch); r.Outcome != OutcomeReply {
			t.Fatalf("%s size %d: %+v", tc.target, tc.size, r)
		}
	}
}

func TestSendControl(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(netip.MustParseAddr("::ffff:10.100.1.11")) // v4-mapped is IPv4
	req.Source = local4
	req.Interface = "eth0"
	req.TOS = 0xb8
	req.FlowLabel = 5 // ignored for IPv4
	ch := h.start(req)
	p := c.nextSent(t)
	want := connControl{Src: local4, IfIndex: 7, TOS: 0xb8, Deadline: t0.Add(req.Timeout)}
	if p.ctl != want || p.dst != target4 {
		t.Errorf("ctl = %+v dst = %s, want %+v %s", p.ctl, p.dst, want, target4)
	}
	c.deliver(echoReply(p, nil), target4, time.Time{})
	h.wait(ch)
}

func TestLinkLocalZone(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(true, "")
	ll := netip.MustParseAddr("fe80::1%eth0")
	ch := h.start(baseReq(ll))
	p := c.nextSent(t)
	if p.ctl.ScopeID != 7 {
		t.Errorf("scope id = %d", p.ctl.ScopeID)
	}
	// Replies carry the numeric zone; matching ignores it.
	c.deliver(echoReply(p, nil), netip.MustParseAddr("fe80::1%7"), time.Time{})
	if r := h.wait(ch); r.Outcome != OutcomeReply {
		t.Fatalf("reply = %+v", r)
	}
}

func TestFlowLabelFallback(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(true, "")
	c.writeErr = &connFlowLabelError{label: 5, err: errors.New("operation not permitted")}
	req := baseReq(target6)
	req.FlowLabel = 5
	ch := h.start(req)
	p := c.nextSent(t)
	if p.ctl.FlowLabel != 5 {
		t.Errorf("flow label = %d", p.ctl.FlowLabel)
	}
	c.deliver(echoReply(p, nil), target6, time.Time{})
	if r := h.wait(ch); r.Outcome != OutcomeReply {
		t.Fatalf("reply = %+v", r)
	}
}

func TestSendError(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	c.writeErr = errors.New("network is unreachable")
	r := h.e.Echo(t.Context(), baseReq(target4))
	if r.Outcome != OutcomeError || r.Err == nil || !strings.Contains(r.Err.Error(), "network is unreachable") {
		t.Fatalf("reply = %+v", r)
	}
	s, _ := h.e.socketFor(sockKey{})
	if len(s.pending) != 0 {
		t.Error("failed request left in the pending table")
	}
}

func TestVRFSocketLazy(t *testing.T) {
	h := newHarness(t, false)
	if h.conn(false, "blue") != nil {
		t.Fatal("vrf socket opened before use")
	}
	req := baseReq(target4)
	req.VRF = "blue"
	ch := h.start(req)
	c := func() *fakeConn {
		for range 500 {
			if c := h.conn(false, "blue"); c != nil {
				return c
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("vrf socket not opened")
		return nil
	}()
	p := c.nextSent(t)
	c.deliver(echoReply(p, nil), target4, time.Time{})
	if r := h.wait(ch); r.Outcome != OutcomeReply {
		t.Fatalf("reply = %+v", r)
	}
	select {
	case <-h.conn(false, "").sent:
		t.Error("vrf request went out on the default socket")
	default:
	}
}

func TestIdentifierAllocation(t *testing.T) {
	s := &socket{idents: map[uint32]uint16{}, owners: map[uint16]uint32{}}
	for _, tc := range []struct {
		op   uint32
		want uint16
	}{
		{1, 1}, {65537, 2}, {2, 3}, {1, 1}, {0xffff, 0xffff}, {0x1ffff, 0},
	} {
		got, err := s.identFor(tc.op)
		if err != nil || got != tc.want {
			t.Errorf("identFor(%d) = %d, %v; want %d", tc.op, got, err, tc.want)
		}
	}
}

func TestSequenceWrapReplacesExpired(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	req.Timeout = time.Second
	ch := h.start(req)
	c.nextSent(t)
	h.waitTimer(1)
	h.clk.Advance(time.Second)
	h.wait(ch)
	// Same 16-bit sequence while the first is still in its grace period.
	req.Seq += 65536
	ch = h.start(req)
	p := c.nextSent(t)
	c.deliver(echoReply(p, nil), target4, time.Time{})
	if r := h.wait(ch); r.Outcome != OutcomeReply {
		t.Fatalf("reply = %+v", r)
	}
}

// TestInFlightCollision: a second Echo of the same operation and Seq while
// the first is waiting supersedes it (see TestNewLifeSupersedesWaitingRequest),
// and a reply to the superseded request does not complete the new one.
func TestInFlightCollision(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	ch := h.start(req)
	first := c.nextSent(t)
	h.waitTimer(1)
	h.clk.Advance(time.Millisecond)
	ch2 := h.start(req)
	if r := h.wait(ch); r.Outcome != OutcomeError || !errors.Is(r.Err, ErrSuperseded) {
		t.Fatalf("first = %+v, want ErrSuperseded", r)
	}
	second := c.nextSent(t)
	c.deliver(echoReply(first, nil), target4, time.Time{})
	waitCount(t, &h.e.stats.foreign, 1)
	c.deliver(echoReply(second, nil), target4, time.Time{})
	if r := h.wait(ch2); r.Outcome != OutcomeReply {
		t.Fatalf("second = %+v", r)
	}
}

func TestRTTCorrection(t *testing.T) {
	e := &engine{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sent := time.Unix(1000, 0)
	for _, tc := range []struct {
		name string
		now  time.Time
		rx   time.Time
		want time.Duration
	}{
		{"no timestamp", sent.Add(5 * time.Millisecond), time.Time{}, 5 * time.Millisecond},
		{"lag", sent.Add(5 * time.Millisecond), sent.Add(4 * time.Millisecond), 4 * time.Millisecond},
		{"lag at the limit", sent.Add(1500 * time.Millisecond), sent.Add(500 * time.Millisecond), 500 * time.Millisecond},
		// The wall clock stepped back after the kernel timestamp: the
		// timestamp is later than the read, the correction is dropped.
		{"step back", sent.Add(5 * time.Millisecond), sent.Add(6 * time.Millisecond), 5 * time.Millisecond},
		{"step back far", sent.Add(5 * time.Millisecond), sent.Add(time.Hour), 5 * time.Millisecond},
		// The wall clock stepped forward: the apparent lag exceeds 1s and
		// would pin the RTT to 0; the correction is dropped.
		{"step forward", sent.Add(5 * time.Millisecond), sent.Add(-10 * time.Second), 5 * time.Millisecond},
		{"lag just over the limit", sent.Add(1500 * time.Millisecond), sent.Add(500*time.Millisecond - time.Nanosecond), 1500 * time.Millisecond},
		{"negative clamps", sent.Add(-time.Millisecond), time.Time{}, 0},
	} {
		if got := e.rtt(sent, tc.now, tc.rx, 1, 1); got != tc.want {
			t.Errorf("%s: rtt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRTTWallClockStepThroughEngine delivers a reply whose kernel timestamp
// is far from the read time, as after a wall-clock step.
func TestRTTWallClockStepThroughEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		rx   time.Time
	}{
		{"forward", t0.Add(-time.Hour)},
		{"back", t0.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			c := h.conn(false, "")
			ch := h.start(baseReq(target4))
			p := c.nextSent(t)
			h.clk.Advance(7 * time.Millisecond)
			c.deliver(echoReply(p, nil), target4, tc.rx)
			if r := h.wait(ch); r.Outcome != OutcomeReply || r.RTT != 7*time.Millisecond {
				t.Fatalf("reply = %+v, want the uncorrected 7ms", r)
			}
		})
	}
}

// TestTimeoutCountsFromSendTime: the timer starts before sendmsg, so a send
// that blocks past the timeout ends in OutcomeTimeout.
func TestTimeoutCountsFromSendTime(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	c.hold = make(chan struct{})
	req := baseReq(target4)
	req.Timeout = time.Second
	ch := h.start(req)
	h.waitTimer(1) // armed while WriteTo is still blocked
	h.clk.Advance(time.Second)
	close(c.hold)
	p := c.nextSent(t)
	r := h.wait(ch)
	if r.Outcome != OutcomeTimeout || !r.SentAt.Equal(t0) {
		t.Fatalf("reply = %+v", r)
	}
	// A reply after that is late.
	c.deliver(echoReply(p, nil), target4, time.Time{})
	h.expectLate(req.OpID, req.Seq)
}

func TestSendDeadlineExceeded(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	c.writeErr = errConnSendTimeout
	r := h.e.Echo(t.Context(), baseReq(target4))
	if r.Outcome != OutcomeTimeout || r.Detail != "send timed out" {
		t.Fatalf("reply = %+v", r)
	}
	s, _ := h.e.socketFor(sockKey{})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) != 0 {
		t.Error("unsent request left in the pending table")
	}
}

// TestConcurrentEchoes runs 1,000 operations at once against an automatic
// responder; run with -race.
func TestConcurrentEchoes(t *testing.T) {
	h := newHarness(t, false)
	c4, c6 := h.conn(false, ""), h.conn(true, "")
	stop := make(chan struct{})
	defer close(stop)
	for _, c := range []*fakeConn{c4, c6} {
		go func() {
			for {
				select {
				case p := <-c.sent:
					c.deliver(echoReply(p, nil), p.dst, time.Time{})
				case <-stop:
					return
				}
			}
		}()
	}
	const n = 1000
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := baseReq(target4)
			if i%2 == 1 {
				req.Target = target6
			}
			req.OpID = uint32(i + 1)
			req.Seq = uint32(i * 7)
			req.Verify = true
			if r := h.e.Echo(t.Context(), req); r.Outcome != OutcomeReply {
				errs <- fmt.Errorf("op %d: %+v", req.OpID, r)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
