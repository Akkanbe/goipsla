//declscope:core // the public enumerations (config.FilterAll, ...): the package API itself

package config

// History filters (rttMonHistoryAdminFilter).
const (
	FilterNone          HistoryFilter = "none"
	FilterAll           HistoryFilter = "all"
	FilterOverThreshold HistoryFilter = "overThreshold"
	FilterFailures      HistoryFilter = "failures"
)

// Schedule start kinds.
const (
	StartNow     StartKind = "now"
	StartPending StartKind = "pending"
	StartAfter   StartKind = "after"
	StartAt      StartKind = "at"
)

// Reaction threshold types (rttMonReactThresholdType).
const (
	ThresholdNever       = "never"
	ThresholdImmediate   = "immediate"
	ThresholdConsecutive = "consecutive"
	ThresholdXofY        = "xofy"
	ThresholdAverage     = "average"
)

// Reaction action types (besides "none").
const (
	ActionSyslog        = "syslog"
	ActionTrap          = "trap"
	ActionTrapAndSyslog = "trap-and-syslog"
)

// Track modes.
const (
	TrackState        = "state"
	TrackReachability = "reachability"
)

// syslogFacilities are the accepted global.syslog.facility values.
//
//declscope:package // the parser accepts only these values
var syslogFacilities = []string{
	"kern", "user", "mail", "daemon", "auth", "syslog", "lpr", "news", "uucp", "cron", "authpriv", "ftp",
	"local0", "local1", "local2", "local3", "local4", "local5", "local6", "local7",
}

// actionNone is the default reaction action.
//
//declscope:package // the parser accepts only these values
const actionNone = "none"

// Events that can trigger an Action: the kinds of event.Kind. A timeout
// reaction reports through threshold-exceeded / threshold-cleared with
// element "timeout".
const (
	// The track events; the parser checks actions[].track against them.
	//
	//declscope:package // the parser checks actions[].track against them
	eventTrackUp = "track-up"
	//declscope:package // the parser checks actions[].track against them
	eventTrackDown = "track-down"

	eventThresholdExceeded = "threshold-exceeded"
	eventThresholdCleared  = "threshold-cleared"
)

// actionEvents are the accepted actions[].on values.
//
//declscope:package // the parser accepts only these values
var actionEvents = []string{
	eventThresholdExceeded, eventThresholdCleared, eventTrackUp, eventTrackDown,
}

// reactElement describes a monitored element of "react".
//
//declscope:package // the parser checks each reaction against its element
type reactElement struct {
	boolean      bool // timeout, verifyError: no threshold values, no average
	oneWay       bool // needs one-way delays (the latency elements)
	upper, lower int  // default threshold-value (Cisco Command Reference, Table 6)
}

// reactElements lists the elements accepted per operation type, after the
// ICMP Echo and ICMP Jitter columns of Cisco's "Supported Elements, by IP SLA
// Operation" table (packetLossSD / packetLossDS / packetMIA are UDP jitter
// only: ICMP cannot tell the direction of a loss). The defaults follow the
// Command Reference, Table 6.
//
//declscope:package // the parser checks each reaction against its element
var reactElements = map[OpType]map[string]reactElement{
	ICMPEcho: {
		"rtt":         {upper: 5000, lower: 3000},
		"timeout":     {boolean: true},
		"verifyError": {boolean: true},
	},
	ICMPJitter: {
		"rtt":                  {upper: 5000, lower: 3000},
		"timeout":              {boolean: true},
		"jitterAvg":            {upper: 100, lower: 100},
		"jitterSDAvg":          {upper: 100, lower: 100},
		"jitterDSAvg":          {upper: 100, lower: 100},
		"packetLateArrival":    {upper: 10000, lower: 10000},
		"packetOutOfSequence":  {upper: 10000, lower: 10000},
		"successivePacketLoss": {upper: 10000, lower: 10000},
		"packetLoss":           {upper: 10000, lower: 10000},
		"maxOfPositiveSD":      {upper: 10000, lower: 10000},
		"maxOfNegativeSD":      {upper: 10000, lower: 10000},
		"maxOfPositiveDS":      {upper: 10000, lower: 10000},
		"maxOfNegativeDS":      {upper: 10000, lower: 10000},
		"maxOfLatencySD":       {oneWay: true, upper: 5000, lower: 3000},
		"maxOfLatencyDS":       {oneWay: true, upper: 5000, lower: 3000},
		"latencySDAvg":         {oneWay: true, upper: 5000, lower: 3000},
		"latencyDSAvg":         {oneWay: true, upper: 5000, lower: 3000},
	},
}
