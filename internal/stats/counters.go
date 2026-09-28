// Package stats accounts the results of IP SLA operations the way
// CISCO-RTTMON-MIB does: the latest result, hourly aggregated statistics
// with distribution buckets, the snapshot history (lives × buckets ×
// samples) and the enhanced history. See docs/statistics.md.
//
//declscope:namespace store
package stats

import (
	"math"
	"time"

	"goipsla/internal/op"
)

// Counters are the MIB-style accumulators shared by hour groups, enhanced
// history buckets and the life totals.
type Counters struct {
	Initiations    uint64
	Completions    uint64
	OverThresholds uint64
	Timeouts       uint64
	Busies         uint64
	Drops          uint64
	SequenceErrors uint64
	VerifyErrors   uint64
	RTTSumMs       uint64 // sum of the completion RTTs, truncated to ms
	RTTSum2Ms      uint64 // sum of their squares
	RTTMinMs       uint64 // 0 while Completions is 0
	RTTMaxMs       uint64
}

// Successes is the CLI "Number of successes": completions within the
// threshold (return code ok).
func (c Counters) Successes() uint64 {
	if c.OverThresholds > c.Completions {
		return 0
	}
	return c.Completions - c.OverThresholds
}

// Failures is the CLI "Number of failures": every initiation that was not a
// success, over-threshold completions included.
func (c Counters) Failures() uint64 {
	s := c.Successes()
	if s > c.Initiations {
		return 0
	}
	return c.Initiations - s
}

// AvgMs is the mean completion RTT in ms, or 0 without completions.
func (c Counters) AvgMs() float64 { return avg(c.RTTSumMs, c.Completions) }

// StdDevMs is sqrt(Sum²/N − (Sum/N)²) over the completions, or 0 for N = 0.
func (c Counters) StdDevMs() float64 { return stddev(c.RTTSumMs, c.RTTSum2Ms, c.Completions) }

func avg(sum, n uint64) float64 {
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

func stddev(sum, sum2, n uint64) float64 {
	if n == 0 {
		return 0
	}
	mean := float64(sum) / float64(n)
	v := float64(sum2)/float64(n) - mean*mean
	if v <= 0 { // rounding can make a zero variance slightly negative
		return 0
	}
	return math.Sqrt(v)
}

// rttMs truncates an RTT to whole milliseconds as Cisco reports it.
func rttMs(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d / time.Millisecond)
}

// isCompletion reports whether code is a timed completion (its RTT is
// accumulated).
func isCompletion(code op.ReturnCode) bool { return code.HasRTT() }

// isAttempt reports whether code stands for an initiated attempt. Busy (the
// attempt was not started) and sequenceError (a late or duplicate reply to an
// earlier attempt) are not attempts: they do not count as initiations, do
// not update Latest and are not written to the history.
func isAttempt(code op.ReturnCode) bool {
	return code != op.RCBusy && code != op.RCSequenceError
}

// add accounts one result with code and RTT ms (docs/statistics.md,
// "Accounting model").
func (c *Counters) add(code op.ReturnCode, ms uint64) {
	if isAttempt(code) {
		c.Initiations++
	}
	switch code {
	case op.RCOK, op.RCOverThreshold:
		if code == op.RCOverThreshold {
			c.OverThresholds++
		}
		if c.Completions == 0 || ms < c.RTTMinMs {
			c.RTTMinMs = ms
		}
		if ms > c.RTTMaxMs {
			c.RTTMaxMs = ms
		}
		c.Completions++
		c.RTTSumMs += ms
		c.RTTSum2Ms += ms * ms
	case op.RCTimeout:
		c.Timeouts++
	case op.RCVerifyError:
		c.VerifyErrors++
	case op.RCDropped, op.RCError:
		c.Drops++
	case op.RCBusy:
		c.Busies++
	case op.RCSequenceError:
		c.SequenceErrors++
	}
	// Other codes (other, disconnected, notConnected, applicationSpecific)
	// cannot come from an ICMP operation; they count as a failed initiation
	// only.
}

// DistBucket is one distribution bucket of an hour group. Only completions
// (over-threshold ones included) are distributed.
type DistBucket struct {
	Index       int    // 1-based
	LowerMs     uint64 // inclusive lower bound
	UpperMs     uint64 // exclusive upper bound; 0 for the last bucket (no upper bound)
	Completions uint64
	// OverThresholds counts the completions in this bucket that were over
	// the threshold (rttMonStatsCaptureOverThresholds); a subset of
	// Completions.
	OverThresholds uint64
	RTTSumMs       uint64
	RTTSum2Ms      uint64
	RTTMinMs       uint64
	RTTMaxMs       uint64
}

func (b *DistBucket) add(ms uint64, overThreshold bool) {
	if overThreshold {
		b.OverThresholds++
	}
	if b.Completions == 0 || ms < b.RTTMinMs {
		b.RTTMinMs = ms
	}
	if ms > b.RTTMaxMs {
		b.RTTMaxMs = ms
	}
	b.Completions++
	b.RTTSumMs += ms
	b.RTTSum2Ms += ms * ms
}

// newDist returns the empty buckets of an hour group: n buckets of width w
// ms, the last one unbounded. With n = 1 the width does not apply.
func newDist(n int, w uint64) []DistBucket {
	d := make([]DistBucket, n)
	for i := range d {
		d[i].Index = i + 1
		d[i].LowerMs = uint64(i) * w
		if i < n-1 {
			d[i].UpperMs = uint64(i+1) * w
		}
	}
	return d
}

// distIndex returns the 0-based bucket for ms: min(floor(ms / w), n − 1).
func distIndex(ms uint64, n int, w uint64) int {
	if n <= 1 || w == 0 {
		return 0
	}
	i := ms / w
	if i >= uint64(n-1) {
		return n - 1
	}
	return int(i)
}
