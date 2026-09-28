package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"goipsla/internal/api/apitest"
)

func TestShowGolden(t *testing.T) {
	sock := apitest.Serve(t, apitest.NewProvider())
	tests := []struct {
		name string
		args []string
	}{
		{"operations", []string{"show", "operations"}},
		{"operations_filtered", []string{"show", "operations", "--tag", "wan", "--rc", "OK"}},
		{"operations_json", []string{"-o", "json", "show", "operations", "--type", "icmp-jitter"}},
		{"statistics", []string{"show", "statistics"}},
		{"statistics_101", []string{"show", "statistics", "101"}},
		{"statistics_101_details", []string{"show", "statistics", "101", "--details"}},
		{"statistics_102", []string{"show", "statistics", "102"}},
		{"statistics_103", []string{"show", "statistics", "103"}},
		{"statistics_details_all", []string{"show", "statistics", "--details"}},
		{"statistics_101_json", []string{"show", "statistics", "101", "-o", "json"}},
		{"aggregated_101", []string{"show", "statistics", "aggregated", "101"}},
		{"aggregated_101_details", []string{"show", "statistics", "aggregated", "101", "--details"}},
		{"aggregated_all", []string{"show", "statistics", "aggregated"}},
		{"statistics_1", []string{"show", "statistics", "1"}},
		{"statistics_1_details", []string{"show", "statistics", "1", "--details"}},
		{"statistics_1_json", []string{"show", "statistics", "1", "-o", "json"}},
		{"aggregated_1", []string{"show", "statistics", "aggregated", "1"}},
		{"history_101", []string{"show", "history", "101"}},
		{"history_all", []string{"show", "history"}},
		{"history_all_json", []string{"show", "history", "-o", "json"}},
		{"enhanced_history_all", []string{"show", "enhanced-history"}},
		{"enhanced_history_101", []string{"show", "enhanced-history", "101"}},
		{"config_101", []string{"show", "config", "101"}},
		{"config_1", []string{"show", "config", "1"}},
		{"config_101_json", []string{"show", "config", "101", "-o", "json"}},
		{"health", []string{"health"}},
		{"health_json", []string{"health", "-o", "json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, errOut, err := runCtl(t, append([]string{"--socket", sock}, tt.args...)...)
			if err != nil || errOut != "" {
				t.Fatalf("err %v, stderr %q", err, errOut)
			}
			golden(t, tt.name, out)
		})
	}
}

// TestAggregatedDetailsJitter: --details on icmp-jitter prints the table
// without distributions and says so on stderr, once, with status 0.
func TestAggregatedDetailsJitter(t *testing.T) {
	sock := apitest.Serve(t, apitest.NewProvider())
	const note = "distribution is not kept for icmp-jitter\n"
	for _, tc := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"aggregated_1_details", []string{"show", "statistics", "aggregated", "1", "--details"}, note},
		{"", []string{"show", "statistics", "aggregated", "--details"}, note}, // one line for all operations
		{"", []string{"show", "statistics", "aggregated", "101", "--details"}, ""},
		{"", []string{"show", "statistics", "aggregated", "1"}, ""},
	} {
		out, errOut, err := runCtl(t, append([]string{"--socket", sock}, tc.args...)...)
		if err != nil || errOut != tc.stderr {
			t.Errorf("%v: err %v, stderr %q, want %q", tc.args, err, errOut, tc.stderr)
		}
		if tc.name != "" {
			golden(t, tc.name, out)
		}
	}
}

