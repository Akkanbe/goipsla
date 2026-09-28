package agentx

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestOIDCompare(t *testing.T) {
	a, b := ParseOID("1.3.6.1.4"), ParseOID("1.3.6.1.4.1")
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 || a.Compare(a) != 0 {
		t.Error("prefix ordering")
	}
	if ParseOID("1.3.6.2").Compare(ParseOID("1.3.6.10")) >= 0 {
		t.Error("numeric ordering")
	}
	if !b.HasPrefix(a) || a.HasPrefix(b) {
		t.Error("hasPrefix")
	}
	if got := ParseOID(".1.3.6").String(); got != "1.3.6" {
		t.Errorf("String %q", got)
	}
}

func TestCodecRoundTrip(t *testing.T) {
	vbs := []varbind{
		{name: ParseOID("1.3.6.1.4.1.9.9.42.1.2.1.1.4.11"), val: Integer(1)},
		{name: ParseOID("1.3.6.1.4.1.9.9.42.1.1.1.0"), val: OctetString("abc")},   // needs padding
		{name: ParseOID("1.2.3"), val: Value{typ: typeCounter64, num: 1<<40 + 5}}, // no prefix compression
		{name: ParseOID("1.3.6.1.2.1.1.2.0"), val: Value{typ: typeObjectIdentifier, oid: ParseOID("1.3.6.1.4.1.9")}},
		{name: ParseOID("1.3.6.1.4.1.9.9.42.1.2.9.1.1.11"), val: TimeTicks(12345)},
		{name: ParseOID("1.3.6.1.4.1.9.9.42.1.2.1.1.5.11"), val: Integer(-7)},
		{name: ParseOID("1.3.6.1.4.1.9.9.42.1.2.1.1.99.11"), val: Value{typ: typeNoSuchObject}},
	}
	raw := packet(header{typ: pduResponse, session: 7, transaction: 8, packet: 9}, responsePayload(42, 0, 0, vbs))
	h, d, err := readPDU(bytesReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if h.typ != pduResponse || h.session != 7 || h.transaction != 8 || h.packet != 9 {
		t.Errorf("header %+v", h)
	}
	r := d.response()
	var got []varbind
	for len(d.b) > 0 {
		got = append(got, d.varbind())
	}
	if d.err != nil || r.sysUpTime != 42 {
		t.Fatalf("%v %+v", d.err, r)
	}
	opt := cmp.AllowUnexported(varbind{}, Value{})
	if diff := cmp.Diff(vbs, got, opt, cmp.Comparer(func(a, b []byte) bool { return string(a) == string(b) })); diff != "" {
		t.Error(diff)
	}
}

// bytesReader avoids importing bytes in several tests.
func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestDecodeLittleEndian(t *testing.T) {
	// A GetNext from a master that does not set NETWORK_BYTE_ORDER.
	le := binary.LittleEndian
	payload := []byte{}
	oidLE := func(prefix uint8, include uint8, subs ...uint32) {
		payload = append(payload, uint8(len(subs)), prefix, include, 0)
		for _, s := range subs {
			payload = le.AppendUint32(payload, s)
		}
	}
	oidLE(4, 1, 1, 9, 9, 42) // start 1.3.6.1.4.1.9.9.42, include
	oidLE(0, 0)              // end: null
	hdr := []byte{1, pduGetNext, 0, 0}
	hdr = le.AppendUint32(hdr, 3)
	hdr = le.AppendUint32(hdr, 4)
	hdr = le.AppendUint32(hdr, 5)
	hdr = le.AppendUint32(hdr, uint32(len(payload)))
	h, d, err := readPDU(bytesReader(append(hdr, payload...)))
	if err != nil {
		t.Fatal(err)
	}
	rs := d.searchRanges()
	if h.session != 3 || len(rs) != 1 || rs[0].start.String() != "1.3.6.1.4.1.9.9.42" || !rs[0].include || len(rs[0].end) != 0 {
		t.Errorf("%+v %+v", h, rs)
	}
}
