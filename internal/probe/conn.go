package probe

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// errConnClosed is returned by packetConn.ReadFrom once the conn is closed.
//
//declscope:package // the socket contract between the engine and its packetConn implementations
var errConnClosed = errors.New("probe: socket closed")

// errConnSendTimeout is returned by packetConn.WriteTo when the socket stayed
// unwritable until connControl.Deadline.
//
//declscope:package // the socket contract between the engine and its packetConn implementations
var errConnSendTimeout = errors.New("probe: send timed out")

// connControl carries the per-packet options of one request.
//
//declscope:package // the socket contract between the engine and its packetConn implementations
type connControl struct {
	Src       netip.Addr // source address; kernel's choice when !IsValid()
	IfIndex   int        // outgoing interface; 0 means unspecified
	TOS       uint8      // IPv4 ToS or IPv6 Traffic Class; 0 leaves the socket default
	ScopeID   int        // IPv6 scope of a link-local destination
	FlowLabel uint32     // IPv6 flow label; 0 means none
	Deadline  time.Time  // give up with errConnSendTimeout if unwritable until then (engine clock); zero waits
}

// packetConn is the part of a raw ICMP socket the engine uses. The Linux
// implementation is rawConn; tests substitute a fake.
//
//declscope:package // the socket contract between the engine and its packetConn implementations
type packetConn interface {
	// WriteTo sends one ICMP message (starting at the ICMP header) to dst.
	WriteTo(msg []byte, dst netip.Addr, ctl connControl) error
	// ReadFrom blocks until a packet arrives or the conn is closed. For IPv4
	// the packet includes the IP header. rx is the kernel receive timestamp
	// (wall clock), or the zero time if none was delivered. After Close it
	// returns errConnClosed.
	ReadFrom(buf []byte) (n int, from netip.Addr, rx time.Time, err error)
	// Close closes the socket and unblocks ReadFrom.
	Close() error
}

// connOpener opens the socket for an address family and VRF ("" = default table).
//
//declscope:package // the engine takes it; New passes openRaw and tests a fake
type connOpener func(v6 bool, vrf string) (packetConn, error)

// connFlowLabelError reports that the socket could not send with a flow label.
// The engine then sends without it and warns once.
//
//declscope:package // the socket contract between the engine and its packetConn implementations
type connFlowLabelError struct {
	label uint32
	err   error
}

func (e *connFlowLabelError) Error() string {
	return fmt.Sprintf("flow label 0x%05x: %v", e.label, e.err)
}

func (e *connFlowLabelError) Unwrap() error { return e.err }
