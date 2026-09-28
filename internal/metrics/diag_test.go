// A test of the collector; it shares the collector's namespace.
//
//declscope:namespace metrics

package metrics

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"goipsla/internal/probe"
)

// diagSource adds the diagnostic counters to a fakeSource.
type diagSource struct {
	fakeSource
	d Diagnostics
}

func (s diagSource) Diagnostics() Diagnostics { return s.d }

func TestDiagnosticsMetrics(t *testing.T) {
	src := diagSource{threeRows(), Diagnostics{
		Probe:            probe.Stats{Foreign: 11, Malformed: 2, Late: 3, Other: 4, RecvErrors: 5},
		ResultsDiscarded: 6,
	}}
	mfs, _ := scrape(t, src)
	for name, want := range map[string]map[string]float64{
		"goipsla_probe_packets_dropped_total": {"reason=foreign": 11, "reason=malformed": 2, "reason=other": 4},
		"goipsla_probe_late_replies_total":    {"": 3},
		"goipsla_probe_receive_errors_total":  {"": 5},
		"goipsla_results_discarded_total":     {"": 6},
	} {
		mf := mfs[name]
		if mf == nil {
			t.Errorf("%s missing", name)
			continue
		}
		if mf.GetType() != dto.MetricType_COUNTER {
			t.Errorf("%s: type %v", name, mf.GetType())
		}
		if diff := cmp.Diff(want, samples(mf)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", name, diff)
		}
	}
}

func TestDiagnosticsLint(t *testing.T) {
	problems, err := testutil.CollectAndLint(newCollector(diagSource{threeRows(), Diagnostics{}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("%s: %s", p.Metric, p.Text)
	}
}

// TestDiagnosticsAbsent: a Source without Diagnostics exports none of them.
func TestDiagnosticsAbsent(t *testing.T) {
	mfs, _ := scrape(t, threeRows())
	for _, name := range []string{"goipsla_probe_packets_dropped_total", "goipsla_probe_late_replies_total",
		"goipsla_probe_receive_errors_total", "goipsla_results_discarded_total"} {
		if mfs[name] != nil {
			t.Errorf("%s present without Diagnostics", name)
		}
	}
}
