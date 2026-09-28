// The API of package probe (contract types); part of its core.
//
//declscope:core

package probe

import (
	"errors"
	"net/netip"
	"time"
)

var (
	// ErrSkipped is JitterPacket.Err for a packet that was not sent because
	// its slot on the send grid was missed by a full Interval (Cisco's
	// "timerwheel slot ... missed", counted as Packet Skipped).
	ErrSkipped = errors.New("probe: send slot missed")
	// ErrUnsupportedFamily is JitterReply.Err for an IPv6 target: ICMP
	// Timestamp exists only in ICMPv4.
	ErrUnsupportedFamily = errors.New("probe: icmp timestamp requires an ipv4 target")
)

// JitterRequest describes one icmp-jitter burst of ICMP Timestamp Requests.
type JitterRequest struct {
	OpID       uint32
	Seq        uint32     // burst number
	Target     netip.Addr // IPv4 only; IPv6 yields ErrUnsupportedFamily
	Source     netip.Addr
	Interface  string
	VRF        string
	TOS        uint8
	NumPackets int
	Interval   time.Duration
	Timeout    time.Duration // per packet, counted from its send time
}

// JitterPacket is the outcome of one packet of a burst.
type JitterPacket struct {
	Index      int     // 0-based
	Outcome    Outcome // OutcomeReply / OutcomeTimeout / OutcomeUnreachable / OutcomeError (not sent = skipped)
	SentAt     time.Time
	ReceivedAt time.Time // OutcomeReply only
	RTT        time.Duration
	Originate  uint32 // Originate sent (ms since midnight UT)
	Receive    uint32 // Receive of the reply (raw 32 bits)
	Transmit   uint32 // Transmit of the reply (raw 32 bits)
	Late       bool   // a reply arrived after the packet's Timeout but before the burst ended (Outcome stays OutcomeTimeout)
	ArrivalPos int    // arrival order among the replies (0-based); OutcomeReply only
	Detail     string // ICMP error description for OutcomeUnreachable
	Err        error
}

// JitterReply is the result of Engine.Jitter.
type JitterReply struct {
	Packets []JitterPacket // NumPackets entries in Index order
	Err     error          // set when the burst could not run or no packet could be sent
}
