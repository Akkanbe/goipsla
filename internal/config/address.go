//declscope:namespace parse

package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// This file reads the addresses of the global section that other packages
// also parse at run time: the trap receivers and the AgentX master. The
// parser validates them with the same functions, so that a value that loads
// is a value that works.

// ParseTrapTarget reads a snmp.traps[].host value: "host", "host:port",
// "[ipv6]:port", "[ipv6]" or a bare IPv6 address. host is an IP address
// (without brackets) or a DNS name; port defaults to 162 (snmptrap).
func ParseTrapTarget(s string) (host string, port int, err error) {
	host, port = s, defaultTrapPort
	switch {
	case strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"):
		host = s[1 : len(s)-1]
	case strings.Count(s, ":") > 1 && !strings.HasPrefix(s, "["):
		// A bare IPv6 address: no port.
	case strings.Contains(s, ":"):
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return "", 0, errors.New("must be host, host:port or [ipv6]:port")
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, errors.New("port must be between 1 and 65535")
		}
		host, port = h, n
	}
	if !parsedHost(host) {
		return "", 0, errors.New("must be host, host:port or [ipv6]:port with an IP address or a host name")
	}
	return host, port, nil
}

// ParseAgentXAddress reads net-snmp's agentXSocket syntax: "/path",
// "unix:/path", "tcp:host:port" or "host:port". network is "unix" or "tcp".
func ParseAgentXAddress(s string) (network, address string, err error) {
	switch {
	case strings.HasPrefix(s, "unix:"):
		p := strings.TrimPrefix(s, "unix:")
		if !strings.HasPrefix(p, "/") {
			return "", "", fmt.Errorf("agentx address %q: unix: needs an absolute path", s)
		}
		return "unix", p, nil
	case strings.HasPrefix(s, "/"):
		return "unix", s, nil
	case strings.HasPrefix(s, "tcp:"):
		s = strings.TrimPrefix(s, "tcp:")
	case strings.HasPrefix(s, "udp:"):
		return "", "", fmt.Errorf("agentx over udp is not supported: %q", s)
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil || !parsedHost(h) {
		return "", "", fmt.Errorf("agentx address %q: want /path, unix:/path or tcp:host:port", s)
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("agentx address %q: port must be between 1 and 65535", s)
	}
	return "tcp", s, nil
}

// parsedHost reports whether h is an IP address or a DNS host name.
func parsedHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return h != "" && hostnamePattern.MatchString(h)
}
