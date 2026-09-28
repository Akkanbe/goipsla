package main

import (
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"goipsla/internal/api"
)

// fmtNow is the reference time for "3s ago" and uptimes; tests replace it.
//
//declscope:package // shared CLI helper
var fmtNow = time.Now

//declscope:package // shared CLI helper
func fmtTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

//declscope:package // shared CLI helper
func fmtDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// fmtMs prints a millisecond value with three decimals.
//
//declscope:package // shared CLI helper
func fmtMs(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

//declscope:package // shared CLI helper
func fmtRTT(l api.LatestJSON) string {
	if l.RTTMs == nil {
		return "-"
	}
	return fmtMs(*l.RTTMs)
}

//declscope:package // shared CLI helper
func fmtRC(l api.LatestJSON) string {
	if !l.Valid {
		return "-"
	}
	return l.Code.String()
}

// fmtAvg prints the average RTT of counters, "-" without completions.
//
//declscope:package // shared CLI helper
func fmtAvg(c api.CountersJSON) string {
	if c.Completions == 0 {
		return "-"
	}
	return fmtMs(c.RTTAvgMs)
}

// fmtMinAvgMax prints "min/avg/max" in ms, "-" without completions.
//
//declscope:package // shared CLI helper
func fmtMinAvgMax(c api.CountersJSON) string {
	if c.Completions == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%s/%d", c.RTTMinMs, fmtMs(c.RTTAvgMs), c.RTTMaxMs)
}

// fmtClock prints the local time of day.
//
//declscope:package // shared CLI helper
func fmtClock(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("15:04:05")
}

// fmtStamp prints a local date and time, for the vertical views.
//
//declscope:package // shared CLI helper
func fmtStamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

// fmtAgo prints how long ago t was ("3s ago", "2m10s ago").
//
//declscope:package // shared CLI helper
func fmtAgo(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	d := fmtNow().Sub(*t).Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}

//declscope:package // shared CLI helper
func fmtUint(v uint64) string { return strconv.FormatUint(v, 10) }
