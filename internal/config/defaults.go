//declscope:namespace default

package config

import "time"

// Global defaults.
//
//declscope:package // the parser fills in these defaults
const (
	defaultAPISocket      = "/run/goipslad/goipslad.sock"
	defaultMetricsListen  = "127.0.0.1:9818"
	defaultSyslogFacility = "daemon"
	defaultAgentX         = "/var/agentx/master"
)

// Operation defaults (Cisco / CISCO-RTTMON-MIB DEFVALs).
//
//declscope:package // the parser fills in these defaults
const (
	defaultFrequency       = 60 * time.Second
	defaultTimeout         = 5000 * time.Millisecond
	defaultThreshold       = 5000 * time.Millisecond
	defaultRequestDataSize = 28
	defaultDataPattern     = 0xABCDABCD
	defaultInterval        = 20 * time.Millisecond
	defaultNumPackets      = 10

	defaultHoursKept    = 2
	defaultDistBuckets  = 1
	defaultDistInterval = 20 * time.Millisecond

	defaultLivesKept   = 0
	defaultBucketsKept = 15

	defaultEnhancedInterval = 900 * time.Second
	defaultEnhancedBuckets  = 100

	defaultThresholdCount = 5 // consecutive / average N, xofy x and y
)

// defaultTrapPort is the port of a trap receiver written without one.
//
//declscope:package // ParseTrapTarget fills it in
const defaultTrapPort = 162

// Log defaults, used only by defaultGlobal.
const (
	defaultLogFormat = "text"
	defaultLogLevel  = "info"
)

// defaultGlobal is the global section of an empty file.
//
//declscope:package // the parser starts from it
func defaultGlobal() Global {
	return Global{
		APISocket:     defaultAPISocket,
		MetricsListen: defaultMetricsListen,
		Log:           LogConfig{Format: defaultLogFormat, Level: defaultLogLevel},
	}
}
