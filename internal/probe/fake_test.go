// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"goipsla/internal/clock"
)

var (
	t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	//declscope:package // a fixture shared by the engine and packet tests
	local4 = netip.MustParseAddr("10.100.1.10")
	//declscope:package // a fixture shared by the engine and packet tests
	target4 = netip.MustParseAddr("10.100.1.11")
	//declscope:package // a fixture shared by the engine and packet tests
	router4 = netip.MustParseAddr("10.100.1.254")
	//declscope:package // a fixture shared by the engine and packet tests
	target6 = netip.MustParseAddr("fd00:100:1::11")
	//declscope:package // a fixture shared by the engine and packet tests
	router6 = netip.MustParseAddr("fd00:100:1::254")
)

//declscope:package // a fixture shared by the engine and packet tests
type sentPkt struct {
	msg []byte
	dst netip.Addr
	ctl connControl
}

type inbound struct {
	pkt  []byte
	from netip.Addr
	rx   time.Time
	err  error // returned by ReadFrom instead of a packet
}

// fakeConn is an in-memory packetConn. Sent packets appear on sent; packets
// pushed with deliver are returned by ReadFrom.
type fakeConn struct {
	v6       bool
	vrf      string
	sent     chan sentPkt
	in       chan inbound
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	writeErr error
	hold     chan struct{} // if set, WriteTo waits until it is closed
}

func newFakeConn(v6 bool, vrf string) *fakeConn {
	return &fakeConn{
		v6:     v6,
		vrf:    vrf,
		sent:   make(chan sentPkt, 4096),
		in:     make(chan inbound, 4096),
		closed: make(chan struct{}),
	}
}

func (c *fakeConn) WriteTo(msg []byte, dst netip.Addr, ctl connControl) error {
	c.mu.Lock()
	err, hold := c.writeErr, c.hold
	c.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-c.closed:
			return ErrClosed
		}
	}
	var fle *connFlowLabelError
	if err != nil && !errors.As(err, &fle) {
		return err
	}
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	c.sent <- sentPkt{msg: append([]byte(nil), msg...), dst: dst, ctl: ctl}
	return err
}

func (c *fakeConn) ReadFrom(buf []byte) (int, netip.Addr, time.Time, error) {
	select {
	case p := <-c.in:
		if p.err != nil {
			return 0, netip.Addr{}, time.Time{}, p.err
		}
		return copy(buf, p.pkt), p.from, p.rx, nil
	case <-c.closed:
		return 0, netip.Addr{}, time.Time{}, errConnClosed
	}
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeConn) deliver(pkt []byte, from netip.Addr, rx time.Time) {
	c.in <- inbound{pkt: pkt, from: from, rx: rx}
}

// failRead makes the next ReadFrom return err.
func (c *fakeConn) failRead(err error) {
	c.in <- inbound{err: err}
}

func (c *fakeConn) nextSent(t *testing.T) sentPkt {
	t.Helper()
	select {
	case p := <-c.sent:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no packet was sent")
		return sentPkt{}
	}
}

// harness wires an engine to fake sockets and a fake clock.
type harness struct {
	t     *testing.T
	e     *engine
	clk   *clock.Fake
	mu    sync.Mutex
	conns map[sockKey]*fakeConn
	late  chan lateCall
	fail6 bool
}

func newHarness(t *testing.T, fail6 bool) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		clk:   clock.NewFake(t0),
		conns: make(map[sockKey]*fakeConn),
		late:  make(chan lateCall, 64),
		fail6: fail6,
	}
	open := func(v6 bool, vrf string) (packetConn, error) {
		if v6 && h.fail6 {
			return nil, errors.New("address family not supported by protocol")
		}
		c := newFakeConn(v6, vrf)
		h.mu.Lock()
		h.conns[sockKey{v6: v6, vrf: vrf}] = c
		h.mu.Unlock()
		return c, nil
	}
	ifIndex := func(name string) (int, error) {
		if name == "eth0" {
			return 7, nil
		}
		return 0, errors.New("no such network interface")
	}
	e, err := newEngine(Options{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:       h.clk,
		OnLateReply: func(opID, seq uint32, sentAt time.Time) { h.late <- lateCall{opID, seq, sentAt} },
	}, open, ifIndex)
	if err != nil {
		t.Fatal(err)
	}
	h.e = e
	t.Cleanup(func() { e.Close() })
	return h
}

