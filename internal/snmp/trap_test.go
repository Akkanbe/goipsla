package snmp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
)

var _ event.Sink = (*TrapSink)(nil)

func trapEvent(kind event.Kind, action string) event.Event {
	return event.Event{
		Kind: kind, OpID: 11, Type: config.ICMPEcho, Target: "10.100.1.11", Tag: "wan",
		Element: "rtt", ThresholdType: "consecutive", Value: 350, Upper: 300, Lower: 200, Action: action,
	}
}

func TestTrapWants(t *testing.T) {
	s := NewTrapSink(&config.SNMPConfig{}, newFake(), Options{Logger: quiet()})
	for _, tt := range []struct {
		ev   event.Event
		want bool
	}{
		{trapEvent(event.ThresholdExceeded, "trap"), true},
		{trapEvent(event.ThresholdCleared, "trap-and-syslog"), true},
		{trapEvent(event.ThresholdExceeded, "syslog"), false},
		{trapEvent(event.ThresholdExceeded, "none"), false},
		{event.Event{Kind: event.TrackDown, OpID: 11}, false},
	} {
		if got := s.Wants(tt.ev); got != tt.want {
			t.Errorf("%s %s: %v", tt.ev.Kind, tt.ev.Action, got)
		}
	}
}

func TestTrapVarbinds(t *testing.T) {
	fake := clock.NewFake(base.Add(90 * time.Second))
	s := NewTrapSink(&config.SNMPConfig{}, newFake(), Options{Start: base, Clock: fake, Logger: quiet()})
	ev := trapEvent(event.ThresholdExceeded, "trap")
	ev.Element = "timeout" // the second reaction row of op 11
	got := []string{}
	for _, v := range s.varbinds(ev) {
		got = append(got, fmt.Sprintf("%s=%v", v.Name, v.Value))
	}
	want := []string{
		".1.3.6.1.2.1.1.3.0=9000",
		".1.3.6.1.6.3.1.1.4.1.0=.1.3.6.1.4.1.9.9.42.2.0.8",
		".1.3.6.1.4.1.9.9.42.1.2.1.1.12.11=[119 97 110]",
		".1.3.6.1.4.1.9.9.42.1.4.1.1.5.11.1.1.1=[10 100 1 11]",
		".1.3.6.1.4.1.9.9.42.1.2.19.1.2.11.2=7",
		".1.3.6.1.4.1.9.9.42.1.2.19.1.10.11.2=1",
		".1.3.6.1.4.1.9.9.42.1.2.19.1.9.11.2=350",
		".1.3.6.1.4.1.9.9.42.1.2.19.1.5.11.2=300",
		".1.3.6.1.4.1.9.9.42.1.2.19.1.6.11.2=200",
		".1.3.6.1.4.1.9.9.42.1.2.2.1.33.11=[]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	cleared := s.varbinds(trapEvent(event.ThresholdCleared, "trap"))
	if cleared[5].Value != 2 || !strings.HasSuffix(cleared[5].Name, ".11.1") {
		t.Errorf("cleared occurred %v %s", cleared[5].Value, cleared[5].Name)
	}
}

func TestTrapOverUDP(t *testing.T) {
	type got struct {
		community string
		vars      map[string]any
	}
	traps := make(chan got, 4)
	tl := gosnmp.NewTrapListener()
	tl.Params = gosnmp.Default
	tl.OnNewTrap = func(p *gosnmp.SnmpPacket, _ *net.UDPAddr) {
		g := got{community: p.Community, vars: map[string]any{}}
		for _, v := range p.Variables {
			g.vars[v.Name] = v.Value
		}
		traps <- g
	}
	// Find a free port, then listen on it.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	go func() { _ = tl.Listen(addr) }()
	defer tl.Close()
	select {
	case <-tl.Listening():
	case <-time.After(5 * time.Second):
		t.Fatal("trap listener did not start")
	}

	cfg := &config.SNMPConfig{Traps: []config.TrapTarget{{Host: addr, Community: "public"}}}
	s := NewTrapSink(cfg, newFake(), Options{Start: base, Clock: clock.NewFake(base.Add(time.Second)), Logger: quiet()})
	if err := s.Deliver(context.Background(), trapEvent(event.ThresholdExceeded, "trap")); err != nil {
		t.Fatal(err)
	}
	// Not a trap: nothing sent.
	if err := s.Deliver(context.Background(), trapEvent(event.ThresholdExceeded, "syslog")); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-traps:
		if g.community != "public" {
			t.Errorf("community %q", g.community)
		}
		if g.vars[".1.3.6.1.6.3.1.1.4.1.0"] != ".1.3.6.1.4.1.9.9.42.2.0.8" {
			keys := make([]string, 0, len(g.vars))
			for k := range g.vars {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Errorf("trap OID %v (vars %v)", g.vars[".1.3.6.1.6.3.1.1.4.1.0"], keys)
		}
		if v := g.vars[".1.3.6.1.4.1.9.9.42.1.2.19.1.10.11.1"]; v != 1 {
			t.Errorf("occurred %v", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no trap received")
	}
	select {
	case g := <-traps:
		t.Errorf("unexpected second trap %v", g)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTrapFailure(t *testing.T) {
	cfg := &config.SNMPConfig{Traps: []config.TrapTarget{{Host: "a", Community: "c"}, {Host: "b", Community: "c"}}}
	s := NewTrapSink(cfg, newFake(), Options{Logger: quiet()})
	var hosts []string
	var mu sync.Mutex
	s.send = func(_ context.Context, t config.TrapTarget, _ []gosnmp.SnmpPDU) error {
		mu.Lock()
		defer mu.Unlock()
		hosts = append(hosts, t.Host)
		if t.Host == "a" {
			return fmt.Errorf("unreachable")
		}
		return nil
	}
	err := s.Deliver(context.Background(), trapEvent(event.ThresholdExceeded, "trap"))
	if err == nil || !strings.Contains(err.Error(), "a: unreachable") || len(hosts) != 2 {
		t.Errorf("err %v hosts %v", err, hosts)
	}
}

func TestTrapDeliverHonorsContext(t *testing.T) {
	cfg := &config.SNMPConfig{Traps: []config.TrapTarget{{Host: "a", Community: "c"}, {Host: "b", Community: "c"}, {Host: "c", Community: "c"}}}
	s := NewTrapSink(cfg, newFake(), Options{Logger: quiet()})
	// Three receivers that never answer, behind a sender that does not look
	// at ctx at all: Deliver itself must stop waiting when ctx ends.
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	defer close(release)
	s.send = func(context.Context, config.TrapTarget, []gosnmp.SnmpPDU) error {
		started <- struct{}{}
		<-release
		return nil
	}

	// Through a Bus, as in the daemon: stopping it must not wait on the
	// receivers (goipslad drains the bus for 5 s at most).
	bus := event.NewBus(quiet())
	bus.Subscribe(s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bus.Run(ctx) }()
	bus.Publish(trapEvent(event.ThresholdExceeded, "trap"))
	for i := 0; i < 3; i++ { // the three sends run in parallel
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 sends started", i)
		}
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bus did not stop within 5 s")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("stop took %v", d)
	}
	if st := bus.Stats(); st.Sinks[0].Failed != 1 {
		t.Errorf("stats %+v", st)
	}
}

func TestTrapSendUDPCanceled(t *testing.T) {
	// An already-canceled context stops the real sender before it sends.
	s := NewTrapSink(&config.SNMPConfig{}, newFake(), Options{Logger: quiet()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := s.sendUDP(ctx, config.TrapTarget{Host: "192.0.2.1", Community: "c"}, s.varbinds(trapEvent(event.ThresholdExceeded, "trap")))
	if err == nil {
		t.Error("send with a canceled context succeeded")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
}

func TestTrapUsesReactionIndexFromEvent(t *testing.T) {
	s := NewTrapSink(&config.SNMPConfig{}, newFake(), Options{Start: base, Clock: clock.NewFake(base), Logger: quiet()})
	// The event was raised for row 3; since then a reload has left rtt as
	// row 1. The trap names row 3, as the event says.
	ev := trapEvent(event.ThresholdExceeded, "trap")
	ev.ReactionIndex = 3
	for _, v := range s.varbinds(ev)[4:9] {
		if !strings.HasSuffix(v.Name, ".11.3") {
			t.Errorf("%s: want instance 11.3", v.Name)
		}
	}
	// Without an index (0), the current rows are searched: rtt is row 1.
	ev.ReactionIndex = 0
	if v := s.varbinds(ev)[4]; !strings.HasSuffix(v.Name, ".11.1") {
		t.Errorf("%s: want instance 11.1", v.Name)
	}
}

// TestTrapOverUDPv6 sends to an IPv6 target written "[addr]:port" (audit
// B3), with a debug logger that must not see the community (audit B2).
func TestTrapOverUDPv6(t *testing.T) {
	pc, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer pc.Close()
	got := make(chan int, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _, err := pc.ReadFrom(buf)
		if err == nil {
			got <- n
		}
	}()

	var logs bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(trapLogWriter(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	host := pc.LocalAddr().String() // "[::1]:port"
	cfg := &config.SNMPConfig{Traps: []config.TrapTarget{{Host: host, Community: "s3cretCommunity"}}}
	s := NewTrapSink(cfg, newFake(), Options{Start: base, Clock: clock.NewFake(base.Add(time.Second)), Logger: logger})
	if err := s.Deliver(context.Background(), trapEvent(event.ThresholdExceeded, "trap")); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n == 0 {
			t.Error("empty trap")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no trap received over IPv6")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), "s3cretCommunity") {
		t.Errorf("community in the debug log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "snmp trap sent") {
		t.Errorf("no debug log of the sent trap:\n%s", logs.String())
	}
}

// trapLogWriter adapts a function to io.Writer.
type trapLogWriter func([]byte) (int, error)

func (f trapLogWriter) Write(p []byte) (int, error) { return f(p) }
