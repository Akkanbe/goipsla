// The icmp-jitter layouts of show statistics and show statistics aggregated
// (namespace "jitter").
//
//declscope:namespace jitter

package main

import (
	"fmt"
	"io"
	"strconv"

	"goipsla/internal/api"
)

// writeJitterStatistics prints an icmp-jitter operation after Cisco's
// show ip sla statistics for icmp-jitter: the latest burst in the RTT
// Values / Latency one-way time / Jitter Time / Over Threshold / Packet ...
// sections, then the burst counts. With details it adds the counters of the
// current life.
//
//declscope:package // show statistics uses it for icmp-jitter operations
func writeJitterStatistics(w io.Writer, d *api.OperationDetail, details bool) error {
	l, t := d.Latest, d.Totals
	fmt.Fprintf(w, "IPSLA operation id: %d\n", d.ID)
	fmt.Fprintf(w, "Type of operation: %s\n", d.Type)
	target := d.Target
	if d.VRF != "" {
		target += " (vrf " + d.VRF + ")"
	}
	fmt.Fprintf(w, "Target address: %s\n", target)
	if d.Tag != "" {
		fmt.Fprintf(w, "Tag: %s\n", d.Tag)
	}
	switch {
	case !l.Valid:
		fmt.Fprintln(w, "        Latest RTT: Unknown")
	case l.RTTMs != nil:
		fmt.Fprintf(w, "        Latest RTT: %s milliseconds\n", fmtMs(*l.RTTMs))
	default:
		fmt.Fprintln(w, "        Latest RTT: NoConnection/Busy/Timeout")
	}
	if l.Start != nil {
		fmt.Fprintf(w, "Latest operation start time: %s\n", fmtStamp(*l.Start))
	} else {
		fmt.Fprintln(w, "Latest operation start time: -")
	}
	fmt.Fprintf(w, "Latest operation return code: %s\n", l.Code.Display())
	if l.Detail != "" {
		fmt.Fprintf(w, "Latest operation detail: %s\n", l.Detail)
	}
	if j := l.Jitter; j != nil {
		writeJitterBlock(w, jitterViewOfResult(j), "")
	}
	fmt.Fprintf(w, "Number of successes: %s\n", fmtUint(t.Successes))
	fmt.Fprintf(w, "Number of failures: %s\n", fmtUint(t.Failures))
	fmt.Fprintf(w, "Operational state: %s\n", d.State)
	fmt.Fprintf(w, "Life start: %s (life %d)\n", fmtStamp(d.LifeStart), d.LifeIndex)
	if !details {
		return nil
	}
	fmt.Fprintln(w, "Bursts of the current life:")
	fmt.Fprintf(w, "        Initiations: %s  Completions: %s  Over thresholds: %s\n", fmtUint(t.Initiations), fmtUint(t.Completions), fmtUint(t.OverThresholds))
	fmt.Fprintf(w, "        Timeouts: %s  Busies: %s  Drops: %s  Sequence errors: %s\n", fmtUint(t.Timeouts), fmtUint(t.Busies), fmtUint(t.Drops), fmtUint(t.SequenceErrors))
	if c := d.TotalsJitter; c != nil {
		fmt.Fprintln(w, "Packets of the current life:")
		writeJitterBlock(w, jitterViewOfCounters(c), "        ")
	}
	return nil
}

// jitterView is what writeJitterBlock prints, from either the latest burst
// or the life's accumulated bursts.
type jitterView struct {
	numRTT, rttMin, rttMax     uint64
	rttAvg                     float64
	numOW                      uint64
	owSD, owDS                 api.JitterSideJSON
	posSD, negSD, posDS, negDS api.JitterSideJSON
	sdAvg, dsAvg               float64
	overTh, late               uint64
	oosSD, oosDS, oosBoth      uint64
	skipped, loss              uint64
	sucMin, sucMax             uint64
}

// jitterViewOfResult is the view of the latest burst.
func jitterViewOfResult(j *api.JitterResultJSON) jitterView {
	return jitterView{
		numRTT: j.NumRTT, rttMin: j.RTTMinMs, rttAvg: j.RTTAvgMs, rttMax: j.RTTMaxMs,
		numOW: j.NumOW, owSD: j.OWSD, owDS: j.OWDS,
		posSD: j.PosSD, negSD: j.NegSD, posDS: j.PosDS, negDS: j.NegDS,
		sdAvg: j.AvgSDJitterMs, dsAvg: j.AvgDSJitterMs,
		overTh: j.NumOverThreshold, late: j.PktLateArrival,
		oosSD: j.PktOutSeqSD, oosDS: j.PktOutSeqDS, oosBoth: j.PktOutSeqBoth,
		skipped: uint64(max(j.Skipped, 0)), loss: j.PktLoss, sucMin: j.MinSucPktLoss, sucMax: j.MaxSucPktLoss,
	}
}

// jitterViewOfCounters is the view of the bursts of the current life.
func jitterViewOfCounters(c *api.JitterCountersJSON) jitterView {
	return jitterView{
		numRTT: c.NumRTT, rttMin: c.RTTMinMs, rttAvg: c.RTTAvgMs, rttMax: c.RTTMaxMs,
		numOW: c.NumOW, owSD: c.OWSD, owDS: c.OWDS,
		posSD: c.PosSD, negSD: c.NegSD, posDS: c.PosDS, negDS: c.NegDS,
		sdAvg: c.AvgSDJitterMs, dsAvg: c.AvgDSJitterMs,
		overTh: c.NumOverThreshold, late: c.PktLateArrival,
		oosSD: c.PktOutSeqSD, oosDS: c.PktOutSeqDS, oosBoth: c.PktOutSeqBoth,
		skipped: c.Skipped, loss: c.PktLoss, sucMin: c.MinSucPktLoss, sucMax: c.MaxSucPktLoss,
	}
}

