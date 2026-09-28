// Package op runs single attempts of IP SLA operations (icmp-echo,
// icmp-jitter) and classifies their outcome with Cisco return codes.
//
//declscope:core
package op

import (
	"context"
	"fmt"
	"strings"
	"time"

	"goipsla/internal/config"
)

// ReturnCode is RttResponseSense from CISCO-RTTMON-TC-MIB. Only the values an
// ICMP operation can produce (plus their neighbors in the MIB) are defined.
type ReturnCode int

// Values from RttResponseSense.
const (
	RCOther ReturnCode = 0
	RCOK    ReturnCode = 1
	//declscope:ignore overexported // RttResponseSense value (CISCO-RTTMON-TC-MIB); the enumeration is kept complete (docs/statistics.md, "Accounting model")
	RCDisconnected  ReturnCode = 2
	RCOverThreshold ReturnCode = 3
	RCTimeout       ReturnCode = 4
	RCBusy          ReturnCode = 5
	//declscope:ignore overexported // RttResponseSense value (CISCO-RTTMON-TC-MIB); the enumeration is kept complete (docs/statistics.md, "Accounting model")
	RCNotConnected  ReturnCode = 6
	RCDropped       ReturnCode = 7
	RCSequenceError ReturnCode = 8
	RCVerifyError   ReturnCode = 9
	//declscope:ignore overexported // RttResponseSense value (CISCO-RTTMON-TC-MIB); the enumeration is kept complete (docs/statistics.md, "Accounting model")
	RCApplicationSpecific ReturnCode = 10
	RCError               ReturnCode = 16
)

// returnCodeNames holds the MIB enumeration labels and the CLI display names.
var returnCodeNames = map[ReturnCode]struct{ mib, display string }{
	RCOther:               {"other", "Unknown"},
	RCOK:                  {"ok", "OK"},
	RCDisconnected:        {"disconnected", "Disconnected"},
	RCOverThreshold:       {"overThreshold", "Over Threshold"},
	RCTimeout:             {"timeout", "Timeout"},
	RCBusy:                {"busy", "Busy"},
	RCNotConnected:        {"notConnected", "Not Connected"},
	RCDropped:             {"dropped", "Dropped"},
	RCSequenceError:       {"sequenceError", "Sequence Error"},
	RCVerifyError:         {"verifyError", "Verify Error"},
	RCApplicationSpecific: {"applicationSpecific", "Application Specific"},
	RCError:               {"error", "Internal Error"},
}

// String returns the MIB enumeration label ("ok", "overThreshold", ...). It is
// also the JSON representation. Undefined values print as "ReturnCode(N)".
func (rc ReturnCode) String() string {
	if n, ok := returnCodeNames[rc]; ok {
		return n.mib
	}
	return fmt.Sprintf("ReturnCode(%d)", int(rc))
}

// Display returns the Cisco CLI style name ("OK", "Over Threshold",
// "Timeout", ...). RCOther and undefined values display as "Unknown", which
// is what IOS shows before the first attempt completes.
func (rc ReturnCode) Display() string {
	if n, ok := returnCodeNames[rc]; ok {
		return n.display
	}
	return "Unknown"
}

// HasRTT reports whether an attempt with return code rc has an RTT: ok and
// overThreshold (a timed completion).
func (rc ReturnCode) HasRTT() bool {
	return rc == RCOK || rc == RCOverThreshold
}

// RTTMillis converts a round-trip time to milliseconds rounded to the
// microsecond (three decimals), the unit of the JSON, the logs and the CLI.
func RTTMillis(d time.Duration) float64 {
	return float64(d.Round(time.Microsecond)/time.Microsecond) / 1000
}

// MarshalText encodes rc as its MIB label, so JSON carries "timeout" rather
// than 4.
func (rc ReturnCode) MarshalText() ([]byte, error) {
	return []byte(rc.String()), nil
}

// UnmarshalText accepts what ParseReturnCode accepts.
func (rc *ReturnCode) UnmarshalText(text []byte) error {
	v, err := ParseReturnCode(string(text))
	if err != nil {
		return err
	}
	*rc = v
	return nil
}

// ParseReturnCode converts a MIB label ("overThreshold") to a ReturnCode. The
// comparison ignores case so that CLI filters such as --rc overthreshold work.
func ParseReturnCode(s string) (ReturnCode, error) {
	for rc, n := range returnCodeNames {
		if strings.EqualFold(s, n.mib) {
			return rc, nil
		}
	}
	return 0, fmt.Errorf("unknown return code %q", s)
}

