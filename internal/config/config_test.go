//declscope:namespace parse

package config

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// parseTime is the reference time for start-time HH:MM in tests.
//
//declscope:package // shared test fixture: export_test.go parses with it too
var parseTime = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func mustParseAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

// parsedDefaultOp returns an operation with every default filled in.
func parsedDefaultOp(id int, typ OpType, target string) *Operation {
	return &Operation{
		ID:              id,
		Type:            typ,
		Target:          mustParseAddr(target),
		TargetName:      target,
		Frequency:       60 * time.Second,
		Timeout:         5000 * time.Millisecond,
		Threshold:       5000 * time.Millisecond,
		RequestDataSize: 28,
		DataPattern:     0xABCDABCD,
		Interval:        20 * time.Millisecond,
		NumPackets:      10,
		Stats:           StatsConfig{HoursKept: 2, DistBuckets: 1, DistInterval: 20 * time.Millisecond},
		History:         HistoryConfig{Lives: 0, Buckets: 15, Filter: FilterNone},
	}
}

func parseFile(t *testing.T, name string) (*Config, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return parse(data, parseTime)
}

// mustParse parses testdata/name or fails the test.
//
//declscope:package // shared test fixture: export_test.go reads the same files
func mustParse(t *testing.T, name string) *Config {
	t.Helper()
	cfg, err := parseFile(t, name)
	if err != nil {
		t.Fatalf("%s: unexpected error:\n%s", name, parseErrLines(err))
	}
	return cfg
}

func parseErrLines(err error) string {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var b strings.Builder
	for _, e := range ve.Errors {
		b.WriteString(e.Path + ": " + e.Msg + "\n")
	}
	return b.String()
}

func TestParsePlanExample(t *testing.T) {
	cfg := mustParse(t, "valid/plan-example.yaml")

	wanReact := []Reaction{
		{Element: "rtt", ThresholdType: "consecutive", Count: 3, X: 5, Y: 5, Upper: 300, Lower: 200, Action: "trap"},
		{Element: "timeout", ThresholdType: "immediate", Count: 5, X: 5, Y: 5, Action: "trap"},
	}
	wan := func(id int, target string) *Operation {
		op := parsedDefaultOp(id, ICMPEcho, target)
		op.SourceInterface = "eth1"
		op.VRF = "blue"
		op.Frequency = 10 * time.Second
		op.Timeout = 2000 * time.Millisecond
		op.Threshold = 300 * time.Millisecond
		op.Tag = "wan"
		op.Template = "wan-echo"
		op.React = wanReact
		if op.Target.Is4() {
			op.TOS = 0xB8
		} else {
			op.TrafficClass = 0xB8 // tos from a template carries over to IPv6
		}
		return op
	}
	jitter := parsedDefaultOp(1, ICMPJitter, "10.100.2.11")
	jitter.Frequency = 30 * time.Second

	want := &Config{
		Global: Global{
			APISocket:     "/run/goipslad/goipslad.sock",
			MetricsListen: "127.0.0.1:9818",
			Log:           LogConfig{Format: "text", Level: "info"},
			Syslog:        &SyslogConfig{Facility: "local0"},
			SNMP: &SNMPConfig{
				AgentX: "/var/agentx/master",
				Traps:  []TrapTarget{{Host: "192.0.2.10", Community: "public"}},
			},
		},
		Operations: []*Operation{
			jitter,
			wan(101, "10.100.1.11"),
			wan(102, "10.100.1.12"),
			wan(103, "fd00:100:2::11"),
		},
		Tracks: []Track{{ID: 1, Operation: 101, Mode: "reachability", DelayUp: 10 * time.Second, DelayDown: 5 * time.Second}},
		Actions: []Action{
			{On: []string{"track-down", "track-up"}, Track: 1, Webhook: "https://example.invalid/hook"},
			{On: []string{"threshold-exceeded"}, Exec: "/usr/local/libexec/ipsla-notify.sh"},
		},
	}
	if diff := cmp.Diff(want, cfg, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Errorf("config mismatch (-want +got):\n%s", diff)
	}
}

func TestParseDefaults(t *testing.T) {
	cfg := mustParse(t, "valid/defaults.yaml")
	want := &Config{
		Global:     Global{APISocket: defaultAPISocket, MetricsListen: defaultMetricsListen, Log: LogConfig{Format: "text", Level: "info"}},
		Operations: []*Operation{parsedDefaultOp(7, ICMPEcho, "192.0.2.1")},
	}
	if diff := cmp.Diff(want, cfg, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Errorf("config mismatch (-want +got):\n%s", diff)
	}
}

func TestAPISocketGroup(t *testing.T) {
	for in, want := range map[string]string{
		"global: { api-socket-group: goipsla }": "goipsla",
		"global: { api-socket-group: 1001 }":    "1001",
		"global: { api-socket-group: \"\" }":    "",
		"global: {}":                            "",
	} {
		cfg, err := parse([]byte(in), parseTime)
		if err != nil || cfg.Global.APISocketGroup != want {
			t.Errorf("%s: %q, %v", in, cfg.Global.APISocketGroup, err)
		}
	}
}

func TestParseEmpty(t *testing.T) {
	for _, in := range []string{"", "# only a comment\n", "---\n"} {
		cfg, err := parse([]byte(in), parseTime)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if len(cfg.Operations) != 0 || cfg.Global.APISocket != defaultAPISocket {
			t.Errorf("%q: got %+v", in, cfg)
		}
	}
}