func TestShowEmptyAndErrors(t *testing.T) {
	sock := apitest.Serve(t, apitest.NewProvider())
	tests := []struct {
		name   string
		args   []string
		code   int // 0: success; -1: plain error (exit 1 via main)
		stderr string
		errMsg string
	}{
		{"no operations", []string{"show", "operations", "--tag", "none"}, 0, "no operations\n", ""},
		{"no history", []string{"show", "history", "103"}, 0, "no history buckets\n", ""},
		{"no enhanced", []string{"show", "enhanced-history", "102"}, 0, "no enhanced history buckets\n", ""},
		{"no hour groups", []string{"show", "statistics", "aggregated", "103"}, 0, "no hour groups\n", ""},
		{"not found", []string{"show", "config", "999"}, -1, "", "operation 999 not found"},
		{"bad id", []string{"show", "history", "abc"}, 2, "goipsla: invalid operation id \"abc\": must be between 1 and 2147483647\nRun 'goipsla show history --help' for usage.\n", ""},
		{"missing id", []string{"show", "config"}, 2, "goipsla: missing operation id\nRun 'goipsla show config --help' for usage.\n", ""},
		{"two ids", []string{"show", "statistics", "1", "2"}, 2, "goipsla: too many arguments: 1 2\nRun 'goipsla show statistics --help' for usage.\n", ""},
		{"bad state", []string{"show", "operations", "--state", "up"}, 2, "goipsla: invalid --state \"up\": must be pending, inactive or active\nRun 'goipsla show operations --help' for usage.\n", ""},
		{"bad rc", []string{"show", "operations", "--rc", "fine"}, 2, "goipsla: invalid --rc: unknown return code \"fine\"\nRun 'goipsla show operations --help' for usage.\n", ""},
		{"unknown flag", []string{"show", "operations", "--verbose"}, 2, "goipsla: unknown flag: --verbose\nRun 'goipsla show operations --help' for usage.\n", ""},
		{"extra arg", []string{"show", "operations", "x"}, 2, "goipsla: unexpected argument \"x\"\nRun 'goipsla show operations --help' for usage.\n", ""},
		{"watch json", []string{"watch", "-o", "json"}, 2, "goipsla: watch does not support -o json; use show operations -o json\nRun 'goipsla watch --help' for usage.\n", ""},
		{"watch interval", []string{"watch", "--interval", "0s"}, 2, "goipsla: invalid --interval 0s: must be positive\nRun 'goipsla watch --help' for usage.\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errOut, err := runCtl(t, append([]string{"--socket", sock}, tt.args...)...)
			var ee *exitError
			switch tt.code {
			case 0:
				if err != nil {
					t.Fatalf("err %v", err)
				}
			case -1:
				if err == nil || errors.As(err, &ee) || err.Error() != tt.errMsg {
					t.Fatalf("err %v, want %q", err, tt.errMsg)
				}
			default:
				if !errors.As(err, &ee) || ee.Code != tt.code {
					t.Fatalf("err %v, want exit %d", err, tt.code)
				}
			}
			if errOut != tt.stderr {
				t.Errorf("stderr %q\nwant   %q", errOut, tt.stderr)
			}
		})
	}
}

func TestShowNoDaemon(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "gone.sock")
	_, _, err := runCtl(t, "--socket", sock, "show", "operations")
	want := "cannot connect to " + sock + ": no such file or directory (is goipslad running?)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
}

// TestShowAllInOneRequest: listing every operation's details, hour groups
// or history takes one request, not one per operation.
func TestShowAllInOneRequest(t *testing.T) {
	sock, requests := countingProxy(t, apitest.Serve(t, apitest.NewProvider()))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"show", "statistics", "--details"}, "GET /v1/operations?include=detail"},
		{[]string{"show", "statistics", "aggregated"}, "GET /v1/operations?include=hours"},
		{[]string{"show", "history"}, "GET /v1/operations?include=history"},
		{[]string{"show", "enhanced-history"}, "GET /v1/operations?include=enhanced"},
		{[]string{"show", "statistics", "aggregated", "101"}, "GET /v1/operations/101?include=hours"},
	} {
		before := len(requests())
		if _, errOut, err := runCtl(t, append([]string{"--socket", sock}, tc.args...)...); err != nil || errOut != "" {
			t.Fatalf("%v: err %v stderr %q", tc.args, err, errOut)
		}
		got := requests()[before:]
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%v: requests %q, want [%q]", tc.args, got, tc.want)
		}
	}
}
