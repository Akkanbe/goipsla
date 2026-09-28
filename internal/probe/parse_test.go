package probe

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

func TestParseReply(t *testing.T) {
	data := buildPayload(28, 7, 9, DefaultPattern, time.Unix(0, 0))
	req4 := sentPkt{msg: buildICMPEcho(false, 0x77, 9, data), dst: target4}
	req6 := sentPkt{msg: buildICMPEcho(true, 0x77, 9, data), dst: target6}

	// IPv6 error quoting a packet with a hop-by-hop and a fragment header.
	ext := func() []byte {
		inner := append([]byte{
			44, 0, 1, 4, 0, 0, 0, 0, // hop-by-hop (next: fragment 44), 8 bytes (PadN)
			protoICMPv6, 0, 0, 0, 0, 0, 0, 1, // first fragment, M=1
		}, req6.msg...)
		q := make([]byte, 40, 40+len(inner))
		q[0] = 0x60
		binary.BigEndian.PutUint16(q[4:], uint16(len(inner)))
		q[6] = 0 // hop-by-hop
		d := target6.As16()
		copy(q[24:], d[:])
		q = append(q, inner...)
		return append([]byte{icmp6TimeExceeded, 0, 0, 0, 0, 0, 0, 0}, q...)
	}()

	for _, tc := range []struct {
		name string
		v6   bool
		pkt  []byte
		kind parsedKind
		code uint8
		dst  netip.Addr
		data int // expected len(Data)
	}{
		{"v4 reply", false, echoReply(req4, nil), parsedEchoReply, 0, netip.Addr{}, 36},
		{"v4 unreach full", false, icmpError(req4, icmp4DestUnreach, 3, router4, -1), parsedDestUnreach, 3, target4, 36},
		{"v4 unreach 8 bytes", false, icmpError(req4, icmp4DestUnreach, 1, router4, 8), parsedDestUnreach, 1, target4, 0},
		{"v4 time exceeded", false, icmpError(req4, icmp4TimeExceeded, 0, router4, -1), parsedTimeExceeded, 0, target4, 36},
		{"v4 timestamp reply", false, ipv4Packet(target4, local4, timestampReplyToParse(0x77, 9, 1, 2, 3)), parsedTimestampReply, 0, netip.Addr{}, 0},
		{"v6 reply", true, echoReply(req6, nil), parsedEchoReply, 0, netip.Addr{}, 36},
		{"v6 unreach", true, icmpError(req6, icmp6DestUnreach, 4, router6, -1), parsedDestUnreach, 4, target6, 36},
		{"v6 ext headers", true, ext, parsedTimeExceeded, 0, target6, 36},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseReply(tc.v6, tc.pkt)
			if err != nil {
				t.Fatal(err)
			}
			if m.Kind != tc.kind || m.Code != tc.code || m.InnerDst != tc.dst || len(m.Data) != tc.data {
				t.Fatalf("parsed = %+v", m)
			}
			if tc.kind != parsedOther && (m.ID != 0x77 || m.Seq != 9) {
				t.Errorf("id/seq = %#x/%d", m.ID, m.Seq)
			}
		})
	}
}

func TestParseReplyErrors(t *testing.T) {
	data := buildPayload(28, 7, 9, DefaultPattern, time.Unix(0, 0))
	req4 := sentPkt{msg: buildICMPEcho(false, 0x77, 9, data), dst: target4}
	notEcho := sentPkt{msg: append([]byte(nil), req4.msg...), dst: target4}
	notEcho.msg[0] = icmp4EchoReply
	for _, tc := range []struct {
		name string
		v6   bool
		pkt  []byte
		err  error
	}{
		{"v4 short icmp", false, ipv4Packet(target4, local4, []byte{0, 0, 0}), errParseShort},
		{"v4 bad checksum", false, ipv4Packet(target4, local4, []byte{0, 0, 1, 2, 0, 0, 0, 0}), errParseChecksum},
		{"v4 quoted too short", false, icmpError(req4, icmp4DestUnreach, 1, router4, 4), errParseShort},
		{"v4 quoted not echo", false, icmpError(notEcho, icmp4DestUnreach, 1, router4, -1), errParseNotEcho},
		{"v6 short", true, []byte{129, 0, 0}, errParseShort},
		{"v6 quoted short", true, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0x60}, errParseShort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseReply(tc.v6, tc.pkt); err != tc.err {
				t.Errorf("err = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestCodeName(t *testing.T) {
	for _, tc := range []struct {
		v6   bool
		kind parsedKind
		code uint8
		want string
	}{
		{false, parsedDestUnreach, 0, "net unreachable"},
		{false, parsedDestUnreach, 1, "host unreachable"},
		{false, parsedDestUnreach, 3, "port unreachable"},
		{false, parsedDestUnreach, 4, "fragmentation needed"},
		{false, parsedDestUnreach, 13, "communication administratively prohibited"},
		{false, parsedDestUnreach, 16, "code 16"},
		{false, parsedTimeExceeded, 1, "fragment reassembly time exceeded"},
		{true, parsedDestUnreach, 0, "no route to destination"},
		{true, parsedDestUnreach, 1, "communication with destination administratively prohibited"},
		{true, parsedDestUnreach, 3, "address unreachable"},
		{true, parsedTimeExceeded, 0, "hop limit exceeded in transit"},
		{true, parsedTimeExceeded, 9, "code 9"},
	} {
		if got := (parsedMsg{Kind: tc.kind, Code: tc.code}).codeName(tc.v6); got != tc.want {
			t.Errorf("codeName(%v, %d, %d) = %q, want %q", tc.v6, tc.kind, tc.code, got, tc.want)
		}
	}
}

func FuzzParseReply(f *testing.F) {
	data := buildPayload(28, 7, 9, DefaultPattern, time.Unix(0, 0))
	req4 := sentPkt{msg: buildICMPEcho(false, 0x77, 9, data), dst: target4}
	req6 := sentPkt{msg: buildICMPEcho(true, 0x77, 9, data), dst: target6}
	f.Add(false, echoReply(req4, nil))
	f.Add(false, icmpError(req4, icmp4DestUnreach, 1, router4, -1))
	f.Add(false, icmpError(req4, icmp4TimeExceeded, 0, router4, 8))
	f.Add(true, echoReply(req6, nil))
	f.Add(true, icmpError(req6, icmp6DestUnreach, 3, router6, -1))
	f.Add(false, []byte{0x4f})
	f.Add(true, []byte{})
	f.Fuzz(func(t *testing.T, v6 bool, pkt []byte) {
		m, err := parseReply(v6, pkt)
		if err != nil {
			return
		}
		switch m.Kind {
		case parsedEchoReply, parsedOther:
			if m.InnerDst.IsValid() {
				t.Errorf("inner dst set for %v", m.Kind)
			}
		case parsedDestUnreach, parsedTimeExceeded:
			if !m.InnerDst.IsValid() || m.InnerDst.Is4() == v6 {
				t.Errorf("inner dst %v for v6=%v", m.InnerDst, v6)
			}
			_ = m.errorDetail(v6, router4)
		}
		_, _ = parsePayloadHeader(m.Data)
	})
}

// timestampReplyToParse returns an ICMP Timestamp Reply with the given
// identifier, sequence and timestamps and a correct checksum.
func timestampReplyToParse(id, seq uint16, orig, recv, xmit uint32) []byte {
	b := buildICMPTimestamp(id, seq, orig)
	b[0] = icmp4TimestampReply
	binary.BigEndian.PutUint32(b[12:], recv)
	binary.BigEndian.PutUint32(b[16:], xmit)
	b[2], b[3] = 0, 0
	binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	return b
}
