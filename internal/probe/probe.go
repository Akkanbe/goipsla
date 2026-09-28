// Package probe is the ICMP engine: it sends Echo Requests on shared raw
// sockets and matches replies, ICMP errors and timeouts to the requests.
//
//declscope:core
package probe

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"goipsla/internal/clock"
)

// ErrClosed is Reply.Err for requests made after, or interrupted by, Close.
//
//declscope:ignore overexported // documented API: docs/probe-engine.md names ErrClosed as the error of Close
var ErrClosed = errors.New("probe: engine closed")

// ErrSuperseded is Reply.Err for an Echo whose (Identifier, Sequence) was
// taken over by a newer request of the same operation before it ended: the
// first attempt of a new life (restart, reset) reuses Seq while the
// canceled attempt of the old life may still be waiting.
//
//declscope:ignore overexported // documented API: docs/probe-engine.md, "Matching", names ErrSuperseded
var ErrSuperseded = errors.New("probe: superseded by a newer request of the operation")

// Payload parameters of an Echo Request.
const (
	MinDataSize    = 28         // lower bound of request-data-size (also the Cisco default)
	DefaultPattern = 0xABCDABCD // default data-pattern
)

// Request describes one Echo Request.
type Request struct {
	OpID      uint32
	Seq       uint32
	Target    netip.Addr
	Source    netip.Addr // left to the kernel when !IsValid()
	Interface string     // outgoing interface name; empty means unspecified
	VRF       string     // VRF device name; empty means the default routing table
	TOS       uint8      // IPv4 ToS byte or IPv6 Traffic Class
	FlowLabel uint32     // IPv6 only
	DataSize  int        // request-data-size, at least MinDataSize
	Pattern   uint32     // data-pattern
	Verify    bool       // compare the reply's data with what was sent
	Timeout   time.Duration
}

// Outcome classifies how an Echo attempt ended.
type Outcome uint8

// Outcomes of Engine.Echo.
const (
	OutcomeReply       Outcome = iota // valid reply; RTT is set
	OutcomeTimeout                    // no reply within Timeout
	OutcomeUnreachable                // an ICMP error (Destination Unreachable / Time Exceeded) for this request
	OutcomeVerifyError                // a reply arrived but its data did not match (Verify=true)
	OutcomeError                      // local failure (socket, send); see Err
)

// Reply is the result of Engine.Echo.
type Reply struct {
	Outcome    Outcome
	RTT        time.Duration
	SentAt     time.Time
	ReceivedAt time.Time
	Detail     string // e.g. "destination unreachable: host, from 10.100.1.254" / "time exceeded, from ..."
	Err        error
}

// Engine sends Echo Requests. It is used concurrently by many operations.
type Engine interface {
	// Echo sends one Echo Request and waits for a reply, an ICMP error, the
	// timeout or the end of ctx, whichever comes first.
	Echo(ctx context.Context, req Request) Reply
	// Jitter sends one icmp-jitter burst of ICMP Timestamp Requests (IPv4
	// only) and waits until every packet has been answered or has timed
	// out, or ctx ends.
	Jitter(ctx context.Context, req JitterRequest) JitterReply
	Close() error
	// Stats returns the diagnostic counters since New.
	Stats() Stats
	// Forget releases the ICMP Identifier and the Timestamp sequence
	// counter of opID on every socket, for an operation that was deleted.
	// Unknown IDs are ignored.
	Forget(opID uint32)
}

// Stats are the engine's diagnostic counters since New, for all sockets.
// Packets that are not replies to a waiting request are dropped and
// counted by reason.
type Stats struct {
	Foreign    uint64 // not ours: other processes' pings, unknown identifier / sequence, bad magic, another source
	Malformed  uint64 // unparsable or with a bad checksum
	Late       uint64 // replies to our requests after their timeout, and duplicates
	Other      uint64 // other ICMP types that passed the socket filter
	RecvErrors uint64 // failed reads from a socket
}

// Options configures New.
type Options struct {
	Logger *slog.Logger // slog.Default() if nil
	// Clock is clock.Real() if nil; it drives the timeout timers. RTT is
	// ReceivedAt - SentAt.
	//
	//declscope:ignore overexported // documented option: docs/probe-engine.md, "RTT measurement", describes the Clock that times the requests
	Clock clock.Clock
	// OnLateReply is called when a reply that arrives after its timeout, or a
	// duplicate reply, matches a request this Engine sent. The caller counts
	// it as a sequenceError. Ignored if nil.
	// sentAt is the send time of the request the reply answers (taken with
	// Clock), so that the caller can tell an earlier life's attempt from
	// the current one that reuses Seq.
	OnLateReply func(opID, seq uint32, sentAt time.Time)
}