// Runner executes one attempt of an operation.
type Runner interface {
	// Run performs attempt seq, which the scheduler started at start, and
	// blocks until it completes (reply, ICMP error, timeout or the end of
	// ctx).
	Run(ctx context.Context, seq uint32, start time.Time) Result
}

// Result is the outcome of one attempt of an operation.
type Result struct {
	OpID   int
	Type   config.OpType
	Life   int           // index of the life the attempt belongs to (1-based); set by the manager, 0 from a Runner
	Seq    uint32        // attempt number, counting from 1 at the start of the operation's life
	Start  time.Time     // wall-clock time the attempt started
	End    time.Time     // wall-clock time the attempt completed (reply or timeout decision)
	RTT    time.Duration // valid only when Code is RCOK or RCOverThreshold; nanosecond resolution
	Code   ReturnCode
	Detail string        // extra information, e.g. "destination unreachable: host, from 10.100.1.254"
	Jitter *JitterResult // icmp-jitter only (defined in P3); nil for icmp-echo
}

// JitterSide accumulates one kind of jitter sample (positive or negative,
// per direction; negative values are stored as absolute values) or the
// one-way delays of one direction, in whole milliseconds.
type JitterSide struct {
	Num    uint64
	SumMs  uint64
	Sum2Ms uint64
	MinMs  uint64 // 0 while Num is 0
	MaxMs  uint64
}

// Add accounts one sample of v ms.
func (s *JitterSide) Add(v uint64) {
	if s.Num == 0 || v < s.MinMs {
		s.MinMs = v
	}
	if v > s.MaxMs {
		s.MaxMs = v
	}
	s.Num++
	s.SumMs += v
	s.Sum2Ms += v * v
}

// Merge adds the samples of o.
func (s *JitterSide) Merge(o JitterSide) {
	if o.Num == 0 {
		return
	}
	if s.Num == 0 || o.MinMs < s.MinMs {
		s.MinMs = o.MinMs
	}
	if o.MaxMs > s.MaxMs {
		s.MaxMs = o.MaxMs
	}
	s.Num += o.Num
	s.SumMs += o.SumMs
	s.Sum2Ms += o.Sum2Ms
}

// AvgMs is the mean sample, or 0 without samples.
func (s JitterSide) AvgMs() float64 {
	if s.Num == 0 {
		return 0
	}
	return float64(s.SumMs) / float64(s.Num)
}

// JitterResult is the result of one icmp-jitter burst, modeled on
// rttMonLatestIcmpJitterOperTable (CISCO-RTTMON-ICMP-MIB). All times are
// whole milliseconds.
type JitterResult struct {
	NumPackets int // packets in the burst
	Sent       int // packets sent
	Skipped    int // packets not sent (excluded from every other count)

	NumRTT           uint64 // replies
	RTTSumMs         uint64
	RTTSum2Ms        uint64
	RTTMinMs         uint64
	RTTMaxMs         uint64
	NumOverThreshold uint64 // replies with RTT_i > threshold

	PosSD, NegSD, PosDS, NegDS JitterSide

	PktLoss        uint64 // sent packets without a reply within their timeout
	PktLateArrival uint64 // replies after their timeout but before the burst ended
	PktOutSeqSD    uint64
	PktOutSeqDS    uint64
	PktOutSeqBoth  uint64
	MinSucPktLoss  uint64 // shortest run of successive losses; 0 without loss
	MaxSucPktLoss  uint64 // longest run of successive losses; 0 without loss

	OneWay bool // one-way delays were accumulated
	NumOW  uint64
	OWSD   JitterSide
	OWDS   JitterSide
}

// AvgJitterMs is the mean absolute jitter over the samples of both signs and
// both directions (rttMonLatestIcmpJitterAvgJitter).
func (r *JitterResult) AvgJitterMs() float64 {
	return avgSides(r.PosSD, r.NegSD, r.PosDS, r.NegDS)
}

// AvgSDJitterMs is the mean absolute source-to-destination jitter.
func (r *JitterResult) AvgSDJitterMs() float64 { return avgSides(r.PosSD, r.NegSD) }

// AvgDSJitterMs is the mean absolute destination-to-source jitter.
func (r *JitterResult) AvgDSJitterMs() float64 { return avgSides(r.PosDS, r.NegDS) }

func avgSides(sides ...JitterSide) float64 {
	var n, sum uint64
	for _, s := range sides {
		n += s.Num
		sum += s.SumMs
	}
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}
