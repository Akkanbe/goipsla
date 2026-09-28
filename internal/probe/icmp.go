package probe

import (
	"encoding/binary"
	"time"
)

// ICMP types, sizes and protocol numbers.
//
//declscope:package // ICMP protocol numbers: the vocabulary of the whole package and its tests
const (
	icmp4EchoReply        = 0
	icmp4DestUnreach      = 3
	icmp4EchoRequest      = 8
	icmp4TimeExceeded     = 11
	icmp4TimestampRequest = 13
	icmp4TimestampReply   = 14

	icmp6DestUnreach  = 1
	icmp6TimeExceeded = 3
	icmp6EchoRequest  = 128
	icmp6EchoReply    = 129

	// icmpHeaderLen is the size of the ICMP header (type, code, checksum,
	// identifier, sequence).
	icmpHeaderLen = 8
	// icmpTimestampLen is the fixed size of an ICMP Timestamp message
	// (RFC 792).
	icmpTimestampLen = 20

	protoICMP   = 1
	protoICMPv6 = 58
)

// buildICMPEcho returns an ICMP (v6=false) or ICMPv6 Echo Request carrying
// data. The IPv4 checksum is computed here; the ICMPv6 checksum is left zero
// for the kernel, which always fills it on ICMPv6 raw sockets.
//
//declscope:package // wire format used by the engine
func buildICMPEcho(v6 bool, id, seq uint16, data []byte) []byte {
	b := make([]byte, icmpHeaderLen+len(data))
	if v6 {
		b[0] = icmp6EchoRequest
	} else {
		b[0] = icmp4EchoRequest
	}
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	copy(b[icmpHeaderLen:], data)
	if !v6 {
		binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	}
	return b
}

// buildICMPTimestamp returns an ICMP Timestamp Request with the Originate
// Timestamp orig and zero Receive and Transmit.
//
//declscope:package // wire format used by the engine
func buildICMPTimestamp(id, seq uint16, orig uint32) []byte {
	b := make([]byte, icmpTimestampLen)
	b[0] = icmp4TimestampRequest
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	binary.BigEndian.PutUint32(b[8:], orig)
	binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	return b
}

// icmpTimestampMs is the RFC 792 timestamp of t: milliseconds since midnight
// UT.
//
//declscope:package // wire format used by the engine
func icmpTimestampMs(t time.Time) uint32 {
	const msPerDay = 86_400_000 // the range of an RFC 792 timestamp
	ms := t.UnixMilli() % msPerDay
	if ms < 0 {
		ms += msPerDay
	}
	return uint32(ms)
}

// icmpChecksum is the Internet checksum (RFC 1071) of an ICMP message. A
// message whose checksum field is correct sums to zero.
//
//declscope:package // the parser and the engine tests check and forge messages with it
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// acceptedICMPTypes lists the ICMP types parseReply handles, for the
// socket's receive filter: Echo Reply, Destination Unreachable and Time
// Exceeded, plus Timestamp Reply for IPv4.
//
//declscope:package // the raw socket's ICMP filter passes exactly these
func acceptedICMPTypes(v6 bool) []int {
	if v6 {
		return []int{icmp6EchoReply, icmp6DestUnreach, icmp6TimeExceeded}
	}
	return []int{icmp4EchoReply, icmp4DestUnreach, icmp4TimeExceeded, icmp4TimestampReply}
}
