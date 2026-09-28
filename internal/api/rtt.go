//declscope:namespace rtt

package api

// This file averages the round-trip times for the JSON. A single RTT is
// converted by op.RTTMillis, and op.ReturnCode.HasRTT tells whether an
// attempt has one.

// rttAvgMs is the mean RTT in ms of n RTTs summing to sum ms.
//
//declscope:package // the JSON constructors convert RTTs with it
func rttAvgMs(sum, n uint64) float64 {
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}
