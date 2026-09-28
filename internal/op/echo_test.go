package op

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/probe"
)

var echoStart = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func echoOp(target string) *config.Operation {
	return &config.Operation{
		ID:              101,
		Type:            config.ICMPEcho,
		Target:          netip.MustParseAddr(target),
		TargetName:      target,
		SourceIP:        netip.MustParseAddr("10.0.0.1"),
		SourceInterface: "eth1",
		VRF:             "blue",
		TOS:             0xA0,
		TrafficClass:    0xB8,
		FlowLabel:       0x12345,
		Frequency:       60 * time.Second,
		Timeout:         5 * time.Second,
		Threshold:       100 * time.Millisecond,
		RequestDataSize: 64,
		DataPattern:     0x01020304,
		VerifyData:      true,
	}
}

func TestEchoRequest(t *testing.T) {
	tests := []struct {
		target string
		tos    uint8
	}{
		{"192.0.2.1", 0xA0},
		{"2001:db8::1", 0xB8},
		{"::ffff:192.0.2.1", 0xA0},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			cfg := echoOp(tt.target)
			eng := &fakeEngine{reply: probe.Reply{Outcome: probe.OutcomeReply, RTT: time.Millisecond}}
			NewEcho(cfg, eng, clock.NewFake(echoStart)).Run(context.Background(), 7, echoStart)
			if len(eng.reqs) != 1 {
				t.Fatalf("Echo called %d times, want 1", len(eng.reqs))
			}
			want := probe.Request{
				OpID:      101,
				Seq:       7,
				Target:    cfg.Target,
				Source:    cfg.SourceIP,
				Interface: "eth1",
				VRF:       "blue",
				TOS:       tt.tos,
				FlowLabel: 0x12345,
				DataSize:  64,
				Pattern:   0x01020304,
				Verify:    true,
				Timeout:   5 * time.Second,
			}
			if got := eng.reqs[0]; got != want {
				t.Errorf("request = %+v\nwant      %+v", got, want)
			}
		})
	}
}

func TestEchoResult(t *testing.T) {
	sockErr := errors.New("sendmsg: network is unreachable")
	tests := []struct {
		name   string
		reply  probe.Reply
		code   ReturnCode
		rtt    time.Duration
		detail string
	}{
		{"ok", probe.Reply{Outcome: probe.OutcomeReply, RTT: 12345 * time.Microsecond}, RCOK, 12345 * time.Microsecond, ""},
		{"ok at threshold", probe.Reply{Outcome: probe.OutcomeReply, RTT: 100 * time.Millisecond}, RCOK, 100 * time.Millisecond, ""},
		{"over threshold", probe.Reply{Outcome: probe.OutcomeReply, RTT: 100*time.Millisecond + 1}, RCOverThreshold, 100*time.Millisecond + 1, ""},
		{"timeout", probe.Reply{Outcome: probe.OutcomeTimeout}, RCTimeout, 0, ""},
		{"unreachable", probe.Reply{Outcome: probe.OutcomeUnreachable, Detail: "destination unreachable: host, from 10.100.1.254"},
			RCTimeout, 0, "destination unreachable: host, from 10.100.1.254"},
		{"verify error", probe.Reply{Outcome: probe.OutcomeVerifyError}, RCVerifyError, 0, ""},
		{"error", probe.Reply{Outcome: probe.OutcomeError, Err: sockErr}, RCError, 0, sockErr.Error()},
		{"error without err", probe.Reply{Outcome: probe.OutcomeError, Detail: "no socket"}, RCError, 0, "no socket"},
		{"unknown outcome", probe.Reply{Outcome: probe.Outcome(99)}, RCOther, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := clock.NewFake(echoStart)
			eng := &fakeEngine{clk: clk, reply: tt.reply}
			start := echoStart.Add(-time.Millisecond)
			got := NewEcho(echoOp("192.0.2.1"), eng, clk).Run(context.Background(), 3, start)
			want := Result{
				OpID:   101,
				Type:   config.ICMPEcho,
				Seq:    3,
				Start:  start,
				End:    echoStart.Add(tt.reply.RTT),
				RTT:    tt.rtt,
				Code:   tt.code,
				Detail: tt.detail,
			}
			if got != want {
				t.Errorf("result = %+v\nwant     %+v", got, want)
			}
		})
	}
}
