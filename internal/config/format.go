//declscope:namespace format

package config

import (
	"strconv"
	"time"
)

// This file prints durations the way the configuration file writes them.

// FormatDuration prints d in the unit the configuration file uses for
// second-granular keys: "60s", falling back to "1500ms".
func FormatDuration(d time.Duration) string {
	switch {
	case d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	case d%time.Millisecond == 0:
		return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
	default:
		return d.String()
	}
}

// FormatMillis prints d in milliseconds, the unit Cisco uses for timeout,
// threshold and interval: "5000ms".
func FormatMillis(d time.Duration) string {
	if d%time.Millisecond != 0 {
		return d.String()
	}
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}
