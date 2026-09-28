// A test of show statistics aggregated; it shares its namespace.
//
//declscope:namespace aggregated

package main

import (
	"strings"
	"testing"

	"goipsla/internal/api"
	"goipsla/internal/config"
)

// TestAggregatedOverThresholdColumn: the distribution table shows each
// bucket's over-threshold completions.
func TestAggregatedOverThresholdColumn(t *testing.T) {
	upper := uint64(20)
	d := &api.OperationDetail{}
	d.ID, d.Type, d.Target = 7, config.ICMPEcho, "192.0.2.7"
	d.Hours = []api.HourGroupJSON{{Index: 1, Dist: []api.DistBucketJSON{
		{Index: 1, LowerMs: 0, UpperMs: &upper, Completions: 8, Percent: 80, RTTAvgMs: 5},
		{Index: 2, LowerMs: 20, Completions: 2, OverThresholds: 2, Percent: 20, RTTAvgMs: 350},
	}}}
	var b strings.Builder
	if err := writeAggregated(&b, d, true); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"RANGE    COMPLETIONS  OVERTH  %     AVG(ms)\n" +
		"0-<20ms  8            0       80.0  5.000\n" +
		">=20ms   2            2       20.0  350.000\n"
	if !strings.HasSuffix(b.String(), want) {
		t.Errorf("got:\n%s\nwant suffix:\n%s", b.String(), want)
	}
}
