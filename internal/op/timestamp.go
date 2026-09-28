package op

import "time"

// RFC 792 timestamps: 32-bit milliseconds since midnight UT; the high-order
// bit flags a non-standard value.
const (
	//declscope:package // the jitter arithmetic masks and tests the flag
	timestampNonStandard = 0x80000000
	timestampValueMask   = 0x7fffffff
	timestampMsPerDay    = 86_400_000 // the range of a timestamp
)

// timestampDiff returns a − b for RFC 792 timestamps in ms, ignoring the
// non-standard flag and correcting the wrap at midnight UT.
//
//declscope:package // the jitter arithmetic takes every difference of timestamps with it
func timestampDiff(a, b uint32) int64 {
	const halfDay = timestampMsPerDay / 2
	d := int64(a&timestampValueMask) - int64(b&timestampValueMask)
	switch {
	case d < -halfDay:
		d += timestampMsPerDay
	case d > halfDay:
		d -= timestampMsPerDay
	}
	return d
}

// timestampOf is the RFC 792 timestamp of t.
//
//declscope:package // the one-way delay (and the jitter tests) convert receive times with it
func timestampOf(t time.Time) uint32 {
	ms := t.UnixMilli() % timestampMsPerDay
	if ms < 0 {
		ms += timestampMsPerDay
	}
	return uint32(ms)
}
