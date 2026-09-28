// Tests of the fixes of the 2026-09-27 audit (fix-audit.md B21, B23, B25).
//
//declscope:namespace agentx

package agentx

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"goipsla/internal/clock"
)

// lockedBuffer is a log sink the agent goroutine writes to.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// slogText is a text logger into w.
func slogText(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// runLogged runs the agent against ln with a text logger and returns its
// logs, a cancel and a channel closed when runAgent returns.
func runLogged(t *testing.T, ln net.Listener) (*lockedBuffer, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runAgent(ctx, testConfig("tcp", ln.Addr().String()), newTestHandler(), clock.Real(), slogText(logs))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return logs, cancel, done
}

// Every loss of an open session is logged, not only the first (B21).
func TestSessionLostIsLogged(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	logs, cancel, done := runLogged(t, ln)
	for i := range 4 {
		conn, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		(&master{t: t, conn: conn}).handshake(uint32(10 + i))
		if i < 3 {
			conn.Close() // the master goes away
		} else {
			defer conn.Close()
		}
	}
	// The agent logs the 4th session after reading the Register response.
	for deadline := time.Now().Add(5 * time.Second); strings.Count(logs.String(), "agentx session open") < 4 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	got := logs.String()
	if n := strings.Count(got, "agentx session open"); n != 4 {
		t.Errorf("%d session open logs, want 4:\n%s", n, got)
	}
	if n := strings.Count(got, "agentx session lost; reconnecting"); n != 3 {
		t.Errorf("%d session lost logs, want 3:\n%s", n, got)
	}
	if strings.Contains(got, "not reachable") {
		t.Errorf("a lost session logged as unreachable:\n%s", got)
	}
	if !strings.Contains(got, "level=WARN msg=\"agentx session lost; reconnecting\"") || !strings.Contains(got, "failures=0") {
		t.Errorf("levels or attributes:\n%s", got)
	}
}

// Stopping while the master does not answer Open returns at once, not after
// the 10 s request timeout (B21).
func TestStopDuringOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, cancel, done := runLogged(t, ln)
	conn, err := ln.Accept() // and never answer
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	(&master{t: t, conn: conn}).read() // the Open
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runAgent did not stop during Open")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("stopped after %v", d)
	}
}

// A refused registration is its own error log with the RFC name (B21).
func TestRegisterRefusedIsLogged(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	logs, cancel, done := runLogged(t, ln)
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	m := &master{t: t, conn: conn}
	h, _ := m.read()
	m.respond(h, 5, errNone)
	h, _ = m.read()
	m.respond(h, 5, 263)
	conn2, err := ln.Accept() // the retry
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	conn2.Close()
	cancel()
	<-done
	got := logs.String()
	if !strings.Contains(got, `level=ERROR msg="agentx registration refused"`) ||
		!strings.Contains(got, "register 1.3.6.1.4.1.9.9.42: master answered duplicateRegistration (263)") {
		t.Errorf("logs:\n%s", got)
	}
}

// CleanupSet and a stray Response get no Response (B23).
func TestAnswerWithoutResponse(t *testing.T) {
	h := newTestHandler()
	for _, typ := range []uint8{pduCleanupSet, pduResponse} {
		if p := answer(header{typ: typ}, &decoder{}, h, 0); p != nil {
			t.Errorf("PDU type %d answered", typ)
		}
	}
	for _, typ := range []uint8{pduCommitSet, pduUndoSet, pduPing} {
		if p := answer(header{typ: typ}, &decoder{}, h, 0); p == nil {
			t.Errorf("PDU type %d not answered", typ)
		}
	}
}

// A request in a non-default context is answered unsupportedContext (B23).
func TestNonDefaultContext(t *testing.T) {
	var e encoder
	e.octets([]byte("vrf-blue")) // the context
	e.b = append(e.b, ranges(searchRange{start: testOID("1.1.4.0")})...)
	raw := packet(header{typ: pduGet, flags: flagNonDefaultContext, session: 1, packet: 1}, e.b)
	h, d, err := readPDU(bytesReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	p := answer(h, d, newTestHandler(), 0)
	_, rd, err := readPDU(bytesReader(packet(header{typ: pduResponse}, p)))
	if err != nil {
		t.Fatal(err)
	}
	if r := rd.response(); r.err != errUnsupportedContext || len(rd.b) != 0 {
		t.Errorf("response %+v with %d bytes of varbinds", r, len(rd.b))
	}
}

// The GetBulk cap counts every varbind, also within one round (B23).
func TestBulkCapWithinRound(t *testing.T) {
	rs := make([]searchRange, 1500)
	for i := range rs {
		rs[i] = searchRange{start: testOID("1.1")}
	}
	if n := len(bulk(newTestHandler(), rs, 0, 3)); n != maxBulkVarbinds {
		t.Errorf("%d varbinds, want %d", n, maxBulkVarbinds)
	}
}

// Truncated PDUs are errors, never panics or reads past the end (B25).
func TestTruncatedPDU(t *testing.T) {
	var e encoder
	e.b = append(e.b, ranges(searchRange{start: testOID("1.2.1.1.4.31")})...)
	e.octets([]byte("abcdef"))
	raw := packet(header{typ: pduGet, session: 1, packet: 1}, e.b)
	for n := 0; n < len(raw); n++ {
		h, d, err := readPDU(bytesReader(raw[:n]))
		if err == nil {
			t.Fatalf("%d of %d bytes: no error (header %+v, decoder %v)", n, len(raw), h, d)
		}
	}
	// A header whose length is right but whose octet string claims more
	// than the payload holds: the decoder reports it.
	var bad encoder
	bad.u32(1000) // octet string length far beyond the payload
	bad.u32(0)
	_, d, err := readPDU(bytesReader(packet(header{typ: pduGet}, bad.b)))
	if err != nil {
		t.Fatal(err)
	}
	if d.octets(); d.err == nil {
		t.Error("an oversized octet string was accepted")
	}
}
