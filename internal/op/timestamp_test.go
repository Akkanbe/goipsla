package op

import "testing"

func TestTimestampDiff(t *testing.T) {
	for _, tc := range []struct {
		a, b uint32
		want int64
	}{
		{100, 40, 60},
		{40, 100, -60},
		{10, 86_399_990, 20},
		{86_399_990, 10, -20},
		{timestampNonStandard | 100, 40, 60},
		{43_200_000, 0, 43_200_000},
		{43_200_001, 0, 43_200_001 - timestampMsPerDay},
	} {
		if got := timestampDiff(tc.a, tc.b); got != tc.want {
			t.Errorf("tsDiff(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestOneWayDelaySetting: the one-way-delay setting decides whether one-way
// delays are accumulated.
