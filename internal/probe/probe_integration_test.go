//go:build integration

package probe_test

import (
	"errors"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"goipsla/internal/probe"
)

func envAddr(t *testing.T, name, def string) netip.Addr {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		s = def
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return a
}

// newEngine opens a real engine, skipping the test without CAP_NET_RAW.
func newEngine(t *testing.T, late func(opID, seq uint32, sentAt time.Time)) probe.Engine {
	t.Helper()
	eng, err := probe.New(probe.Options{OnLateReply: late})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("raw sockets unavailable (needs CAP_NET_RAW): %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return eng
}

func TestIntegrationEcho(t *testing.T) {
	var (
		mu   sync.Mutex
		late int
	)
	eng := newEngine(t, func(uint32, uint32, time.Time) { mu.Lock(); late++; mu.Unlock() })
	targets := []netip.Addr{
		envAddr(t, "IPSLA_TARGET4", "10.100.1.11"),
		envAddr(t, "IPSLA_TARGET6", "fd00:100:1::11"),
	}
	for i, target := range targets {
		for _, size := range []int{probe.MinDataSize, 1400} {
			for seq := uint32(1); seq <= 3; seq++ {
				r := eng.Echo(t.Context(), probe.Request{
					OpID:     uint32(1000 + i),
					Seq:      seq,
					Target:   target,
					DataSize: size,
					Pattern:  probe.DefaultPattern,
					Verify:   true,
					Timeout:  2 * time.Second,
				})
				if r.Outcome != probe.OutcomeReply {
					t.Errorf("%s size %d seq %d: outcome %v detail %q err %v", target, size, seq, r.Outcome, r.Detail, r.Err)
					continue
				}
				if r.RTT <= 0 || r.RTT >= time.Second {
					t.Errorf("%s size %d seq %d: rtt %v out of (0, 1s)", target, size, seq, r.RTT)
				}
				if got := r.ReceivedAt.Sub(r.SentAt); got != r.RTT {
					t.Errorf("ReceivedAt - SentAt = %v, RTT = %v", got, r.RTT)
				}
				t.Logf("%s size %d seq %d: rtt %v", target, size, seq, r.RTT)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if late != 0 {
		t.Errorf("%d unexpected late replies", late)
	}
}

// TestIntegrationConcurrent sends from many operations at once to both
// targets over the shared sockets.
func TestIntegrationConcurrent(t *testing.T) {
	eng := newEngine(t, nil)
	targets := []netip.Addr{
		envAddr(t, "IPSLA_TARGET4", "10.100.1.11"),
		envAddr(t, "IPSLA_TARGET6", "fd00:100:1::11"),
	}
	const ops = 200
	var wg sync.WaitGroup
	for i := range ops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := eng.Echo(t.Context(), probe.Request{
				OpID:     uint32(i + 1),
				Seq:      1,
				Target:   targets[i%len(targets)],
				DataSize: probe.MinDataSize,
				Pattern:  probe.DefaultPattern,
				Verify:   true,
				Timeout:  3 * time.Second,
			})
			if r.Outcome != probe.OutcomeReply {
				t.Errorf("op %d: outcome %v detail %q err %v", i+1, r.Outcome, r.Detail, r.Err)
			}
		}()
	}
	wg.Wait()
}

func TestIntegrationJitter(t *testing.T) {
	eng := newEngine(t, nil)
	target := envAddr(t, "IPSLA_TARGET4", "10.100.1.11")
	r := eng.Jitter(t.Context(), probe.JitterRequest{
		OpID:       2000,
		Seq:        1,
		Target:     target,
		NumPackets: 10,
		Interval:   20 * time.Millisecond,
		Timeout:    2 * time.Second,
	})
	if r.Err != nil || len(r.Packets) != 10 {
		t.Fatalf("reply = %+v", r)
	}
	for _, pk := range r.Packets {
		if pk.Outcome != probe.OutcomeReply {
			t.Errorf("packet %d: outcome %v detail %q err %v", pk.Index, pk.Outcome, pk.Detail, pk.Err)
			continue
		}
		if pk.RTT <= 0 || pk.RTT >= time.Second || pk.Receive == 0 || pk.Transmit == 0 {
			t.Errorf("packet %d: %+v", pk.Index, pk)
		}
		if pk.Index > 0 {
			gap := pk.SentAt.Sub(r.Packets[pk.Index-1].SentAt)
			if gap < 15*time.Millisecond || gap > 30*time.Millisecond {
				t.Errorf("packet %d sent %v after the previous one", pk.Index, gap)
			}
		}
	}
	t.Logf("packet 0: %+v", r.Packets[0])

	if r := eng.Jitter(t.Context(), probe.JitterRequest{Target: envAddr(t, "IPSLA_TARGET6", "fd00:100:1::11"), NumPackets: 1, Timeout: time.Second}); !errors.Is(r.Err, probe.ErrUnsupportedFamily) {
		t.Errorf("ipv6 jitter err = %v", r.Err)
	}
}
