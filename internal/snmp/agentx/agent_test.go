//declscope:namespace agentx

package agentx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goipsla/internal/clock"
)

// testSubtree is the subtree the test subagent registers.
var testSubtree = ParseOID("1.3.6.1.4.1.9.9.42")

// testOID is sub under testSubtree.
func testOID(sub string) OID { return testSubtree.Join(ParseOID(sub)...) }

// testHandler is a Handler over a fixed, sorted table whose sysUpTime is
// 6000 (one minute).
type testHandler struct{ rows []varbind }

func newTestHandler() *testHandler {
	return &testHandler{rows: []varbind{
		{testOID("1.1.1.0"), OctetString("2.2.0")},
		{testOID("1.1.4.0"), Integer(1000)},
		{testOID("1.2.1.1.4.11"), Integer(5)},
		{testOID("1.2.1.1.4.12"), Integer(8)},
		{testOID("1.2.1.1.4.31"), Integer(16)},
		{testOID("1.2.1.1.5.11"), Integer(5000)},
	}}
}

func (h *testHandler) Get(name OID) Value {
	for _, r := range h.rows {
		if r.name.Compare(name) == 0 {
			return r.val
		}
	}
	return NoSuchInstance
}

func (h *testHandler) Next(start OID, include bool, end OID) (OID, Value, bool) {
	for _, r := range h.rows {
		if c := r.name.Compare(start); c > 0 || (include && c == 0) {
			if len(end) > 0 && r.name.Compare(end) >= 0 {
				break
			}
			return r.name, r.val, true
		}
	}
	return nil, Value{}, false
}

func (h *testHandler) Uptime() uint32 { return 6000 }

// testLogger drops everything.
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// master is a fake AgentX master on one accepted connection.
type master struct {
	t    *testing.T
	conn net.Conn
	pkt  uint32
}

func (m *master) read() (header, *decoder) {
	m.t.Helper()
	if err := m.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		m.t.Fatal(err)
	}
	h, d, err := readPDU(m.conn)
	if err != nil {
		m.t.Fatalf("master read: %v", err)
	}
	return h, d
}

func (m *master) respond(h header, session uint32, errStatus uint16) {
	m.t.Helper()
	rh := header{typ: pduResponse, session: session, transaction: h.transaction, packet: h.packet}
	if _, err := m.conn.Write(packet(rh, responsePayload(0, errStatus, 0, nil))); err != nil {
		m.t.Fatal(err)
	}
}

// handshake answers Open and Register.
func (m *master) handshake(session uint32) {
	m.t.Helper()
	h, d := m.read()
	if h.typ != pduOpen {
		m.t.Fatalf("expected Open, got %d", h.typ)
	}
	d.u32()
	id, _ := d.oid()
	descr := d.octets()
	if id.String() != testSubtree.String() || string(descr) != "goipslad test" {
		m.t.Errorf("open id %s descr %q", id, descr)
	}
	m.respond(h, session, errNone)
	h, d = m.read()
	if h.typ != pduRegister || h.session != session {
		m.t.Fatalf("expected Register for session %d, got type %d session %d", session, h.typ, h.session)
	}
	d.u32()
	subtree, _ := d.oid()
	if subtree.String() != testSubtree.String() {
		m.t.Errorf("registered %s", subtree)
	}
	m.respond(h, session, errNone)
}

// ask sends a request and returns the response's varbinds and error.
func (m *master) ask(session uint32, typ uint8, payload []byte) (response, []varbind) {
	m.t.Helper()
	m.pkt++
	if _, err := m.conn.Write(packet(header{typ: typ, session: session, transaction: m.pkt, packet: m.pkt}, payload)); err != nil {
		m.t.Fatal(err)
	}
	h, d := m.read()
	if h.typ != pduResponse || h.packet != m.pkt {
		m.t.Fatalf("response type %d packet %d, want %d", h.typ, h.packet, m.pkt)
	}
	r := d.response()
	var vbs []varbind
	for len(d.b) > 0 {
		vbs = append(vbs, d.varbind())
	}
	return r, vbs
}

func ranges(rs ...searchRange) []byte {
	var e encoder
	for _, r := range rs {
		e.oid(r.start, r.include)
		e.oid(r.end, false)
	}
	return e.b
}

func testConfig(network, addr string) agentConfig {
	return agentConfig{network: network, address: addr, subtrees: []OID{testSubtree}, descr: "goipslad test", retry: 20 * time.Millisecond}
}

