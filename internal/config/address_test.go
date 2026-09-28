//declscope:namespace parse

package config

import "testing"

func TestParseTrapTarget(t *testing.T) {
	tests := []struct {
		in   string
		host string
		port int
	}{
		{"192.0.2.10", "192.0.2.10", 162},
		{"192.0.2.10:1162", "192.0.2.10", 1162},
		{"traps.example.net", "traps.example.net", 162},
		{"traps.example.net:10162", "traps.example.net", 10162},
		{"[2001:db8::10]:1162", "2001:db8::10", 1162},
		{"[2001:db8::10]", "2001:db8::10", 162},
		{"2001:db8::10", "2001:db8::10", 162},
	}
	for _, tt := range tests {
		h, p, err := ParseTrapTarget(tt.in)
		if err != nil || h != tt.host || p != tt.port {
			t.Errorf("%s: %q %d %v, want %q %d", tt.in, h, p, err, tt.host, tt.port)
		}
	}
	for _, bad := range []string{"", "192.0.2.1:0", "192.0.2.1:65536", "192.0.2.1:abc", "bad host", "[]", "[2001:db8::1]:", "host:"} {
		if h, p, err := ParseTrapTarget(bad); err == nil {
			t.Errorf("%q accepted as %q %d", bad, h, p)
		}
	}
}

func TestParseAgentXAddress(t *testing.T) {
	tests := []struct{ in, network, addr string }{
		{"/var/agentx/master", "unix", "/var/agentx/master"},
		{"unix:/run/agentx", "unix", "/run/agentx"},
		{"tcp:10.100.1.20:705", "tcp", "10.100.1.20:705"},
		{"localhost:705", "tcp", "localhost:705"},
		{"tcp:[::1]:705", "tcp", "[::1]:705"},
	}
	for _, tt := range tests {
		n, a, err := ParseAgentXAddress(tt.in)
		if err != nil || n != tt.network || a != tt.addr {
			t.Errorf("%s: %s %s %v", tt.in, n, a, err)
		}
	}
	for _, bad := range []string{"udp:1.2.3.4:705", "tcp:nohost", "foo bar", "unix:relative", "host:0", "bad host:705"} {
		if _, _, err := ParseAgentXAddress(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
