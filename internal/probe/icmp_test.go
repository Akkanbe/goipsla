package probe

import (
	"testing"
	"time"
)

func TestChecksum(t *testing.T) {
	// RFC 1071 section 3 example: 0001 f203 f4f5 f6f7 sums to ddf2.
	b := []byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}
	if got := icmpChecksum(b); got != ^uint16(0xddf2) {
		t.Errorf("checksum = %#04x, want %#04x", got, ^uint16(0xddf2))
	}
	if got := icmpChecksum([]byte{0xff}); got != ^uint16(0xff00) {
		t.Errorf("odd checksum = %#04x", got)
	}
	msg := buildICMPEcho(false, 0x1234, 0x0001, buildPayload(28, 1, 1, DefaultPattern, time.Unix(0, 0)))
	if icmpChecksum(msg) != 0 {
		t.Error("built echo does not verify")
	}
}
