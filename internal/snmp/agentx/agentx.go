// Package agentx is an AgentX subagent (RFC 2741) for read-only MIB views:
// it opens a session with the master agent (net-snmp's snmpd), registers
// subtrees and answers Get, GetNext and GetBulk through a Handler. SET is
// refused. RunSubagent keeps the session, reconnecting when it drops.
package agentx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// This file is the AgentX wire format (RFC 2741) for the subset goipslad
// needs: the header, object identifiers, octet strings, varbinds and search
// ranges, and the Open, Register, Close, Response, Get, GetNext, GetBulk and
// TestSet PDUs. We always send in network byte order and read either order.

// PDU types (RFC 2741 6.1).
const (
	pduOpen       = 1
	pduClose      = 2
	pduRegister   = 3
	pduUnregister = 4
	pduGet        = 5
	pduGetNext    = 6
	pduGetBulk    = 7
	pduTestSet    = 8
	pduCommitSet  = 9
	pduUndoSet    = 10
	pduCleanupSet = 11
	pduNotify     = 12
	pduPing       = 13
	pduResponse   = 18
)

// Header flags.
const (
	flagNonDefaultContext = 0x08
	flagNetworkByteOrder  = 0x10
)

// Response errors (RFC 2741 6.2.16) and the SNMP error used in responses.
const (
	errNone               = 0
	errNotWritable        = 17
	errUnsupportedContext = 262
	errParse              = 266
	errProcessing         = 268
)

// errNames name the response errors of RFC 2741 6.2.16 for the logs.
var errNames = map[uint16]string{
	errNotWritable: "notWritable",
	256:            "openFailed",
	257:            "notOpen",
	258:            "indexWrongType",
	259:            "indexAlreadyAllocated",
	260:            "indexNoneAvailable",
	261:            "indexNotAllocated",
	262:            "unsupportedContext",
	263:            "duplicateRegistration",
	264:            "unknownRegistration",
	265:            "unknownAgentCaps",
	266:            "parseError",
	267:            "requestDenied",
	268:            "processingError",
}

// errName is the RFC 2741 name of a response error, or "error".
func errName(code uint16) string {
	if n, ok := errNames[code]; ok {
		return n
	}
	return "error"
}

// Close reasons.
const (
	closeShutdown = 5
)

const headerSize = 20

// varType is a varbind type (RFC 2741 5.4).
type varType uint16

// Varbind types the MIB tree answers with.
const (
	typeInteger        varType = 2
	typeOctetString    varType = 4
	typeCounter32      varType = 65
	typeGauge32        varType = 66
	typeTimeTicks      varType = 67
	typeNoSuchObject   varType = 128
	typeNoSuchInstance varType = 129
)

// Varbind types only the codec handles.
const (
	typeNull             varType = 5
	typeObjectIdentifier varType = 6
	typeIPAddress        varType = 64
	typeOpaque           varType = 68
	typeCounter64        varType = 70
	typeEndOfMibView     varType = 130
)

// OID is an object identifier.
type OID []uint32

// ParseOID parses a dotted OID; it panics on a malformed constant.
func ParseOID(s string) OID {
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ".")
	o := make(OID, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			panic(fmt.Sprintf("agentx: bad OID %q", s))
		}
		o[i] = uint32(n)
	}
	return o
}

func (o OID) String() string {
	var b strings.Builder
	for i, n := range o {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.FormatUint(uint64(n), 10))
	}
	return b.String()
}

// Compare orders OIDs lexicographically.
func (o OID) Compare(p OID) int {
	for i := 0; i < len(o) && i < len(p); i++ {
		switch {
		case o[i] < p[i]:
			return -1
		case o[i] > p[i]:
			return 1
		}
	}
	switch {
	case len(o) < len(p):
		return -1
	case len(o) > len(p):
		return 1
	}
	return 0
}

// HasPrefix reports whether p is a prefix of o.
func (o OID) HasPrefix(p OID) bool {
	if len(p) > len(o) {
		return false
	}
	for i := range p {
		if o[i] != p[i] {
			return false
		}
	}
	return true
}

// Join returns o followed by the sub-identifiers.
func (o OID) Join(sub ...uint32) OID {
	r := make(OID, 0, len(o)+len(sub))
	return append(append(r, o...), sub...)
}

// Value is the Value of a varbind.
type Value struct {
	typ varType
	num uint64 // Integer (as int32 bits), Counter32, Gauge32, TimeTicks, Counter64
	str []byte // OctetString, IpAddress, Opaque
	// oid is the value of an ObjectIdentifier.
	oid OID
}

// varbind is a name and its value.
type varbind struct {
	name OID
	val  Value
}

// searchRange is one range of a Get, GetNext or GetBulk.
type searchRange struct {
	start   OID
	include bool
	end     OID
}

// header is the fixed 20-byte PDU header.
type header struct {
	typ         uint8
	flags       uint8
	session     uint32
	transaction uint32
	packet      uint32
	length      uint32
}