func (h *harness) conn(v6 bool, vrf string) *fakeConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[sockKey{v6: v6, vrf: vrf}]
}

// start runs Echo in a goroutine and returns the channel of its reply.
func (h *harness) start(req Request) <-chan Reply {
	ch := make(chan Reply, 1)
	go func() { ch <- h.e.Echo(h.t.Context(), req) }()
	return ch
}

func (h *harness) wait(ch <-chan Reply) Reply {
	h.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		h.t.Fatal("Echo did not return")
		return Reply{}
	}
}

// waitTimer blocks until the Echo timeout timer is armed (the sweeper's timer
// is always pending as well).
func (h *harness) waitTimer(n int) {
	h.clk.BlockUntil(1 + n)
}

// lateCall is one OnLateReply call.
type lateCall struct {
	opID, seq uint32
	sentAt    time.Time
}

// expectLate waits for OnLateReply(opID, seq, ...) and returns the sentAt
// it was given.
func (h *harness) expectLate(opID, seq uint32) time.Time {
	h.t.Helper()
	select {
	case got := <-h.late:
		if got.opID != opID || got.seq != seq {
			h.t.Fatalf("OnLateReply(%d, %d), want (%d, %d)", got.opID, got.seq, opID, seq)
		}
		return got.sentAt
	case <-time.After(5 * time.Second):
		h.t.Fatalf("OnLateReply(%d, %d) was not called", opID, seq)
	}
	return time.Time{}
}

func (h *harness) expectNoLate() {
	h.t.Helper()
	select {
	case got := <-h.late:
		h.t.Fatalf("unexpected OnLateReply(%d, %d)", got.opID, got.seq)
	default:
	}
}

// echoReply turns a sent Echo Request into the reply the target would send,
// wrapped in an IPv4 header for IPv4. mutate may alter the ICMP message
// before the checksum is computed.
//
//declscope:package // a fixture shared by the engine and packet tests
func echoReply(p sentPkt, mutate func(icmp []byte)) []byte {
	icmp := append([]byte(nil), p.msg...)
	if p.dst.Is6() {
		icmp[0] = icmp6EchoReply
	} else {
		icmp[0] = icmp4EchoReply
	}
	if mutate != nil {
		mutate(icmp)
	}
	if p.dst.Is6() {
		return icmp
	}
	icmp[2], icmp[3] = 0, 0
	binary.BigEndian.PutUint16(icmp[2:], icmpChecksum(icmp))
	return ipv4Packet(p.dst, local4, icmp)
}

//declscope:package // a fixture shared by the engine and packet tests
func ipv4Packet(src, dst netip.Addr, payload []byte) []byte {
	b := make([]byte, 20+len(payload))
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	b[8] = 64
	b[9] = protoICMP
	s, d := src.As4(), dst.As4()
	copy(b[12:], s[:])
	copy(b[16:], d[:])
	binary.BigEndian.PutUint16(b[10:], icmpChecksum(b[:20]))
	copy(b[20:], payload)
	return b
}

// icmpError builds an ICMP error from router quoting the sent request. For
// IPv4 quoteLen limits the quoted bytes after the IP header (RFC 792: 8).
//
//declscope:package // a fixture shared by the engine and packet tests
func icmpError(p sentPkt, typ, code uint8, from netip.Addr, quoteLen int) []byte {
	var quoted []byte
	if p.dst.Is6() {
		q := make([]byte, 40+len(p.msg))
		q[0] = 0x60
		binary.BigEndian.PutUint16(q[4:], uint16(len(p.msg)))
		q[6] = protoICMPv6
		q[7] = 64
		s, d := netip.MustParseAddr("fd00:100:1::10").As16(), p.dst.As16()
		copy(q[8:], s[:])
		copy(q[24:], d[:])
		copy(q[40:], p.msg)
		quoted = q
	} else {
		quoted = ipv4Packet(local4, p.dst, p.msg)
		if quoteLen >= 0 && 20+quoteLen < len(quoted) {
			quoted = quoted[:20+quoteLen]
		}
	}
	icmp := make([]byte, 8+len(quoted))
	icmp[0], icmp[1] = typ, code
	copy(icmp[8:], quoted)
	if p.dst.Is6() {
		return icmp
	}
	binary.BigEndian.PutUint16(icmp[2:], icmpChecksum(icmp))
	return ipv4Packet(from, local4, icmp)
}