func TestParseFull(t *testing.T) {
	cfg := mustParse(t, "valid/full.yaml")

	op5 := parsedDefaultOp(5, ICMPEcho, "2001:db8::5")
	op5.SourceIP = mustParseAddr("2001:db8::1")
	op5.TrafficClass = 0x20
	op5.FlowLabel = 0xFFFFF
	op5.Frequency = 30 * time.Second
	op5.Timeout = 1000 * time.Millisecond
	op5.Threshold = 200 * time.Millisecond
	op5.RequestDataSize = 100
	op5.DataPattern = 0x01020304
	op5.VerifyData = true
	op5.Tag = "v6 echo"
	op5.Owner = "noc"
	op5.Template = "base"
	op5.Stats = StatsConfig{HoursKept: 25, DistBuckets: 20, DistInterval: 10 * time.Millisecond}
	op5.History = HistoryConfig{Lives: 1, Buckets: 60, Filter: FilterFailures}
	op5.Enhanced = &EnhancedHistory{Interval: 60 * time.Second, Buckets: 10}
	op5.Schedule = &Schedule{Forever: true, Start: StartPending, Ageout: 7200 * time.Second}
	op5.React = []Reaction{
		{Element: "rtt", ThresholdType: "xofy", Count: 5, X: 3, Y: 10, Upper: 5000, Lower: 3000, Action: "trap-and-syslog"},
		{Element: "verifyError", ThresholdType: "consecutive", Count: 2, X: 5, Y: 5, Action: "syslog"},
	}

	op2 := parsedDefaultOp(2, ICMPJitter, "192.0.2.2")
	op2.SourceIP = mustParseAddr("192.0.2.254")
	op2.SourceInterface = "eth0"
	op2.VRF = "red"
	op2.TOS = 184
	op2.Frequency = 10 * time.Second
	op2.Timeout = 2000 * time.Millisecond
	op2.Threshold = 1000 * time.Millisecond
	op2.Interval = 50 * time.Millisecond
	op2.NumPackets = 100
	op2.OneWayDelay = true
	op2.Schedule = &Schedule{Life: 12 * time.Hour, Start: StartAfter, After: 10 * time.Minute}
	op2.React = []Reaction{
		{Element: "jitterAvg", ThresholdType: "average", Count: 16, X: 5, Y: 5, Upper: 30, Lower: 10, Action: "none"},
		{Element: "packetLoss", ThresholdType: "immediate", Count: 5, X: 5, Y: 5, Upper: 10000, Lower: 10000, Action: "none"},
		{Element: "maxOfLatencySD", ThresholdType: "never", Count: 5, X: 5, Y: 5, Upper: 5000, Lower: 3000, Action: "none"},
	}

	want := &Config{
		Global:     Global{APISocket: "/tmp/goipslad.sock", MetricsListen: "", Log: LogConfig{Format: "json", Level: "debug"}},
		Operations: []*Operation{op2, op5},
		Tracks: []Track{
			{ID: 10, Operation: 2, Mode: "state", DelayDown: 180 * time.Second},
			{ID: 20, Operation: 5, Mode: "state"},
		},
		Actions: []Action{
			{On: []string{"threshold-exceeded", "threshold-cleared"}, Exec: "/bin/true"},
			{On: []string{"track-up"}, Track: 20, Webhook: "http://127.0.0.1:8080/hook", Exec: "/usr/bin/logger"},
		},
		Warnings: []string{
			"operations(id=5).react[0].action: sends to syslog, but global.syslog is not set: no syslog message is sent (2 reactions)",
			"operations(id=5).react[0].action: sends a trap, but global.snmp.traps is empty: no trap is sent (1 reaction)",
		},
	}
	if diff := cmp.Diff(want, cfg, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Errorf("config mismatch (-want +got):\n%s", diff)
	}
}

// TestParseValid checks individual effective values of small valid inputs.
func TestParseValid(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		check func(t *testing.T, op *Operation)
	}{
		{
			name: "form A with template, explicit keys override",
			yaml: `
templates:
  wan-echo: { type: icmp-echo, frequency: 10s, timeout: 2000ms, threshold: 300ms, tag: wan }
operations:
  - { id: 5, template: wan-echo, target: 10.100.1.13, timeout: 1000ms }
`,
			check: func(t *testing.T, op *Operation) {
				if op.Timeout != time.Second || op.Threshold != 300*time.Millisecond || op.Frequency != 10*time.Second || op.Tag != "wan" || op.Template != "wan-echo" {
					t.Errorf("got %+v", op)
				}
			},
		},
		{
			name: "timeout and threshold defaults shrink to fit frequency",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, frequency: 2s } ]`,
			check: func(t *testing.T, op *Operation) {
				if op.Timeout != 2*time.Second || op.Threshold != 2*time.Second {
					t.Errorf("timeout %v threshold %v", op.Timeout, op.Threshold)
				}
			},
		},
		{
			name: "threshold default shrinks to fit timeout",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, timeout: 800ms } ]`,
			check: func(t *testing.T, op *Operation) {
				if op.Threshold != 800*time.Millisecond {
					t.Errorf("threshold %v", op.Threshold)
				}
			},
		},
		{
			name: "jitter timeout default leaves room for the packet train",
			yaml: `operations: [ { id: 1, type: icmp-jitter, target: 192.0.2.1, frequency: 3s, interval: 100ms, num-packets: 10 } ]`,
			check: func(t *testing.T, op *Operation) {
				if op.Timeout != 2*time.Second || op.Threshold != 2*time.Second {
					t.Errorf("timeout %v threshold %v", op.Timeout, op.Threshold)
				}
			},
		},
		{
			name: "boundaries: threshold == timeout == frequency",
			yaml: `operations: [ { id: 2147483647, type: icmp-echo, target: 192.0.2.1, frequency: 1s, timeout: 1000ms, threshold: 1000ms, request-data-size: 16384 } ]`,
			check: func(t *testing.T, op *Operation) {
				if op.ID != 2147483647 || op.RequestDataSize != 16384 {
					t.Errorf("got %+v", op)
				}
			},
		},
		{
			name: "timeout lower boundary 1ms with threshold 0ms",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, timeout: 1ms, threshold: 0ms } ]`,
			check: func(t *testing.T, op *Operation) {
				if op.Timeout != time.Millisecond || op.Threshold != 0 {
					t.Errorf("timeout %v threshold %v", op.Timeout, op.Threshold)
				}
			},
		},
		{
			name: "IPv4-mapped IPv6 target becomes IPv4",
			yaml: `operations: [ { id: 1, type: icmp-jitter, target: "::ffff:192.0.2.1" } ]`,
			check: func(t *testing.T, op *Operation) {
				if !op.Target.Is4() || op.TargetName != "::ffff:192.0.2.1" {
					t.Errorf("target %v name %q", op.Target, op.TargetName)
				}
			},
		},
		{
			name: "traffic-class from template carries over to IPv4 tos; flow-label ignored",
			yaml: `