// ---- encoding (always network byte order) ----

type encoder struct{ b []byte }

func (e *encoder) u8(v uint8)   { e.b = append(e.b, v) }
func (e *encoder) u16(v uint16) { e.b = binary.BigEndian.AppendUint16(e.b, v) }
func (e *encoder) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *encoder) u64(v uint64) { e.b = binary.BigEndian.AppendUint64(e.b, v) }

func (e *encoder) oid(o OID, include bool) {
	// Use the 1.3.6.1.x prefix compression when it applies.
	prefix, rest := uint8(0), o
	if len(o) >= 5 && o[0] == 1 && o[1] == 3 && o[2] == 6 && o[3] == 1 && o[4] > 0 && o[4] < 256 {
		prefix, rest = uint8(o[4]), o[5:]
	}
	e.u8(uint8(len(rest)))
	e.u8(prefix)
	if include {
		e.u8(1)
	} else {
		e.u8(0)
	}
	e.u8(0)
	for _, n := range rest {
		e.u32(n)
	}
}

func (e *encoder) octets(s []byte) {
	e.u32(uint32(len(s)))
	e.b = append(e.b, s...)
	for len(e.b)%4 != 0 {
		e.b = append(e.b, 0)
	}
}

func (e *encoder) varbind(v varbind) {
	e.u16(uint16(v.val.typ))
	e.u16(0)
	e.oid(v.name, false)
	switch v.val.typ {
	case typeInteger, typeCounter32, typeGauge32, typeTimeTicks:
		e.u32(uint32(v.val.num))
	case typeCounter64:
		e.u64(v.val.num)
	case typeOctetString, typeIPAddress, typeOpaque:
		e.octets(v.val.str)
	case typeObjectIdentifier:
		e.oid(v.val.oid, false)
	}
}

// packet returns the header and payload as one PDU.
func packet(h header, payload []byte) []byte {
	e := encoder{b: make([]byte, 0, headerSize+len(payload))}
	e.u8(1) // version
	e.u8(h.typ)
	e.u8(h.flags | flagNetworkByteOrder)
	e.u8(0)
	e.u32(h.session)
	e.u32(h.transaction)
	e.u32(h.packet)
	e.u32(uint32(len(payload)))
	return append(e.b, payload...)
}

func openPayload(timeout uint8, id OID, descr string) []byte {
	var e encoder
	e.u8(timeout)
	e.u8(0)
	e.u8(0)
	e.u8(0)
	e.oid(id, false)
	e.octets([]byte(descr))
	return e.b
}

func registerPayload(timeout, priority uint8, subtree OID) []byte {
	var e encoder
	e.u8(timeout)
	e.u8(priority)
	e.u8(0) // range_subid
	e.u8(0)
	e.oid(subtree, false)
	return e.b
}

func closePayload(reason uint8) []byte {
	return []byte{reason, 0, 0, 0}
}

func responsePayload(sysUpTime uint32, errStatus, errIndex uint16, vbs []varbind) []byte {
	var e encoder
	e.u32(sysUpTime)
	e.u16(errStatus)
	e.u16(errIndex)
	for _, v := range vbs {
		e.varbind(v)
	}
	return e.b
}

// ---- decoding ----

var errShort = errors.New("agentx: truncated PDU")

type decoder struct {
	b     []byte
	order binary.ByteOrder
	err   error
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if len(d.b) < n {
		d.err = errShort
		return nil
	}
	p := d.b[:n]
	d.b = d.b[n:]
	return p
}

func (d *decoder) u8() uint8 {
	if p := d.take(1); p != nil {
		return p[0]
	}
	return 0
}

func (d *decoder) u16() uint16 {
	if p := d.take(2); p != nil {
		return d.order.Uint16(p)
	}
	return 0
}

func (d *decoder) u32() uint32 {
	if p := d.take(4); p != nil {
		return d.order.Uint32(p)
	}
	return 0
}

func (d *decoder) u64() uint64 {
	if p := d.take(8); p != nil {
		return d.order.Uint64(p)
	}
	return 0
}

func (d *decoder) oid() (OID, bool) {
	n := int(d.u8())
	prefix := d.u8()
	include := d.u8() != 0
	d.u8()
	if d.err != nil {
		return nil, false
	}
	o := make(OID, 0, n+5)
	if prefix != 0 {
		o = append(o, 1, 3, 6, 1, uint32(prefix))
	}
	for i := 0; i < n; i++ {
		o = append(o, d.u32())
	}
	return o, include
}

func (d *decoder) octets() []byte {
	n := int(d.u32())
	if d.err != nil {
		return nil
	}
	if n > len(d.b) {
		d.err = errShort
		return nil
	}
	s := append([]byte(nil), d.take(n)...)
	if pad := (4 - n%4) % 4; pad > 0 {
		d.take(pad)
	}
	return s
}

