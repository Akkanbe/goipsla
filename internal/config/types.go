//declscope:core // the public types (config.Config, config.Operation, ...): the package API itself

// Package config loads the goipslad YAML configuration, expands templates,
// fills in Cisco defaults and validates the result.
package config

import (
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// OpType is the operation type.
type OpType string

// Operation types.
const (
	ICMPEcho   OpType = "icmp-echo"
	ICMPJitter OpType = "icmp-jitter"
)

// HistoryFilter selects which attempts are kept in the history buckets:
// "none" | "all" | "overThreshold" | "failures".
type HistoryFilter string

// Config is a validated configuration.
type Config struct {
	Global     Global
	Operations []*Operation // sorted by ID; IDs are unique
	Tracks     []Track
	Actions    []Action
	// Warnings are settings that are accepted but have no effect, as
	// "<path>: <message>" lines: a reaction action with no receiver, an
	// unused template, a flow-label ignored for an IPv4 target, a track
	// filter on an action that only reacts to threshold events. goipsla
	// validate prints them; goipslad logs them at start and on reload.
	Warnings []string
}

// Global holds daemon-wide settings.
type Global struct {
	APISocket string // default /run/goipslad/goipslad.sock
	// APISocketGroup, if not empty, becomes the group of the API socket (a
	// group name or a numeric gid), so that its members can use goipsla.
	APISocketGroup string
	MetricsListen  string // default 127.0.0.1:9818; empty disables the exporter
	Log            LogConfig
	Syslog         *SyslogConfig // nil disables syslog (P5)
	SNMP           *SNMPConfig   // nil disables SNMP (P7)
}

// LogConfig configures the daemon's slog handler.
type LogConfig struct {
	Format string // "text" | "json"; default text
	Level  string // "debug" | "info" | "warn" | "error"; default info
}

// SyslogConfig configures the syslog sink.
type SyslogConfig struct{ Facility string }

// SNMPConfig configures the AgentX subagent and traps.
type SNMPConfig struct {
	AgentX string
	Traps  []TrapTarget
}

// TrapTarget is a trap receiver.
type TrapTarget struct {
	Host      string
	Community string
}

// Operation is the effective configuration of one operation, after template
// expansion and with defaults filled in.
type Operation struct {
	ID         int
	Type       OpType
	Target     netip.Addr
	TargetName string // the target exactly as written in the file

	SourceIP        netip.Addr
	SourceInterface string
	VRF             string
	TOS             uint8  // IPv4
	TrafficClass    uint8  // IPv6
	FlowLabel       uint32 // IPv6

	Frequency       time.Duration // default 60s
	Timeout         time.Duration // default 5000ms
	Threshold       time.Duration // default 5000ms
	RequestDataSize int           // default 28
	DataPattern     uint32        // default 0xABCDABCD
	VerifyData      bool
	Tag             string // 0..128 characters (rttMonCtrlAdminLongTag)
	Owner           string // 0..255 characters
	Template        string // name of the template used; empty if none

	// icmp-jitter
	Interval    time.Duration // default 20ms
	NumPackets  int           // default 10
	OneWayDelay bool          // one-way-delay: accumulate one-way delays; default false (target clocks are not known to be synchronized)

	Stats    StatsConfig
	History  HistoryConfig
	Enhanced *EnhancedHistory // nil disables enhanced history
	Schedule *Schedule        // nil means start at load time and run forever
	React    []Reaction
}

// StatsConfig configures the hourly aggregated statistics.
type StatsConfig struct {
	HoursKept    int           // hours-of-statistics-kept 0..25, default 2
	DistBuckets  int           // distributions-of-statistics-kept 1..20, default 1
	DistInterval time.Duration // statistics-distribution-interval 1..100ms, default 20ms
}

// HistoryConfig configures the snapshot history.
type HistoryConfig struct {
	Lives   int           // lives-kept 0..2, default 0
	Buckets int           // buckets-kept 1..60, default 15
	Filter  HistoryFilter // default none
}

// EnhancedHistory configures the enhanced (interval) history.
type EnhancedHistory struct {
	Interval time.Duration // 1..3600s, default 900s
	Buckets  int           // 1..100, default 100
}

// StartKind is how a schedule starts: "now" | "pending" | "after" | "at".
type StartKind string

// Schedule is the Cisco "ip sla schedule" of an operation.
type Schedule struct {
	Life      time.Duration // ignored when Forever is true
	Forever   bool
	Start     StartKind
	After     time.Duration // Start == "after"
	At        time.Time     // Start == "at"
	Ageout    time.Duration // 0 disables ageout
	Recurring bool
	Daily     bool // start-time was written as HH:MM[:SS]: At is the next occurrence of that time of day
}

// Reaction is one "ip sla reaction-configuration" entry.
type Reaction struct {
	Element       string // rtt | timeout | verifyError | jitterAvg | jitterSDAvg | jitterDSAvg | packetLossSD | packetLossDS | ...
	ThresholdType string // never | immediate | consecutive | xofy | average
	Count         int    // N of consecutive / average
	X, Y          int    // xofy
	Upper, Lower  int    // threshold-value; milliseconds for rtt
	Action        string // none | syslog | trap | trap-and-syslog
}

// Track is an object-tracking entry bound to an operation.
type Track struct {
	ID        int
	Operation int
	Mode      string // state | reachability
	DelayUp   time.Duration
	DelayDown time.Duration
}

// Action is an external notification triggered by events.
type Action struct {
	On      []string // track-down, track-up, threshold-exceeded, threshold-cleared, ...
	Track   int      // 0 means every track
	Webhook string
	Exec    string
}

// FieldError is one validation error.
type FieldError struct {
	Path string // e.g. "operations[2].timeout" / "operations(id=101).frequency"
	Msg  string
}

// String renders the error as "<path>: <msg>", or "<msg>" when there is
// no path (a YAML syntax error).
func (fe FieldError) String() string {
	if fe.Path == "" {
		return fe.Msg
	}
	return fe.Path + ": " + fe.Msg
}

// ValidationError carries every validation error found in a configuration.
type ValidationError struct{ Errors []FieldError }

// Error joins all errors on one line, e.g.
// "invalid configuration: 2 errors: operations[0].timeout: must be less than frequency; ...".
// Callers that report to a terminal should print Errors one per line instead.
func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("invalid configuration")
	switch n := len(e.Errors); n {
	case 0:
		return b.String()
	case 1:
		b.WriteString(": ")
	default:
		b.WriteString(": ")
		b.WriteString(strconv.Itoa(n))
		b.WriteString(" errors: ")
	}
	for i, fe := range e.Errors {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(fe.String())
	}
	return b.String()
}

// MaxOperations is the largest number of operations after template
// expansion (rttMonApplNumCtrlAdminEntries).
const MaxOperations = 1000

// MaxRequestDataSize is the largest request-data-size in bytes
// (rttMonApplMaxPacketDataSize).
const MaxRequestDataSize = 16384

// MaxOpID is the largest operation (and track) ID: rttMonCtrlAdminIndex is
// Integer32 (1..2147483647).
const MaxOpID = 2147483647
