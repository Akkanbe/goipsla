package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// parsedKind classifies a received ICMP message.
type parsedKind uint8

const (
	parsedOther parsedKind = iota // passed the filter but irrelevant
	//declscope:package // wire format used by the engine
	parsedEchoReply
	//declscope:package // wire format used by the engine
	parsedTimestampReply
	//declscope:package // wire format used by the engine
	parsedDestUnreach
	//declscope:package // wire format used by the engine
	parsedTimeExceeded
)

// parsedMsg is a received ICMP message reduced to what matching needs.
//
// For an Echo Reply, ID/Seq come from the reply's ICMP header and Data is the
// echoed data part. For a Timestamp Reply, Originate/Receive/Transmit hold
// its three timestamps. For an ICMP error, ID/Seq/Data describe the Echo or
// Timestamp Request quoted inside it (InnerTimestamp tells which; Data of a
// quoted Timestamp Request starts at its Originate), and InnerDst is that
// request's destination.
//
//declscope:package // wire format used by the engine
type parsedMsg struct {
	Kind     parsedKind
	Code     uint8
	ID       uint16
	Seq      uint16
	Data     []byte
	InnerDst netip.Addr

	Originate, Receive, Transmit uint32
	InnerTimestamp               bool
}

// Errors of the parser (after the IP header).
var (
	errParseShort    = errors.New("packet too short")
	errParseChecksum = errors.New("bad icmp checksum")
	errParseNotEcho  = errors.New("quoted packet is not our echo request")
)

// parseReply parses one packet read from a raw socket. For IPv4, pkt starts
// with the IP header; for IPv6 it starts with the ICMPv6 header.
//
//declscope:package // wire format used by the engine
func parseReply(v6 bool, pkt []byte) (parsedMsg, error) {
	if v6 {
		return parseICMPv6(pkt)
	}
	icmp, err := stripIPv4Header(pkt)
	if err != nil {
		return parsedMsg{}, err
	}
	return parseICMPv4(icmp)
}

func parseICMPv4(b []byte) (parsedMsg, error) {
	if len(b) < icmpHeaderLen {
		return parsedMsg{}, errParseShort
	}
	// The kernel does not verify ICMP checksums for raw sockets.
	if icmpChecksum(b) != 0 {
		return parsedMsg{}, errParseChecksum
	}
	m := parsedMsg{Code: b[1]}
	switch b[0] {
	case icmp4EchoReply:
		m.Kind = parsedEchoReply
		m.ID = binary.BigEndian.Uint16(b[4:])
		m.Seq = binary.BigEndian.Uint16(b[6:])
		m.Data = b[icmpHeaderLen:]
		return m, nil
	case icmp4TimestampReply:
		if len(b) < icmpTimestampLen {
			return parsedMsg{}, errParseShort
		}
		m.Kind = parsedTimestampReply
		m.ID = binary.BigEndian.Uint16(b[4:])
		m.Seq = binary.BigEndian.Uint16(b[6:])
		m.Originate = binary.BigEndian.Uint32(b[8:])
		m.Receive = binary.BigEndian.Uint32(b[12:])
		m.Transmit = binary.BigEndian.Uint32(b[16:])
		return m, nil
	case icmp4DestUnreach, icmp4TimeExceeded:
		if b[0] == icmp4DestUnreach {
			m.Kind = parsedDestUnreach
		} else {
			m.Kind = parsedTimeExceeded
		}
		return parseQuoted4(m, b[icmpHeaderLen:])
	default:
		m.Kind = parsedOther
		return m, nil
	}
}

// parseQuoted4 extracts our request from the datagram quoted in an ICMP
// error: the original IP header plus at least 8 bytes of its payload.
func parseQuoted4(m parsedMsg, q []byte) (parsedMsg, error) {
	if len(q) < ipv4MinHdrLen || q[0]>>4 != 4 {
		return m, errParseShort
	}
	ihl := int(q[0]&0x0f) * 4
	if ihl < ipv4MinHdrLen || len(q) < ihl+icmpHeaderLen {
		return m, errParseShort
	}
	if q[9] != protoICMP {
		return m, errParseNotEcho
	}
	m.InnerDst = netip.AddrFrom4([4]byte(q[16:20]))
	icmp := q[ihl:]
	if (icmp[0] != icmp4EchoRequest && icmp[0] != icmp4TimestampRequest) || icmp[1] != 0 {
		return m, errParseNotEcho
	}
	m.InnerTimestamp = icmp[0] == icmp4TimestampRequest
	m.ID = binary.BigEndian.Uint16(icmp[4:])
	m.Seq = binary.BigEndian.Uint16(icmp[6:])
	m.Data = icmp[icmpHeaderLen:]
	if tot := int(binary.BigEndian.Uint16(q[2:])); tot >= ihl+icmpHeaderLen && tot <= len(q) {
		m.Data = q[ihl+icmpHeaderLen : tot]
	}
	return m, nil
}