func TestSession(t *testing.T) {
	dir, _ := os.MkdirTemp("", "agentx")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "master")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	mb := newTestHandler()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runAgent(ctx, testConfig("unix", path), mb, clock.Real(), testLogger()) }()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	m := &master{t: t, conn: conn}
	m.handshake(77)

	// Get: two varbinds, one missing.
	r, vbs := m.ask(77, pduGet, ranges(searchRange{start: testOID("1.2.1.1.4.31")}, searchRange{start: testOID("1.2.1.1.4.99")}))
	if r.err != errNone || len(vbs) != 2 || vbs[0].val.String() != "INTEGER 16" || vbs[1].val.typ != typeNoSuchInstance {
		t.Errorf("get: %+v %v", r, vbs)
	}
	if r.sysUpTime != 6000 {
		t.Errorf("sysUpTime %d", r.sysUpTime)
	}

	// GetNext with an end bound and one past the end.
	_, vbs = m.ask(77, pduGetNext, ranges(
		searchRange{start: testOID("1.2.1.1.4"), end: testOID("1.2.1.1.5")},
		searchRange{start: testOID("1.2.1.1.4.31"), end: testOID("1.2.1.1.5")},
	))
	if len(vbs) != 2 || vbs[0].name.String() != testOID("1.2.1.1.4.11").String() || vbs[1].val.typ != typeEndOfMibView {
		t.Errorf("getnext: %v", vbs)
	}

	// GetBulk: 0 non-repeaters, 3 repetitions.
	var e encoder
	e.u16(0)
	e.u16(3)
	_, vbs = m.ask(77, pduGetBulk, append(e.b, ranges(searchRange{start: testOID("1.2.1.1.4")})...))
	if len(vbs) != 3 || vbs[2].name.String() != testOID("1.2.1.1.4.31").String() {
		t.Errorf("getbulk: %v", vbs)
	}

	// SET is refused.
	r, _ = m.ask(77, pduTestSet, nil)
	if r.err != errNotWritable {
		t.Errorf("testset error %d", r.err)
	}
	// Ping.
	if r, _ = m.ask(77, pduPing, nil); r.err != errNone {
		t.Errorf("ping error %d", r.err)
	}

	// The master goes away: the agent reconnects and registers again.
	conn.Close()
	conn2, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	m2 := &master{t: t, conn: conn2}
	m2.handshake(78)
	if _, vbs = m2.ask(78, pduGet, ranges(searchRange{start: testOID("1.1.4.0")})); len(vbs) != 1 || vbs[0].val.String() != "INTEGER 1000" {
		t.Errorf("after reconnect: %v", vbs)
	}

	// Shutdown: the agent sends Close.
	cancel()
	h, d := m2.read()
	if h.typ != pduClose || h.session != 78 || d.u8() != closeShutdown {
		t.Errorf("close: %+v", h)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAgent did not return")
	}
}

func TestSessionRegisterRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mb := newTestHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runAgent(ctx, testConfig("tcp", ln.Addr().String()), mb, clock.Real(), testLogger()) }()

	// First attempt: Open succeeds, Register is refused (duplicate
	// registration); the agent drops the connection and tries again.
	conn, _ := ln.Accept()
	m := &master{t: t, conn: conn}
	h, _ := m.read()
	m.respond(h, 5, errNone)
	h, _ = m.read()
	m.respond(h, 5, 263) // duplicateRegistration
	conn2, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	(&master{t: t, conn: conn2}).handshake(6)
	conn.Close()
	conn2.Close()
}

func TestRunNoMaster(t *testing.T) {
	// No master: Run keeps retrying and returns nil on cancel.
	dir, _ := os.MkdirTemp("", "agentx")
	defer os.RemoveAll(dir)
	mb := newTestHandler()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := runAgent(ctx, testConfig("unix", filepath.Join(dir, "none")), mb, clock.Real(), testLogger()); err != nil {
		t.Fatal(err)
	}
}

func TestBulk(t *testing.T) {
	m := newTestHandler()
	ranges := []searchRange{
		{start: testOID("1.1.1")},               // non-repeater
		{start: testOID("1.2.1.1.4")},           // repeater: 11, 12, 31, then Threshold.11
		{start: ParseOID("1.3.6.1.4.1.9.9.43")}, // repeater at the end: endOfMibView
	}
	vbs := bulk(m, ranges, 1, 4)
	var got []string
	for _, v := range vbs {
		if v.val.typ == typeEndOfMibView {
			got = append(got, "end")
			continue
		}
		got = append(got, v.name[len(testSubtree):].String())
	}
	want := []string{"1.1.1.0", "1.2.1.1.4.11", "end", "1.2.1.1.4.12", "end", "1.2.1.1.4.31", "end", "1.2.1.1.5.11", "end"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}
