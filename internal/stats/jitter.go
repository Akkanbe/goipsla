// The jitter accumulators are the store's working parts; their unexported methods are the store's.
//
//declscope:namespace store

package stats

import "goipsla/internal/op"

// JitterCounters are the accumulators of rttMonIcmpJitterStatsTable
// (CISCO-RTTMON-ICMP-MIB). Hour groups and the life totals of an
// icmp-jitter operation carry them; they are nil for other types. Every
// field sums the per-burst values of op.JitterResult, except the minimums
// and maximums, which span all bursts.
type JitterCounters struct {
	NumRTT, RTTSumMs, RTTSum2Ms, RTTMinMs, RTTMaxMs                  uint64
	NumOverThreshold                                                 uint64
	PosSD, NegSD, PosDS, NegDS                                       op.JitterSide
	PktLoss, PktLateArrival, PktOutSeqSD, PktOutSeqDS, PktOutSeqBoth uint64
	MinSucPktLoss, MaxSucPktLoss                                     uint64 // over the bursts that lost packets
	Skipped                                                          uint64
	NumOW                                                            uint64
	OWSD, OWDS                                                       op.JitterSide
}

// AvgJitterMs is the mean absolute jitter over both signs and directions.
func (c *JitterCounters) AvgJitterMs() float64 {
	return sidesAvg(c.PosSD, c.NegSD, c.PosDS, c.NegDS)
}

// AvgSDJitterMs is the mean absolute source-to-destination jitter.
func (c *JitterCounters) AvgSDJitterMs() float64 { return sidesAvg(c.PosSD, c.NegSD) }

// AvgDSJitterMs is the mean absolute destination-to-source jitter.
func (c *JitterCounters) AvgDSJitterMs() float64 { return sidesAvg(c.PosDS, c.NegDS) }

func sidesAvg(sides ...op.JitterSide) float64 {
	var n, sum uint64
	for _, s := range sides {
		n += s.Num
		sum += s.SumMs
	}
	return avg(sum, n)
}

// add accounts one burst.
func (c *JitterCounters) add(r *op.JitterResult) {
	if r.NumRTT > 0 {
		if c.NumRTT == 0 || r.RTTMinMs < c.RTTMinMs {
			c.RTTMinMs = r.RTTMinMs
		}
		c.RTTMaxMs = max(c.RTTMaxMs, r.RTTMaxMs)
	}
	c.NumRTT += r.NumRTT
	c.RTTSumMs += r.RTTSumMs
	c.RTTSum2Ms += r.RTTSum2Ms
	c.NumOverThreshold += r.NumOverThreshold

	c.PosSD.Merge(r.PosSD)
	c.NegSD.Merge(r.NegSD)
	c.PosDS.Merge(r.PosDS)
	c.NegDS.Merge(r.NegDS)

	if r.MaxSucPktLoss > 0 { // bursts without loss do not count
		if c.MaxSucPktLoss == 0 || r.MinSucPktLoss < c.MinSucPktLoss {
			c.MinSucPktLoss = r.MinSucPktLoss
		}
		c.MaxSucPktLoss = max(c.MaxSucPktLoss, r.MaxSucPktLoss)
	}
	c.PktLoss += r.PktLoss
	c.PktLateArrival += r.PktLateArrival
	c.PktOutSeqSD += r.PktOutSeqSD
	c.PktOutSeqDS += r.PktOutSeqDS
	c.PktOutSeqBoth += r.PktOutSeqBoth
	c.Skipped += uint64(max(r.Skipped, 0))

	c.NumOW += r.NumOW
	c.OWSD.Merge(r.OWSD)
	c.OWDS.Merge(r.OWDS)
}

func cloneJitterCounters(c *JitterCounters) *JitterCounters {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func cloneJitterResult(r *op.JitterResult) *op.JitterResult {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}