func (d *decoder) varbind() varbind {
	t := varType(d.u16())
	d.u16()
	name, _ := d.oid()
	v := varbind{name: name, val: Value{typ: t}}
	switch t {
	case typeInteger, typeCounter32, typeGauge32, typeTimeTicks:
		v.val.num = uint64(d.u32())
	case typeCounter64:
		v.val.num = d.u64()
	case typeOctetString, typeIPAddress, typeOpaque:
		v.val.str = d.octets()
	case typeObjectIdentifier:
		v.val.oid, _ = d.oid()
	}
	return v
}

func (d *decoder) searchRanges() []searchRange {
	var rs []searchRange
	for len(d.b) > 0 && d.err == nil {
		start, include := d.oid()
		end, _ := d.oid()
		rs = append(rs, searchRange{start: start, include: include, end: end})
	}
	return rs
}

// readPDU reads one PDU: its header and a decoder positioned at the payload
// (after the context, if any).
func readPDU(r io.Reader) (header, *decoder, error) {
	var hb [headerSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return header{}, nil, err
	}
	if hb[0] != 1 {
		return header{}, nil, fmt.Errorf("agentx: unsupported version %d", hb[0])
	}
	var order binary.ByteOrder = binary.LittleEndian
	if hb[2]&flagNetworkByteOrder != 0 {
		order = binary.BigEndian
	}
	h := header{
		typ:         hb[1],
		flags:       hb[2],
		session:     order.Uint32(hb[4:]),
		transaction: order.Uint32(hb[8:]),
		packet:      order.Uint32(hb[12:]),
		length:      order.Uint32(hb[16:]),
	}
	if h.length > 1<<20 || h.length%4 != 0 {
		return h, nil, fmt.Errorf("agentx: bad payload length %d", h.length)
	}
	payload := make([]byte, h.length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return h, nil, err
	}
	d := &decoder{b: payload, order: order}
	if h.flags&flagNonDefaultContext != 0 && h.typ != pduResponse {
		d.octets() // context: goipslad serves the default context only
	}
	return h, d, nil
}

// response is a decoded Response PDU.
type response struct {
	sysUpTime uint32
	err       uint16
	index     uint16
}

func (d *decoder) response() response {
	return response{sysUpTime: d.u32(), err: d.u16(), index: d.u16()}
}

// ---- value constructors ----

// Integer is an Integer32.
func Integer(n int64) Value {
	return Value{typ: typeInteger, num: uint64(uint32(int32(n)))}
}

// ClampInt is an Integer32 clamped to 0..2147483647.
func ClampInt(n uint64) Value {
	if n > 2147483647 {
		n = 2147483647
	}
	return Integer(int64(n))
}

// Gauge is a Gauge32, saturating at 2^32-1.
func Gauge(n uint64) Value {
	if n > 0xFFFFFFFF {
		n = 0xFFFFFFFF
	}
	return Value{typ: typeGauge32, num: n}
}

// Counter32 is a Counter32, wrapping at 2^32.
func Counter32(n uint64) Value { return Value{typ: typeCounter32, num: n & 0xFFFFFFFF} }

// TimeTicks is a TimeTicks (hundredths of a second).
func TimeTicks(n uint32) Value { return Value{typ: typeTimeTicks, num: uint64(n)} }

// Octets is an OCTET STRING.
func Octets(b []byte) Value { return Value{typ: typeOctetString, str: b} }

// OctetString is an OCTET STRING holding text.
func OctetString(s string) Value { return Octets([]byte(s)) }

// Truth is a TruthValue: true(1) or false(2).
func Truth(b bool) Value {
	if b {
		return Integer(1)
	}
	return Integer(2)
}

// The exceptions a Get answers with (RFC 2741 5.4).
var (
	NoSuchObject   = Value{typ: typeNoSuchObject}
	NoSuchInstance = Value{typ: typeNoSuchInstance}
)

// Exception reports whether v is noSuchObject, noSuchInstance or
// endOfMibView rather than a value.
func (v Value) Exception() bool { return v.typ >= typeNoSuchObject }

// String renders v as net-snmp's tools print it: "INTEGER 5",
// "STRING \"wan\"", "noSuchObject".
func (v Value) String() string {
	switch v.typ {
	case typeInteger:
		return fmt.Sprintf("INTEGER %d", int32(uint32(v.num)))
	case typeGauge32:
		return fmt.Sprintf("Gauge32 %d", v.num)
	case typeCounter32:
		return fmt.Sprintf("Counter32 %d", v.num)
	case typeCounter64:
		return fmt.Sprintf("Counter64 %d", v.num)
	case typeTimeTicks:
		return fmt.Sprintf("TimeTicks %d", v.num)
	case typeOctetString:
		return fmt.Sprintf("STRING %q", v.str)
	case typeObjectIdentifier:
		return "OID " + v.oid.String()
	case typeNoSuchObject:
		return "noSuchObject"
	case typeNoSuchInstance:
		return "noSuchInstance"
	case typeEndOfMibView:
		return "endOfMibView"
	}
	return fmt.Sprintf("type %d", v.typ)
}