// writeJitterBlock prints the Cisco icmp-jitter sections, each line
// prefixed with indent. Packet Unprocessed is not tracked and not printed;
// the number of loss periods is not tracked either, so only their length is.
func writeJitterBlock(w io.Writer, v jitterView, indent string) {
	p := func(format string, a ...any) { fmt.Fprintf(w, indent+format+"\n", a...) }
	p("RTT Values:")
	p("        Number Of RTT: %s               RTT Min/Avg/Max: %s/%s/%s milliseconds", fmtUint(v.numRTT), fmtUint(v.rttMin), fmtMs(v.rttAvg), fmtUint(v.rttMax))
	p("Latency one-way time:")
	p("        Number of Latency one-way Samples: %s", fmtUint(v.numOW))
	p("        Source to Destination Latency one way Min/Avg/Max: %s milliseconds", jitterSideMinAvgMax(v.owSD))
	p("        Destination to Source Latency one way Min/Avg/Max: %s milliseconds", jitterSideMinAvgMax(v.owDS))
	p("Jitter Time:")
	p("        Number of SD Jitter Samples: %s", fmtUint(v.posSD.Num+v.negSD.Num))
	p("        Number of DS Jitter Samples: %s", fmtUint(v.posDS.Num+v.negDS.Num))
	p("        Source to Destination Jitter Min/Avg/Max: %s milliseconds", jitterMinAvgMax(v.posSD, v.negSD, v.sdAvg))
	p("        Destination to Source Jitter Min/Avg/Max: %s milliseconds", jitterMinAvgMax(v.posDS, v.negDS, v.dsAvg))
	p("Over Threshold:")
	pct := 0
	if v.numRTT > 0 {
		pct = int(float64(v.overTh)*100/float64(v.numRTT) + 0.5)
	}
	p("        Number Of RTT Over Threshold: %s (%d%%)", fmtUint(v.overTh), pct)
	p("Packet Late Arrival: %s", fmtUint(v.late))
	p("Out Of Sequence: %s", fmtUint(v.oosSD+v.oosDS+v.oosBoth))
	p("        Source to Destination: %s        Destination to Source %s", fmtUint(v.oosSD), fmtUint(v.oosDS))
	p("        In both Directions: %s", fmtUint(v.oosBoth))
	p("Packet Skipped: %s", fmtUint(v.skipped))
	p("Packet Loss: %s", fmtUint(v.loss))
	p("        Loss Period Length Min/Max: %s/%s", fmtUint(v.sucMin), fmtUint(v.sucMax))
}

// jitterSideMinAvgMax prints "min/avg/max" of one side, "0/0.000/0" without samples.
func jitterSideMinAvgMax(s api.JitterSideJSON) string {
	return fmtUint(s.MinMs) + "/" + fmtMs(s.AvgMs) + "/" + fmtUint(s.MaxMs)
}

// jitterMinAvgMax prints min/avg/max of the absolute jitter of one
// direction, over its positive and negative samples.
func jitterMinAvgMax(pos, neg api.JitterSideJSON, avg float64) string {
	var lo, hi uint64
	first := true
	for _, s := range []api.JitterSideJSON{pos, neg} {
		if s.Num == 0 {
			continue
		}
		if first || s.MinMs < lo {
			lo = s.MinMs
		}
		hi = max(hi, s.MaxMs)
		first = false
	}
	return fmtUint(lo) + "/" + fmtMs(avg) + "/" + fmtUint(hi)
}

// writeJitterAggregated prints the hour groups of an icmp-jitter operation:
// the burst counters, the packet RTTs and the jitter columns. Jitter has no
// distribution buckets.
//
//declscope:package // show statistics aggregated uses it for icmp-jitter operations
func writeJitterAggregated(w io.Writer, d *api.OperationDetail) error {
	tw := fmtTable(w)
	fmt.Fprintln(tw, "INDEX\tSTART\tRTTs\tMIN/AVG/MAX\tSUCC\tFAIL\tOVERTH\tTIMEOUT\tBUSY\tDROP\tSEQERR\tVERERR\tSDJ\tDSJ\tLOSS\tLATE\tOOS")
	for _, h := range d.Hours {
		c := h.Counters
		rtts, mam, sdj, dsj, loss, late, oos := "0", "-", "-", "-", "0", "0", "0"
		if j := h.Jitter; j != nil {
			rtts = fmtUint(j.NumRTT)
			if j.NumRTT > 0 {
				mam = fmtUint(j.RTTMinMs) + "/" + fmtMs(j.RTTAvgMs) + "/" + fmtUint(j.RTTMaxMs)
			}
			if j.PosSD.Num+j.NegSD.Num > 0 {
				sdj = fmtMs(j.AvgSDJitterMs)
			}
			if j.PosDS.Num+j.NegDS.Num > 0 {
				dsj = fmtMs(j.AvgDSJitterMs)
			}
			loss, late = fmtUint(j.PktLoss), fmtUint(j.PktLateArrival)
			oos = fmtUint(j.PktOutSeqSD + j.PktOutSeqDS + j.PktOutSeqBoth)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			strconv.Itoa(h.Index), fmtClock(h.Start), rtts, mam, fmtUint(c.Successes), fmtUint(c.Failures),
			fmtUint(c.OverThresholds), fmtUint(c.Timeouts), fmtUint(c.Busies), fmtUint(c.Drops), fmtUint(c.SequenceErrors), fmtUint(c.VerifyErrors),
			sdj, dsj, loss, late, oos)
	}
	return tw.Flush()
}