templates: { t: { type: icmp-echo, traffic-class: 0x28, flow-label: 5 } }
operations: [ { template: t, targets: { 1: 192.0.2.1, 2: "2001:db8::2" } } ]
`,
			check: func(t *testing.T, op *Operation) {
				switch op.ID {
				case 1:
					if op.TOS != 0x28 || op.FlowLabel != 0 {
						t.Errorf("v4: %+v", op)
					}
				case 2:
					if op.TrafficClass != 0x28 || op.FlowLabel != 5 || op.TOS != 0 {
						t.Errorf("v6: %+v", op)
					}
				}
			},
		},
		{
			name: "react on the operation replaces the template's list",
			yaml: `
templates: { t: { type: icmp-echo, react: [ { element: rtt } ] } }
operations: [ { id: 1, template: t, target: 192.0.2.1, react: [ { element: timeout } ] }, { id: 2, template: t, target: 192.0.2.2, react: } ]
`,
			check: func(t *testing.T, op *Operation) {
				want := map[int]int{1: 1, 2: 0}[op.ID]
				if len(op.React) != want || (want == 1 && op.React[0].Element != "timeout") {
					t.Errorf("op %d react %+v", op.ID, op.React)
				}
			},
		},
		{
			name: "react defaults",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, react: [ { element: rtt } ] } ]`,
			check: func(t *testing.T, op *Operation) {
				want := []Reaction{{Element: "rtt", ThresholdType: "never", Count: 5, X: 5, Y: 5, Upper: 5000, Lower: 3000, Action: "none"}}
				if diff := cmp.Diff(want, op.React); diff != "" {
					t.Error(diff)
				}
			},
		},
		{
			name: "one-way-delay from a template, overridden by the operation",
			yaml: `
templates: { j: { type: icmp-jitter, one-way-delay: true, react: [ { element: latencySDAvg } ] } }
operations: [ { template: j, targets: { 1: 192.0.2.1 } }, { id: 2, template: j, target: 192.0.2.2, one-way-delay: false, react: [] } ]
`,
			check: func(t *testing.T, op *Operation) {
				if want := op.ID == 1; op.OneWayDelay != want {
					t.Errorf("op %d one-way-delay %v", op.ID, op.OneWayDelay)
				}
			},
		},
		{
			name: "enhanced {} enables with defaults",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, history: { enhanced: {} } } ]`,
			check: func(t *testing.T, op *Operation) {
				if op.Enhanced == nil || *op.Enhanced != (EnhancedHistory{Interval: 900 * time.Second, Buckets: 100}) {
					t.Errorf("enhanced %+v", op.Enhanced)
				}
			},
		},
		{
			// schedule: {} is the same as no schedule (nil), so that adding or
			// removing it does not look like a schedule change on reload.
			name: "schedule {} means no schedule",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, schedule: {} } ]`,
			check: func(t *testing.T, op *Operation) {
				if op.Schedule != nil {
					t.Errorf("schedule = %+v, want nil", op.Schedule)
				}
			},
		},
		{
			name: "start-time HH:MM later today",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, schedule: { start-time: "13:30", life: 1h, recurring: true } } ]`,
			check: func(t *testing.T, op *Operation) {
				want := &Schedule{Life: time.Hour, Start: StartAt, At: time.Date(2026, 9, 27, 13, 30, 0, 0, time.UTC), Recurring: true, Daily: true}
				if diff := cmp.Diff(want, op.Schedule); diff != "" {
					t.Error(diff)
				}
			},
		},
		{
			name: "start-time HH:MM:SS already passed today means tomorrow",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, schedule: { start-time: "12:00:00", life: 23h, ageout: 2h, recurring: true } } ]`,
			check: func(t *testing.T, op *Operation) {
				if want := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC); !op.Schedule.At.Equal(want) || !op.Schedule.Daily {
					t.Errorf("at %v, want %v", op.Schedule.At, want)
				}
			},
		},
		{
			name: "start-time RFC 3339 and after duration",
			yaml: `
operations:
  - { id: 1, type: icmp-echo, target: 192.0.2.1, schedule: { start-time: "2026-10-01T09:00:00+09:00" } }
  - { id: 2, type: icmp-echo, target: 192.0.2.2, schedule: { start-time: after 90s } }
`,
			check: func(t *testing.T, op *Operation) {
				s := op.Schedule
				switch op.ID {
				case 1:
					if s.Start != StartAt || !s.At.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || s.Daily {
						t.Errorf("%+v", s)
					}
				case 2:
					if s.Start != StartAfter || s.After != 90*time.Second || s.Daily {
						t.Errorf("%+v", s)
					}
				}
			},
		},
		{
			name: "schedule merges per key; the operation's life wins",
			yaml: `
templates: { t: { type: icmp-echo, schedule: { life: forever, start-time: pending } } }
operations: [ { id: 1, template: t, target: 192.0.2.1, schedule: { life: 60s } } ]
`,
			check: func(t *testing.T, op *Operation) {
				if diff := cmp.Diff(&Schedule{Life: time.Minute, Start: StartPending}, op.Schedule); diff != "" {
					t.Error(diff)
				}
			},
		},
		{
			name: "aliases are followed",
			yaml: `
templates: { t: { type: icmp-echo, react: &r [ { element: rtt } ] } }
operations: [ { id: 1, template: t, target: 192.0.2.1, react: *r } ]
`,
			check: func(t *testing.T, op *Operation) {
				if len(op.React) != 1 {
					t.Errorf("react %+v", op.React)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parse([]byte(tt.yaml), parseTime)
			if err != nil {
				t.Fatalf("unexpected error:\n%s", parseErrLines(err))
			}
			if len(cfg.Operations) == 0 {
				t.Fatal("no operations")
			}
			for _, op := range cfg.Operations {
				tt.check(t, op)
			}
		})
	}
}

