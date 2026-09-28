// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goipsla/internal/clock"
)

// logBuffer is a concurrency-safe log sink for checking what the engine logs.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitLog polls until the log holds want n times.
func (b *logBuffer) waitLog(t *testing.T, want string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(b.String(), want) < n {
		if time.Now().After(deadline) {
			t.Fatalf("log has %d × %q, want %d:\n%s", strings.Count(b.String(), want), want, n, b.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// newLoggedEngine is an engine on fake sockets whose info-level log is kept.
// open may fail for some sockets; it defaults to fake sockets for all.
func newLoggedEngine(t *testing.T, open func(v6 bool, vrf string) (packetConn, error)) (*engine, *clock.Fake, *logBuffer) {
	t.Helper()
	clk := clock.NewFake(t0)
	logs := &logBuffer{}
	e, err := newEngine(Options{
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Clock:  clk,
	}, open, func(string) (int, error) { return 0, errors.New("no interfaces") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, clk, logs
}

func TestStats(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	req := baseReq(target4)
	ch := h.start(req)
	p := c.nextSent(t)
	c.deliver(echoReply(p, func(icmp []byte) { binary.BigEndian.PutUint16(icmp[4:], 999) }), target4, time.Time{}) // foreign
	c.deliver([]byte{0x45}, target4, time.Time{})                                                                  // malformed
	c.deliver(echoReply(p, nil), target4, time.Time{})
	h.wait(ch)
	c.deliver(echoReply(p, nil), target4, time.Time{}) // duplicate: late
	h.expectLate(req.OpID, req.Seq)
	c.failRead(errors.New("boom"))
	waitCount(t, &h.e.stats.recvErrors, 1)
	want := Stats{Foreign: 1, Malformed: 1, Late: 1, RecvErrors: 1}
	if got := h.e.Stats(); got != want {
		t.Errorf("Stats() = %+v, want %+v", got, want)
	}
}

// TestReceiveErrorWarnsThrottled: the first failed read and then one a
// minute are warnings with the count; the recovery is logged at info.
func TestReceiveErrorWarnsThrottled(t *testing.T) {
	var c4 *fakeConn
	e, clk, logs := newLoggedEngine(t, func(v6 bool, vrf string) (packetConn, error) {
		c := newFakeConn(v6, vrf)
		if !v6 {
			c4 = c
		}
		return c, nil
	})
	fail := func() {
		n := e.stats.recvErrors.Load()
		c4.failRead(errors.New("no buffer space available"))
		waitCount(t, &e.stats.recvErrors, n+1)
		clk.BlockUntil(2) // the sweeper and the read backoff
		clk.Advance(readErrorBackoff)
	}
	fail()
	fail()
	logs.waitLog(t, "receive failed", 1)
	clk.Advance(recvErrorLogEvery)
	fail()
	logs.waitLog(t, "receive failed", 2)
	if !strings.Contains(logs.String(), "count=3") {
		t.Errorf("second warning should carry count=3:\n%s", logs)
	}
	c4.deliver([]byte{0x45}, target4, time.Time{})
	logs.waitLog(t, "receiving again", 1)
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "failed_reads=3") {
		t.Errorf("log:\n%s", logs)
	}
}

// TestVRFOpenFailure: a VRF socket that cannot be opened is warned about
// once, not retried for vrfRetryAfter, and reported as recovered.
func TestVRFOpenFailure(t *testing.T) {
	var opens atomic.Int32
	var fixed atomic.Bool
	e, clk, logs := newLoggedEngine(t, func(v6 bool, vrf string) (packetConn, error) {
		if vrf == "red" {
			opens.Add(1)
			if !fixed.Load() {
				return nil, errors.New("SO_BINDTODEVICE \"red\": no such device")
			}
		}
		return newFakeConn(v6, vrf), nil
	})
	req := baseReq(target4)
	req.VRF = "red"
	for range 3 {
		if r := e.Echo(t.Context(), req); r.Outcome != OutcomeError || !strings.Contains(r.Err.Error(), "vrf red: SO_BINDTODEVICE") {
			t.Fatalf("reply = %+v", r)
		}
	}
	if opens.Load() != 1 {
		t.Errorf("opened %d times within the retry delay, want 1", opens.Load())
	}
	clk.Advance(vrfRetryAfter)
	e.Echo(t.Context(), req)
	if opens.Load() != 2 {
		t.Errorf("opened %d times after the retry delay, want 2", opens.Load())
	}
	if n := strings.Count(logs.String(), "cannot open the vrf raw socket"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs)
	}
	clk.Advance(vrfRetryAfter)
	fixed.Store(true)
	if _, err := e.socketFor(sockKey{vrf: "red"}); err != nil {
		t.Fatal(err)
	}
	logs.waitLog(t, "opened the vrf raw socket after earlier failures", 1)
}

// TestForgetReleasesIdentifier: after Forget, the operation's Identifier is
// free for another operation.
func TestForgetReleasesIdentifier(t *testing.T) {
	h := newHarness(t, false)
	c := h.conn(false, "")
	a := baseReq(target4) // OpID 101 takes Identifier 101
	ch := h.start(a)
	p := c.nextSent(t)
	c.deliver(echoReply(p, nil), target4, time.Time{})
	h.wait(ch)

	b := baseReq(target4)
	b.OpID = 101 + 65536 // prefers Identifier 101 too
	send := func() uint16 {
		ch := h.start(b)
		p := c.nextSent(t)
		c.deliver(echoReply(p, nil), target4, time.Time{})
		if r := h.wait(ch); r.Outcome != OutcomeReply {
			t.Fatalf("reply = %+v", r)
		}
		return binary.BigEndian.Uint16(p.msg[4:])
	}
	if id := send(); id != 102 {
		t.Fatalf("identifier while 101 is taken = %d, want 102", id)
	}
	h.e.Forget(a.OpID)
	h.e.Forget(b.OpID)
	b.Seq++
	if id := send(); id != 101 {
		t.Errorf("identifier after Forget = %d, want 101", id)
	}
}
