//declscope:namespace pattern

package config

import "regexp"

// This file holds the patterns the parser matches values against.
//
//declscope:package // the parser matches values against these patterns
var (
	// digitsPattern is a bare number, which is a duration without a unit.
	digitsPattern = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]*)?$`)
	// groupPattern is a POSIX-style group name or a numeric gid
	// (api-socket-group). Whether the group exists is checked by the daemon
	// when it creates the socket.
	groupPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}[$]?$`)
	// hostnamePattern is a DNS host name (target).
	hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*\.?$`)
	// clockPattern is a start time of day, HH:MM[:SS].
	clockPattern = regexp.MustCompile(`^([0-9]{1,2}):([0-9]{2})(?::([0-9]{2}))?$`)
)
