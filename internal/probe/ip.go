package probe

import (
	"encoding/binary"
	"errors"
)

// IP header sizes.
//
//declscope:package // the parser skips IP headers with them; the payload limits and tests size packets with them
const (
	ipv4MinHdrLen = 20 // IPv4 header without options
	ipv6HdrLen    = 40 // IPv6 fixed header
)

// Errors of stripIPv4Header.
var (
	errIPShort   = errors.New("packet too short")
	errIPHeader  = errors.New("malformed ip header")
	errIPNotICMP = errors.New("not an icmp packet")
)

// stripIPv4Header returns the payload of an IPv4 packet as delivered by an
// IPv4 raw socket, checking that it carries ICMP.
//
//declscope:package // the parser strips the IP header of every IPv4 packet with it
func stripIPv4Header(b []byte) ([]byte, error) {
	if len(b) < ipv4MinHdrLen {
		return nil, errIPShort
	}
	if b[0]>>4 != 4 {
		return nil, errIPHeader
	}
	ihl := int(b[0]&0x0f) * 4
	if ihl < ipv4MinHdrLen || len(b) < ihl {
		return nil, errIPHeader
	}
	if b[9] != protoICMP {
		return nil, errIPNotICMP
	}
	// Trust the total length when it is sane: some paths pad short frames.
	if tot := int(binary.BigEndian.Uint16(b[2:])); tot >= ihl && tot <= len(b) {
		b = b[:tot]
	}
	return b[ihl:], nil
}
