package probe

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestStripIPv4Header(t *testing.T) {
	icmp := []byte{0, 0, 0xff, 0xff, 0, 0, 0, 0}
	plain := ipv4Packet(target4, local4, icmp)
	withOpts := append([]byte(nil), plain[:20]...)
	withOpts[0] = 0x46 // IHL 6: one 4-byte option
	binary.BigEndian.PutUint16(withOpts[2:], uint16(24+len(icmp)))
	withOpts = append(withOpts, 1, 1, 1, 0)
	withOpts = append(withOpts, icmp...)
	padded := append(append([]byte(nil), plain...), 0, 0, 0, 0)
	udp := append([]byte(nil), plain...)
	udp[9] = 17

	for _, tc := range []struct {
		name string
		in   []byte
		want []byte
		err  error
	}{
		{"plain", plain, icmp, nil},
		{"options", withOpts, icmp, nil},
		{"padded", padded, icmp, nil},
		{"short", plain[:19], nil, errIPShort},
		{"ihl too small", append([]byte{0x44}, plain[1:]...), nil, errIPHeader},
		{"ihl beyond packet", append([]byte{0x4f}, plain[1:24]...), nil, errIPHeader},
		{"version", append([]byte{0x65}, plain[1:]...), nil, errIPHeader},
		{"not icmp", udp, nil, errIPNotICMP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripIPv4Header(tc.in)
			if err != tc.err || !bytes.Equal(got, tc.want) {
				t.Errorf("= % x, %v; want % x, %v", got, err, tc.want, tc.err)
			}
		})
	}
}