func parseICMPv6(b []byte) (parsedMsg, error) {
	if len(b) < icmpHeaderLen {
		return parsedMsg{}, errParseShort
	}
	m := parsedMsg{Code: b[1]}
	switch b[0] {
	case icmp6EchoReply:
		m.Kind = parsedEchoReply
		m.ID = binary.BigEndian.Uint16(b[4:])
		m.Seq = binary.BigEndian.Uint16(b[6:])
		m.Data = b[icmpHeaderLen:]
		return m, nil
	case icmp6DestUnreach, icmp6TimeExceeded:
		if b[0] == icmp6DestUnreach {
			m.Kind = parsedDestUnreach
		} else {
			m.Kind = parsedTimeExceeded
		}
		return parseQuoted6(m, b[icmpHeaderLen:])
	default:
		m.Kind = parsedOther
		return m, nil
	}
}

// parseQuoted6 extracts our Echo Request from the packet quoted in an ICMPv6
// error, skipping extension headers.
func parseQuoted6(m parsedMsg, q []byte) (parsedMsg, error) {
	// IPv6 extension headers that may precede the quoted ICMPv6 header.
	const (
		hopByHop = 0
		routing  = 43
		fragment = 44
		destOpts = 60
	)
	if len(q) < ipv6HdrLen || q[0]>>4 != 6 {
		return m, errParseShort
	}
	m.InnerDst = netip.AddrFrom16([16]byte(q[24:40]))
	payloadLen := int(binary.BigEndian.Uint16(q[4:]))
	next := q[6]
	rest := q[ipv6HdrLen:]
	if payloadLen <= len(rest) {
		rest = rest[:payloadLen]
	}
	for next != protoICMPv6 {
		switch next {
		case hopByHop, routing, destOpts:
			if len(rest) < 8 {
				return m, errParseShort
			}
			n := (int(rest[1]) + 1) * 8
			if len(rest) < n {
				return m, errParseShort
			}
			next, rest = rest[0], rest[n:]
		case fragment:
			if len(rest) < 8 {
				return m, errParseShort
			}
			// Only the first fragment carries the ICMPv6 header.
			if binary.BigEndian.Uint16(rest[2:])&0xfff8 != 0 {
				return m, errParseNotEcho
			}
			next, rest = rest[0], rest[8:]
		default:
			return m, errParseNotEcho
		}
	}
	if len(rest) < icmpHeaderLen {
		return m, errParseShort
	}
	if rest[0] != icmp6EchoRequest || rest[1] != 0 {
		return m, errParseNotEcho
	}
	m.ID = binary.BigEndian.Uint16(rest[4:])
	m.Seq = binary.BigEndian.Uint16(rest[6:])
	m.Data = rest[icmpHeaderLen:]
	return m, nil
}

// errorDetail formats Reply.Detail for an ICMP error received from from.
//
//declscope:package // the engine reports it as Reply.Detail
func (m parsedMsg) errorDetail(v6 bool, from netip.Addr) string {
	var kind string
	if m.Kind == parsedTimeExceeded {
		kind = "time exceeded"
	} else {
		kind = "destination unreachable"
	}
	return fmt.Sprintf("%s: %s, from %s", kind, m.codeName(v6), from)
}

// codeName returns the RFC 792 / RFC 4443 name of the message's ICMP error
// code in lower case, or "code N" for codes without a name.
func (m parsedMsg) codeName(v6 bool) string {
	var names []string
	switch {
	case !v6 && m.Kind == parsedDestUnreach:
		names = []string{
			0:  "net unreachable",
			1:  "host unreachable",
			2:  "protocol unreachable",
			3:  "port unreachable",
			4:  "fragmentation needed",
			5:  "source route failed",
			6:  "destination network unknown",
			7:  "destination host unknown",
			8:  "source host isolated",
			9:  "network administratively prohibited",
			10: "host administratively prohibited",
			11: "network unreachable for tos",
			12: "host unreachable for tos",
			13: "communication administratively prohibited",
			14: "host precedence violation",
			15: "precedence cutoff in effect",
		}
	case !v6 && m.Kind == parsedTimeExceeded:
		names = []string{
			0: "ttl exceeded in transit",
			1: "fragment reassembly time exceeded",
		}
	case v6 && m.Kind == parsedDestUnreach:
		names = []string{
			0: "no route to destination",
			1: "communication with destination administratively prohibited",
			2: "beyond scope of source address",
			3: "address unreachable",
			4: "port unreachable",
			5: "source address failed ingress/egress policy",
			6: "reject route to destination",
			7: "error in source routing header",
		}
	case v6 && m.Kind == parsedTimeExceeded:
		names = []string{
			0: "hop limit exceeded in transit",
			1: "fragment reassembly time exceeded",
		}
	}
	if int(m.Code) < len(names) {
		return names[m.Code]
	}
	return fmt.Sprintf("code %d", m.Code)
}
