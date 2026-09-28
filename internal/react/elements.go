//declscope:namespace value

package react

import (
	"time"

	"goipsla/internal/op"
)

// elementValue returns the monitored value of element for one result, and
// false when the result gives the element no value (it is then not
// evaluated and does not enter the windows).
//
//   - rtt: the RTT in whole ms, only for completions (ok / overThreshold).
//     For icmp-jitter this is Result.RTT, the average of the burst.
//   - timeout, verifyError: 1 if the result is that code, else 0, for every
//     attempt.
//   - jitter elements: only when Result.Jitter is set and at least one
//     packet was sent (an error attempt, Sent == 0, measured nothing: its
//     zero loss must not clear a loss reaction). Averages and maxima need at
//     least one sample of their kind; the packet counts then always have a
//     value. The latency elements need one-way delays (OneWay).
//
//declscope:package // the engine evaluates each reaction on this value
func elementValue(element string, r *op.Result) (int64, bool) {
	switch element {
	case "rtt":
		if !r.Code.HasRTT() {
			return 0, false
		}
		return int64(r.RTT / time.Millisecond), true
	case "timeout":
		return boolValue(r.Code == op.RCTimeout), true
	case "verifyError":
		return boolValue(r.Code == op.RCVerifyError), true
	}
	j := r.Jitter
	if j == nil || j.Sent == 0 {
		return 0, false
	}
	switch element {
	case "jitterAvg":
		return avgValue(j.AvgJitterMs(), j.PosSD, j.NegSD, j.PosDS, j.NegDS)
	case "jitterSDAvg":
		return avgValue(j.AvgSDJitterMs(), j.PosSD, j.NegSD)
	case "jitterDSAvg":
		return avgValue(j.AvgDSJitterMs(), j.PosDS, j.NegDS)
	case "maxOfPositiveSD":
		return maxValue(j.PosSD)
	case "maxOfNegativeSD":
		return maxValue(j.NegSD)
	case "maxOfPositiveDS":
		return maxValue(j.PosDS)
	case "maxOfNegativeDS":
		return maxValue(j.NegDS)
	case "packetLateArrival":
		return int64(j.PktLateArrival), true
	case "packetOutOfSequence":
		return int64(j.PktOutSeqBoth + j.PktOutSeqSD + j.PktOutSeqDS), true
	case "successivePacketLoss":
		return int64(j.MaxSucPktLoss), true
	case "packetLoss":
		return int64(j.PktLoss), true
	}
	if !j.OneWay {
		return 0, false
	}
	switch element {
	case "maxOfLatencySD":
		return maxValue(j.OWSD)
	case "maxOfLatencyDS":
		return maxValue(j.OWDS)
	case "latencySDAvg":
		return sideAvgValue(j.OWSD)
	case "latencyDSAvg":
		return sideAvgValue(j.OWDS)
	}
	return 0, false
}

func boolValue(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// avgValue truncates avg to whole ms when the sides have samples.
func avgValue(avg float64, sides ...op.JitterSide) (int64, bool) {
	var n uint64
	for _, s := range sides {
		n += s.Num
	}
	if n == 0 {
		return 0, false
	}
	return int64(avg), true
}

func maxValue(s op.JitterSide) (int64, bool) {
	if s.Num == 0 {
		return 0, false
	}
	return int64(s.MaxMs), true
}

func sideAvgValue(s op.JitterSide) (int64, bool) {
	if s.Num == 0 {
		return 0, false
	}
	return int64(s.SumMs / s.Num), true
}
