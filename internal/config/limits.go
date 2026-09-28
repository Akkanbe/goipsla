//declscope:namespace limit

package config

import (
	"math"
	"strconv"
	"time"
)

// This file holds the accepted range of every bounded key, as documented in
// docs/config.md. The parser checks each value against its limit.

// durationLimit is the accepted range of a duration key and the unit the
// value must be a whole number of (time.Second or time.Millisecond).
//
//declscope:package // the parser checks durations against these limits
type durationLimit struct {
	min, max, unit time.Duration
}

// format prints v as a whole number of the limit's unit: "604800s", "20ms".
//
//declscope:package // the parser prints the limits in its errors
func (l durationLimit) format(v time.Duration) string {
	if l.unit == time.Second {
		return strconv.FormatInt(int64(v/time.Second), 10) + "s"
	}
	return strconv.FormatInt(int64(v/time.Millisecond), 10) + "ms"
}

// intLimit is the accepted range of an integer key.
//
//declscope:package // the parser checks integers against these limits
type intLimit struct {
	min, max int64
}

// Limits of the duration keys.
//
//declscope:package // the parser checks durations against these limits
var (
	frequencyLimit        = durationLimit{time.Second, 604800 * time.Second, time.Second}
	timeoutLimit          = durationLimit{time.Millisecond, 604800000 * time.Millisecond, time.Millisecond}
	thresholdLimit        = durationLimit{0, 60000 * time.Millisecond, time.Millisecond}
	intervalLimit         = durationLimit{time.Millisecond, 60000 * time.Millisecond, time.Millisecond}
	distIntervalLimit     = durationLimit{time.Millisecond, 100 * time.Millisecond, time.Millisecond}
	enhancedIntervalLimit = durationLimit{time.Second, 3600 * time.Second, time.Second}
	lifeLimit             = durationLimit{time.Second, 2147483647 * time.Second, time.Second}
	ageoutLimit           = durationLimit{0, 2073600 * time.Second, time.Second}
	trackDelayLimit       = durationLimit{0, 180 * time.Second, time.Second}
	// unboundedMillisLimit only checks the unit; the timeout key reports
	// its own range errors.
	unboundedMillisLimit = durationLimit{math.MinInt64, math.MaxInt64, time.Millisecond}
)

// Limits of the integer keys.
//
//declscope:package // the parser checks integers against these limits
var (
	idLimit              = intLimit{1, MaxOpID}
	dsByteLimit          = intLimit{0, 255} // tos, traffic-class
	flowLabelLimit       = intLimit{0, 1048575}
	dataSizeLimit        = intLimit{28, MaxRequestDataSize} // 28: probe.MinDataSize, not imported to keep config free of probe
	dataPatternLimit     = intLimit{0, 0xFFFFFFFF}
	numPacketsLimit      = intLimit{1, 1000}
	livesKeptLimit       = intLimit{0, 2}
	bucketsKeptLimit     = intLimit{1, 60}
	hoursKeptLimit       = intLimit{0, 25}
	distBucketsLimit     = intLimit{1, 20}
	enhancedBucketsLimit = intLimit{1, 100}
	thresholdCountLimit  = intLimit{1, 16}
	thresholdValueLimit  = intLimit{0, 2147483647}
)

// Other limits.
//
//declscope:package // the parser and the validation enforce these limits
const (
	operationsLimit  = MaxOperations // operations after template expansion
	tagLengthLimit   = 128           // characters (rttMonCtrlAdminLongTag)
	ownerLengthLimit = 255           // characters
	// recurringLimit is the period of a recurring schedule: its life must be
	// shorter, and life + ageout longer unless ageout is 0.
	recurringLimit = 24 * time.Hour
)
