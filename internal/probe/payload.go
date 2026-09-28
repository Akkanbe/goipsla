package probe

import (
	"encoding/binary"
	"time"
)

// Payload layout (docs/probe-engine.md, "Echo data part").
const (
	payloadMagic = 0x49534C41 // "ISLA"
	//declscope:package // wire format used by the engine
	payloadHeaderLen = 20
	// payloadOverhead is the part of the data that Cisco counts outside
	// request-data-size ("8 (internal timestamps)" in CISCO-RTTMON-MIB
	// rttMonEchoAdminPktDataRequestSize): the data part of an Echo Request
	// is 8 + request-data-size bytes, so the default 28 makes a 64-byte
	// IPv4 packet.
	payloadOverhead = 8

	// Largest request-data-size that fits in one IP datagram without
	// jumbograms: 65499 for IPv4, 65519 for IPv6.
	//
	//declscope:package // wire format used by the engine
	maxPayloadDataSize4 = 65535 - ipv4MinHdrLen - icmpHeaderLen - payloadOverhead
	//declscope:package // wire format used by the engine
	maxPayloadDataSize6 = 65535 - icmpHeaderLen - payloadOverhead
)

// payloadHeader is the decoded 20-byte header at the start of the data part.
type payloadHeader struct {
	OpID   uint32
	Seq    uint32
	SentNs int64
}

// payloadLen returns the length of the ICMP data part for a
// request-data-size.
func payloadLen(dataSize int) int { return payloadOverhead + dataSize }

// buildPayload returns the data part of an Echo Request for request-data-size
// dataSize: payloadLen(dataSize) bytes holding the 20-byte header followed by
// pattern repeated big-endian and truncated at the end.
//
//declscope:package // wire format used by the engine
func buildPayload(dataSize int, opID, seq, pattern uint32, sent time.Time) []byte {
	b := make([]byte, payloadLen(dataSize))
	fillPayloadPattern(b[payloadHeaderLen:], pattern)
	putPayloadHeader(b, opID, seq, sent)
	return b
}

func fillPayloadPattern(b []byte, pattern uint32) {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], pattern)
	for i := range b {
		b[i] = p[i%4]
	}
}

//declscope:package // wire format used by the engine
func putPayloadHeader(b []byte, opID, seq uint32, sent time.Time) {
	binary.BigEndian.PutUint32(b[0:], payloadMagic)
	binary.BigEndian.PutUint32(b[4:], opID)
	binary.BigEndian.PutUint32(b[8:], seq)
	binary.BigEndian.PutUint64(b[12:], uint64(sent.UnixNano()))
}

// parsePayloadHeader decodes the header of an echoed data part. ok is false
// when the data is too short or the magic does not match.
//
//declscope:package // wire format used by the engine
func parsePayloadHeader(b []byte) (h payloadHeader, ok bool) {
	if len(b) < payloadHeaderLen || binary.BigEndian.Uint32(b) != payloadMagic {
		return payloadHeader{}, false
	}
	return payloadHeader{
		OpID:   binary.BigEndian.Uint32(b[4:]),
		Seq:    binary.BigEndian.Uint32(b[8:]),
		SentNs: int64(binary.BigEndian.Uint64(b[12:])),
	}, true
}