type parseErr = FieldError

// TestParseInvalid checks the exact list of errors, in order.
func TestParseInvalid(t *testing.T) {
	const echo = "type: icmp-echo, target: 192.0.2.1"
	tests := []struct {
		name string
		yaml string
		file string // under testdata/invalid, instead of yaml
		want []FieldError
	}{
		{
			name: "file with many errors",
			file: "many-errors.yaml",
			want: []parseErr{
				{"global.api-sockt", "unknown key"},
				{"global.log.format", "must be one of text, json"},
				{"templates.t.id", "not allowed in a template"},
				{"templates.t.frequency", "duration needs a unit such as 60s or 5000ms"},
				{"operations[0].target", "hostnames are not supported yet"},
				{"tracks[0].mode", "must be one of state, reachability"},
				{"actions[0].on[0]", "must be one of threshold-exceeded, threshold-cleared, track-up, track-down"},
				{"actions[0]", "webhook or exec is required"},
				{"operations(id=1).id", "duplicate id 1 (first defined at operations[0])"},
				{"operations[2].template", `unknown template "missing"`},
				{"operations[3].target", "cannot be used together with targets"},
				{"operations[3].id", "cannot be used together with targets; the keys of targets are the ids"},
				{"tracks[0].operation", "operation 99 is not defined"},
				{"actions[0].track", "track 2 is not defined"},
			},
		},
		{
			name: "yaml syntax error",
			file: "syntax.yaml",
			want: []parseErr{{"", "yaml: line 1: did not find expected '-' indicator"}},
		},
		{
			name: "multiple documents",
			yaml: "operations: []\n---\noperations: []\n",
			want: []parseErr{{"", "multiple YAML documents are not supported"}},
		},
		{
			name: "root must be a mapping",
			yaml: "- 1\n",
			want: []parseErr{{"", "the file must be a mapping (global, templates, operations, tracks, actions)"}},
		},
		{
			name: "unknown top-level and operation keys, duplicate key",
			yaml: "operation: []\noperations: [ { id: 1, " + echo + ", frequncy: 10s, id: 2 } ]\n",
			want: []parseErr{
				{"operation", "unknown key"},
				{"operations[0].frequncy", "unknown key"},
				{"operations[0].id", "duplicate key"},
			},
		},
		{
			name: "required keys",
			yaml: "operations: [ { target: 192.0.2.1 }, { id: 2 }, { id: 3, target: 192.0.2.3 } ]\n",
			want: []parseErr{
				{"operations[0].id", "required"},
				{"operations[1].target", "required"},
				{"operations(id=3).type", "required"},
			},
		},
		{
			name: "types of values",
			yaml: `operations: [ { id: one, type: icmp-ping, target: [1], tos: high, verify-data: yes, frequency: 10, timeout: "500", threshold: 1.5, interval: fast, tag: { a: 1 }, history: [] } ]`,
			want: []parseErr{
				{"operations[0].id", "must be an integer"},
				{"operations[0].type", "must be one of icmp-echo, icmp-jitter"},
				{"operations[0].target", "must be a single value"},
				{"operations[0].tos", "must be an integer"},
				{"operations[0].verify-data", "must be true or false"},
				{"operations[0].frequency", "duration needs a unit such as 60s or 5000ms"},
				{"operations[0].timeout", "duration needs a unit such as 60s or 5000ms"},
				{"operations[0].threshold", "duration needs a unit such as 60s or 5000ms"},
				{"operations[0].interval", "must be a duration such as 60s or 5000ms"},
				{"operations[0].tag", "must be a single value"},
				{"operations[0].history", "must be a mapping"},
			},
		},
		{
			name: "missing values",
			yaml: "operations: [ { id: 1, type: icmp-echo, target: } ]\n",
			want: []parseErr{{"operations[0].target", "missing value"}},
		},
		{
			name: "operation ranges",
			yaml: `operations: [ { id: 0, type: icmp-echo, target: 192.0.2.1, frequency: 604801s, timeout: 604800001ms, threshold: 60001ms, request-data-size: 27, data-pattern: 0x100000000, tos: 256, tag: "` + strings.Repeat("x", 129) + `", owner: "` + strings.Repeat("y", 256) + `" } ]`,
			want: []parseErr{
				{"operations[0].id", "must be between 1 and 2147483647"},
				{"operations[0].frequency", "must be between 1s and 604800s"},
				{"operations[0].timeout", "must be between 1ms and 604800000ms"},
				{"operations[0].threshold", "must be between 0ms and 60000ms"},
				{"operations[0].request-data-size", "must be between 28 and 16384"},
				{"operations[0].data-pattern", "must be between 0 and 4294967295"},
				{"operations[0].tos", "must be between 0 and 255"},
				{"operations[0].tag", "must be at most 128 characters"},
				{"operations[0].owner", "must be at most 255 characters"},
			},
		},
		{
			name: "more ranges",
			yaml: `operations: [ { id: 1, type: icmp-jitter, target: 192.0.2.1, frequency: 0s, interval: 0ms, num-packets: 1001, request-data-size: 16385, history: { lives-kept: 3, buckets-kept: 0, hours-of-statistics-kept: 26, distributions-of-statistics-kept: 21, statistics-distribution-interval: 101ms, filter: some, enhanced: { interval: 3601s, buckets: 101 } } } ]`,
			want: []parseErr{
				{"operations[0].frequency", "must be between 1s and 604800s"},
				{"operations[0].interval", "must be between 1ms and 60000ms"},
				{"operations[0].num-packets", "must be between 1 and 1000"},
				{"operations[0].request-data-size", "must be between 28 and 16384"},
				{"operations[0].history.lives-kept", "must be between 0 and 2"},
				{"operations[0].history.buckets-kept", "must be between 1 and 60"},
				{"operations[0].history.hours-of-statistics-kept", "must be between 0 and 25"},
				{"operations[0].history.distributions-of-statistics-kept", "must be between 1 and 20"},
				{"operations[0].history.statistics-distribution-interval", "must be between 1ms and 100ms"},
				{"operations[0].history.filter", "must be one of none, all, overThreshold, failures"},
				{"operations[0].history.enhanced.interval", "must be between 1s and 3600s"},
				{"operations[0].history.enhanced.buckets", "must be between 1 and 100"},
			},
		},
		{
			name: "control characters in tag and owner",
			yaml: `operations: [ { id: 1, ` + echo + `, tag: "wan\nX-Forged: 1", owner: "noc\tteam" }, { id: 2, ` + echo + `, tag: "a\u007fb" }, { id: 3, ` + echo + `, tag: "日本語 ok" } ]`,
			want: []parseErr{
				{"operations[0].tag", "must not contain control characters (found U+000A)"},
				{"operations[0].owner", "must not contain control characters (found U+0009)"},
				{"operations[1].tag", "must not contain control characters (found U+007F)"},
			},
		},
		{
			name: "timeout lower bound is 1ms",
			yaml: `operations: [ { id: 1, ` + echo + `, timeout: 0ms, threshold: 0ms }, { id: 2, ` + echo + `, timeout: -5ms }, { id: 3, ` + echo + `, timeout: 0s } ]`,
			want: []parseErr{
				{"operations[0].timeout", "must be at least 1ms"},
				{"operations[1].timeout", "must be at least 1ms"},
				{"operations[2].timeout", "must be at least 1ms"},
			},
		},
		{
			name: "timeout 0 in a template",
			yaml: "templates: { t: { type: icmp-echo, timeout: 0ms } }\noperations: [ { template: t, targets: { 1: 192.0.2.1 } } ]\n",
			want: []parseErr{{"templates.t.timeout", "must be at least 1ms"}},
		},
		{
			name: "IPv6 ranges and granularity",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: "2001:db8::1", traffic-class: 256, flow-label: 1048576, frequency: 1500ms, timeout: 1500us } ]`,
			want: []parseErr{
				{"operations[0].traffic-class", "must be between 0 and 255"},
				{"operations[0].flow-label", "must be between 0 and 1048575"},
				{"operations[0].frequency", "must be a whole number of seconds"},
				{"operations[0].timeout", "must be a whole number of milliseconds"},
			},
		},
		{
			name: "addresses",
			yaml: `operations: [ { id: 1, type: icmp-echo, target: 999.1.1.1 }, { id: 2, type: icmp-echo, target: "fe80::1%eth0" }, { id: 3, type: icmp-echo, target: 0.0.0.0 }, { id: 4, type: icmp-echo, target: localhost, source-ip: 10.0.0.300 } ]`,
			want: []parseErr{
				{"operations[0].target", "must be an IP address"},
				{"operations[1].target", "zoned addresses are not supported; use source-interface"},
				{"operations[2].target", "must not be the unspecified address"},
				{"operations[3].target", "hostnames are not supported yet"},
				{"operations[3].source-ip", "must be an IP address"},
			},
		},
		{
			name: "threshold <= timeout <= frequency",
			yaml: `operations: [ { id: 1, ` + echo + `, timeout: 1000ms, threshold: 1001ms }, { id: 2, ` + echo + `, frequency: 5s, timeout: 5001ms, threshold: 0ms } ]`,
			want: []parseErr{
				{"operations(id=1).threshold", "must not exceed timeout (1s)"},
				{"operations(id=2).timeout", "must not exceed frequency (5s)"},
			},
		},
		{
			name: "jitter timeout + interval * num-packets <= frequency",
			yaml: `operations: [ { id: 1, type: icmp-jitter, target: 192.0.2.1, frequency: 5s, timeout: 4900ms, interval: 20ms, num-packets: 10 } ]`,
			want: []parseErr{{"operations(id=1).frequency", "must be at least timeout + interval * num-packets (5.1s) for icmp-jitter"}},
		},
		{
			name: "jitter packet train alone exceeds frequency",
			yaml: `operations: [ { id: 1, type: icmp-jitter, target: 192.0.2.1, frequency: 1s, interval: 200ms, num-packets: 10 } ]`,
			want: []parseErr{{"operations(id=1).frequency", "must be at least timeout + interval * num-packets (2.001s) for icmp-jitter"}},
		},
		{
			name: "keys not applicable to the type",
			yaml: `operations: [ { id: 1, type: icmp-jitter, target: 192.0.2.1, request-data-size: 64, data-pattern: 0, verify-data: true }, { id: 2, ` + echo + `, interval: 20ms, num-packets: 5 } ]`,
			want: []parseErr{
				{"operations(id=1).request-data-size", "not applicable to icmp-jitter"},
				{"operations(id=1).data-pattern", "not applicable to icmp-jitter"},
				{"operations(id=1).verify-data", "not applicable to icmp-jitter"},
				{"operations(id=2).interval", "not applicable to icmp-echo"},
				{"operations(id=2).num-packets", "not applicable to icmp-echo"},
			},
		},
		{
			name: "not applicable via template",
			yaml: "templates: { j: { type: icmp-jitter, verify-data: false } }\noperations: [ { template: j, targets: { 8: 192.0.2.8 } } ]\n",
			want: []parseErr{{"operations(id=8).verify-data", "not applicable to icmp-jitter"}},
		},
		{
			name: "address families",
			yaml: `operations:
  - { id: 1, type: icmp-jitter, target: "2001:db8::1" }
  - { id: 2, type: icmp-echo, target: "2001:db8::2", tos: 184, source-ip: 192.0.2.1 }
  - { id: 3, type: icmp-echo, target: 192.0.2.3, traffic-class: 1, flow-label: 2, source-ip: "2001:db8::1" }`,
			want: []parseErr{
				{"operations(id=1).target", "icmp-jitter supports IPv4 targets only"},
				{"operations(id=2).source-ip", "must be the same address family as target"},
				{"operations(id=2).tos", "applies to IPv4 targets only; use traffic-class"},
				{"operations(id=3).source-ip", "must be the same address family as target"},
				{"operations(id=3).traffic-class", "applies to IPv6 targets only; use tos"},
				{"operations(id=3).flow-label", "applies to IPv6 targets only"},
			},
		},
		{
			name: "template misuse",
			yaml: `templates:
  t: { type: icmp-echo, target: 192.0.2.1, targets: { 1: 192.0.2.1 }, template: u }
operations:
  - { targets: { 1: 192.0.2.1 } }
  - { template: t, targets: {} }
  - { template: t, targets: [ 192.0.2.1 ] }
  - { template: t, targets: { 0: 192.0.2.1, x: 192.0.2.2, 3: 192.0.2.3, 3: 192.0.2.4, 4: 1.2.3 } }`,
			want: []parseErr{
				{"templates.t.target", "not allowed in a template"},
				{"templates.t.targets", "not allowed in a template"},
				{"templates.t.template", "not allowed in a template"},
				{"operations[1].targets", "must not be empty"},
				{"operations[2].targets", "must be a mapping of id to target address"},
				{"operations[3].targets.0", "must be between 1 and 2147483647"},
				{"operations[3].targets.x", "must be an integer"},
				{"operations[3].targets.3", "duplicate key"},
				{"operations[3].targets.4", "must be an IP address"},
				{"operations[0].template", "required when targets is used"},
				{"operations(id=1).type", "required"},
			},
		},
		{
			name: "duplicate id across targets",
			yaml: "templates: { t: { type: icmp-echo } }\noperations: [ { id: 5, " + echo + " }, { template: t, targets: { 6: 192.0.2.6, 5: 192.0.2.5 } } ]\n",
			want: []parseErr{{"operations(id=5).id", "duplicate id 5 (first defined at operations[0])"}},
		},
		{
			name: "schedule",
			yaml: `operations:
  - { id: 1, ` + echo + `, schedule: { life: 0s, start-time: tomorrow, ageout: 2073601s, recurring: 1, every: 1s } }
  - { id: 2, ` + echo + `, schedule: { life: forever, start-time: now, recurring: true } }
  - { id: 3, ` + echo + `, schedule: { life: 24h, start-time: "01:00", recurring: true } }
  - { id: 4, ` + echo + `, schedule: { life: 12h, start-time: "01:00", ageout: 12h, recurring: true } }
  - { id: 5, ` + echo + `, schedule: { start-time: "24:00" } }
  - { id: 6, ` + echo + `, schedule: { start-time: "after 5" } }
  - { id: 7, ` + echo + `, schedule: { life: 1.5s } }`,
			want: []parseErr{
				{"operations[0].schedule.life", "must be between 1s and 2147483647s"},
				{"operations[0].schedule.start-time", startParseError},
				{"operations[0].schedule.ageout", "must be between 0s and 2073600s"},
				{"operations[0].schedule.recurring", "must be true or false"},
				{"operations[0].schedule.every", "unknown key"},
				{"operations[4].schedule.start-time", startParseError},
				{"operations[5].schedule.start-time", "after duration needs a unit such as 60s or 5000ms"},
				{"operations[6].schedule.life", "must be a whole number of seconds"},
				{"operations(id=2).schedule.recurring", "requires start-time in HH:MM[:SS] form"},
				{"operations(id=2).schedule.recurring", "requires life shorter than 24h"},
				{"operations(id=3).schedule.recurring", "requires life shorter than 24h"},
				{"operations(id=4).schedule.recurring", "requires ageout 0s or life + ageout longer than 24h"},
			},
		},
		{
			name: "react syntax and ranges",
			yaml: `operations: [ { id: 1, ` + echo + `, react: [ { threshold-type: sometimes, count: 0, x: 17, y: 0, upper: -1, lower: 2147483648, action: page, when: now } ] } ]`,
			want: []parseErr{
				{"operations[0].react[0].threshold-type", "must be one of never, immediate, consecutive, xofy, average"},
				{"operations[0].react[0].count", "must be between 1 and 16"},
				{"operations[0].react[0].x", "must be between 1 and 16"},
				{"operations[0].react[0].y", "must be between 1 and 16"},
				{"operations[0].react[0].upper", "must be between 0 and 2147483647"},
				{"operations[0].react[0].lower", "must be between 0 and 2147483647"},
				{"operations[0].react[0].action", "must be one of none, syslog, trap, trap-and-syslog"},
				{"operations[0].react[0].when", "unknown key"},
				{"operations[0].react[0].element", "required"},
			},
		},
		{
			name: "react semantics",
			yaml: `operations:
  - id: 1
    type: icmp-echo
    target: 192.0.2.1
    react:
      - { element: jitterAvg }
      - { element: timeout, upper: 1, lower: 0, threshold-type: average }
      - { element: rtt, threshold-type: immediate, count: 3, y: 2 }
      - { element: rtt, threshold-type: xofy, x: 6, y: 5 }
      - { element: verifyError, threshold-type: consecutive }
  - id: 2
    type: icmp-jitter
    target: 192.0.2.2
    react:
      - { element: verifyError }
      - { element: jitterAvg, upper: 10, lower: 20 }`,
			want: []parseErr{
				{"operations(id=1).react[0].element", `unknown element "jitterAvg" for icmp-echo`},
				{"operations(id=1).react[1].upper", "not applicable to element timeout"},
				{"operations(id=1).react[1].lower", "not applicable to element timeout"},
				{"operations(id=1).react[1].threshold-type", "average is not available for element timeout"},
				{"operations(id=1).react[2].count", "only applies to threshold-type consecutive or average"},
				{"operations(id=1).react[2].y", "only applies to threshold-type xofy"},
				{"operations(id=1).react[2].x", "must not exceed y (2)"},
				{"operations(id=1).react[3].element", `duplicate element "rtt"`},
				{"operations(id=1).react[3].x", "must not exceed y (5)"},
				{"operations(id=2).react[0].element", `unknown element "verifyError" for icmp-jitter`},
				{"operations(id=2).react[1].lower", "must not exceed upper (10)"},
			},
		},
		{
			name: "P5 element set of icmp-jitter",
			yaml: `operations:
  - id: 1
    type: icmp-jitter
    target: 192.0.2.1
    react:
      - { element: packetLossSD }
      - { element: packetMIA }
      - { element: maxOfLatencySD }
      - { element: latencyDSAvg }
      - { element: packetLoss, upper: 3, lower: 1 }
  - id: 2
    type: icmp-echo
    target: 192.0.2.2
    one-way-delay: true
    react:
      - { element: packetLoss }`,
			want: []parseErr{
				{"operations(id=1).react[0].element", `unknown element "packetLossSD" for icmp-jitter`},
				{"operations(id=1).react[1].element", `unknown element "packetMIA" for icmp-jitter`},
				{"operations(id=1).react[2].element", `element "maxOfLatencySD" needs one-way-delay: true`},
				{"operations(id=1).react[3].element", `element "latencyDSAvg" needs one-way-delay: true`},
				{"operations(id=2).one-way-delay", "not applicable to icmp-echo"},
				{"operations(id=2).react[0].element", `unknown element "packetLoss" for icmp-echo`},
			},
		},
		{
			name: "actions accept the event kinds only",
			yaml: "operations: [ { id: 1, " + echo + " } ]\nactions: [ { on: [timeout, timeout-cleared], exec: /bin/true } ]\n",
			want: []parseErr{
				{"actions[0].on[0]", "must be one of threshold-exceeded, threshold-cleared, track-up, track-down"},
				{"actions[0].on[1]", "must be one of threshold-exceeded, threshold-cleared, track-up, track-down"},
			},
		},
		{
			name: "global",
			yaml: `global:
  api-socket: relative.sock
  metrics-listen: 9818
  log: { level: trace, verbose: true }
  syslog: { facility: local8 }
  snmp: { agentx: "", traps: [ { host: 192.0.2.1 }, { community: c, port: 162 }, { host: h, community: c, version: v3 } ], version: 3 }`,
			want: []parseErr{
				{"global.api-socket", "must be an absolute path"},
				{"global.metrics-listen", "must be host:port"},
				{"global.log.level", "must be one of debug, info, warn, error"},
				{"global.log.verbose", "unknown key"},
				{"global.syslog.facility", "must be one of " + strings.Join(syslogFacilities, ", ")},
				{"global.snmp.agentx", "must not be empty"},
				{"global.snmp.traps[0].community", "required"},
				{"global.snmp.traps[1].port", "unknown key"},
				{"global.snmp.traps[1].host", "required"},
				{"global.snmp.traps[2].version", "must be one of v2c"},
				{"global.snmp.version", "unknown key"},
			},
		},
		{
			name: "api-socket-group",
			yaml: "global: { api-socket-group: \"bad group\" }",
			want: []parseErr{{"global.api-socket-group", "must be a group name or a numeric gid"}},
		},
		{
			name: "metrics-listen port",
			yaml: `global: { metrics-listen: "127.0.0.1:70000" }`,
			want: []parseErr{{"global.metrics-listen", "port must be between 1 and 65535"}},
		},
		{
			name: "tracks and actions",
			yaml: `operations: [ { id: 1, ` + echo + ` } ]
tracks:
  - { id: 1, operation: 1, delay: { up: 181s, down: -1s, sideways: 1s } }
  - { id: 1, operation: 1 }
  - { mode: state }
actions:
  - { on: [], webhook: "ftp://example.invalid/x" }
  - { on: [track-up, track-up], exec: bin/notify }
  - { webhook: "https://example.invalid/", track: 0 }`,
			want: []parseErr{
				{"tracks[0].delay.up", "must be between 0s and 180s"},
				{"tracks[0].delay.down", "must be between 0s and 180s"},
				{"tracks[0].delay.sideways", "unknown key"},
				{"tracks[2].id", "required"},
				{"tracks[2].operation", "required"},
				{"actions[0].on", "must list at least one event"},
				{"actions[0].webhook", "must be an http or https URL"},
				{"actions[1].on[1]", "duplicate event track-up"},
				{"actions[1].exec", "must be an absolute path"},
				{"actions[2].track", "must be between 1 and 2147483647"},
				{"actions[2].on", "required"},
				{"tracks[1].id", "duplicate id 1"},
			},
		},
		{
			// Addresses of the global section are checked at load time, with
			// the parsers the daemon uses (audit B3, B7).
			name: "global addresses",
			yaml: "global:\n" +
				"  api-socket: /" + strings.Repeat("s", 107) + "\n" +
				"  metrics-listen: 'foo bar:9818'\n" +
				"  snmp:\n" +
				"    agentx: 'foo bar'\n" +
				"    traps:\n" +
				"      - { host: '192.0.2.1:99999', community: c }\n" +
				"      - { host: 'bad host', community: c }\n" +
				"      - { host: '[2001:db8::1]', community: c }\n",
			want: []parseErr{
				{"global.api-socket", "must be at most 107 bytes (the limit of a Unix socket path)"},
				{"global.metrics-listen", "host must be an IP address or a host name"},
				{"global.snmp.agentx", "must be /path, unix:/path, tcp:host:port or host:port"},
				{"global.snmp.traps[0].host", "port must be between 1 and 65535"},
				{"global.snmp.traps[1].host", "must be host, host:port or [ipv6]:port with an IP address or a host name"},
			},
		},
		{
			// Interface names follow Linux's IFNAMSIZ (audit B6).
			name: "source-interface and vrf",
			yaml: "operations:\n" +
				"  - { id: 1, " + echo + ", source-interface: \"eth0\\n\" }\n" +
				"  - { id: 2, " + echo + ", vrf: abcdefghijklmnop }\n" +
				"  - { id: 3, " + echo + ", vrf: 'a/b' }\n" +
				"  - { id: 4, " + echo + ", source-interface: eth0, vrf: blue }\n",
			want: []parseErr{
				{"operations[0].source-interface", `must not contain "/", white space or control characters`},
				{"operations[1].vrf", "must be at most 15 bytes (a Linux interface name)"},
				{"operations[2].vrf", `must not contain "/", white space or control characters`},
			},
		},
		{
			// Cisco's threshold-value takes both values (audit B4).
			name: "upper without lower",
			yaml: "operations:\n" +
				"  - { id: 1, " + echo + ", react: [ { element: rtt, threshold-type: immediate, upper: 1000 } ] }\n" +
				"  - { id: 2, " + echo + ", react: [ { element: rtt, threshold-type: immediate, lower: 10 } ] }\n",
			want: []parseErr{
				{"operations(id=1).react[0].upper", "upper and lower must be given together"},
				{"operations(id=2).react[0].lower", "upper and lower must be given together"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.file != "" {
				_, err = parseFile(t, filepath.Join("invalid", tt.file))
			} else {
				_, err = parse([]byte(tt.yaml), parseTime)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("got error %v, want *ValidationError", err)
			}
			if diff := cmp.Diff(tt.want, ve.Errors); diff != "" {
				t.Errorf("errors mismatch (-want +got):\n%s\ngot:\n%s", diff, parseErrLines(err))
			}
		})
	}
}

func TestTooManyOperations(t *testing.T) {
	var b strings.Builder
	b.WriteString("templates: { t: { type: icmp-echo } }\noperations:\n  - template: t\n    targets:\n")
	for i := 1; i <= operationsLimit+1; i++ {
		b.WriteString("      " + strconv.Itoa(i) + ": 192.0.2.1\n")
	}
	_, err := parse([]byte(b.String()), parseTime)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %v", err)
	}
	want := []parseErr{{"operations", "at most 1000 operations are allowed, got 1001"}}
	if diff := cmp.Diff(want, ve.Errors); diff != "" {
		t.Error(diff)
	}

	// Exactly the limit is fine.
	s := strings.Replace(b.String(), "      1001: 192.0.2.1\n", "", 1)
	cfg, err := parse([]byte(s), parseTime)
	if err != nil || len(cfg.Operations) != operationsLimit {
		t.Fatalf("limit: %v", err)
	}
	for i, op := range cfg.Operations {
		if op.ID != i+1 {
			t.Fatalf("operations not sorted: index %d has id %d", i, op.ID)
		}
	}
}

func TestLoad(t *testing.T) {
	cfg, err := Load("testdata/valid/defaults.yaml")
	if err != nil || len(cfg.Operations) != 1 {
		t.Fatalf("Load: %v", err)
	}
	_, err = Load("testdata/does-not-exist.yaml")
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load missing file: got %v", err)
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatal("read errors must not be validation errors")
	}
}

func TestValidationErrorMessage(t *testing.T) {
	_, err := parse([]byte("operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, timeout: 2000ms, threshold: 3000ms, frequency: 1s } ]"), parseTime)
	want := "invalid configuration: 2 errors: operations(id=1).threshold: must not exceed timeout (2s); operations(id=1).timeout: must not exceed frequency (1s)"
	if err == nil || err.Error() != want {
		t.Errorf("got %q\nwant %q", err, want)
	}
}

func FuzzParse(f *testing.F) {
	for _, dir := range []string{"testdata/valid", "testdata/invalid"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		for _, name := range files {
			data, err := os.ReadFile(name)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(data)
		}
	}
	f.Add([]byte("operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, react: &a [ *a ] } ]"))
	f.Add([]byte("a: &a [*a, *a]\n"))
	f.Add([]byte("operations: [ { template: t, targets: { 1: ::1 } } ]"))
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := parse(data, parseTime)
		if err != nil {
			var ve *ValidationError
			if !errors.As(err, &ve) || len(ve.Errors) == 0 {
				t.Fatalf("error is not a non-empty *ValidationError: %v", err)
			}
			return
		}
		for i := 1; i < len(cfg.Operations); i++ {
			if cfg.Operations[i-1].ID >= cfg.Operations[i].ID {
				t.Fatalf("operations not sorted by unique id")
			}
		}
	})
}

// TestWarnings covers the settings that load but do nothing (audit B8).
func TestWarnings(t *testing.T) {
	in := `
templates:
  used: { type: icmp-echo, flow-label: 5 }
  spare: { type: icmp-echo }
operations:
  - template: used
    targets: { 1: 192.0.2.1, 2: "2001:db8::2" }
  - id: 3
    type: icmp-echo
    target: 192.0.2.3
    react:
      - { element: rtt, threshold-type: immediate, action: trap-and-syslog }
      - { element: timeout, threshold-type: immediate, action: syslog }
tracks:
  - { id: 1, operation: 3 }
actions:
  - { on: [threshold-exceeded], track: 1, exec: /bin/true }
  - { on: [track-down], track: 1, exec: /bin/true }
`
	cfg, err := parse([]byte(in), parseTime)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"templates.used.flow-label: ignored for the IPv4 targets",
		"actions[0].track: has no effect: on lists no track-up / track-down event",
		"templates.spare: not used by any operation",
		"operations(id=3).react[0].action: sends to syslog, but global.syslog is not set: no syslog message is sent (2 reactions)",
		"operations(id=3).react[0].action: sends a trap, but global.snmp.traps is empty: no trap is sent (1 reaction)",
	}
	if diff := cmp.Diff(want, cfg.Warnings); diff != "" {
		t.Errorf("warnings (-want +got):\n%s", diff)
	}

	// With the receivers configured, the reaction warnings go away.
	in = "global: { syslog: {}, snmp: { traps: [ { host: 192.0.2.9, community: c } ] } }\n" +
		"operations: [ { id: 3, type: icmp-echo, target: 192.0.2.3, react: [ { element: rtt, threshold-type: immediate, action: trap-and-syslog } ] } ]\n"
	cfg, err = parse([]byte(in), parseTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("warnings %q, want none", cfg.Warnings)
	}
}

// TestFieldErrorString is the one rendering of a FieldError (audit B10).
func TestFieldErrorString(t *testing.T) {
	if got := (FieldError{Msg: "yaml: line 1: x"}).String(); got != "yaml: line 1: x" {
		t.Errorf("no path: %q", got)
	}
	if got := (FieldError{Path: "a.b", Msg: "bad"}).String(); got != "a.b: bad" {
		t.Errorf("path: %q", got)
	}
}
