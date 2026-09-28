package probe

import (
	"bytes"
	"testing"
	"time"
)

func TestBuildPayload(t *testing.T) {
	sent := time.Unix(0, 0x0102030405060708)
	abcd := []byte{0xab, 0xcd, 0xab, 0xcd}
	p1234 := []byte{0x11, 0x22, 0x33, 0x44}
	for _, tc := range []struct {
		name     string
		dataSize int
		pattern  uint32
		wantLen  int    // 8 + request-data-size
		tail     []byte // expected bytes after the 20-byte header
	}{
		// Cisco default: 28 -> 36-byte data part -> 44-byte ICMP -> 64-byte IPv4.
		{"default", 28, 0xABCDABCD, 36, bytes.Repeat(abcd, 4)},
		{"truncated", 29, 0x11223344, 37, append(bytes.Repeat(p1234, 4), 0x11)},
		{"odd", 31, 0x11223344, 39, append(bytes.Repeat(p1234, 4), 0x11, 0x22, 0x33)},
		{"large", 1400, 0xABCDABCD, 1408, bytes.Repeat(abcd, 347)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := buildPayload(tc.dataSize, 0x0a0b0c0d, 0xfffffffe, tc.pattern, sent)
			if len(b) != tc.wantLen || payloadLen(tc.dataSize) != tc.wantLen {
				t.Fatalf("len = %d, payloadLen = %d, want %d", len(b), payloadLen(tc.dataSize), tc.wantLen)
			}
			head := []byte{0x49, 0x53, 0x4c, 0x41, 0x0a, 0x0b, 0x0c, 0x0d, 0xff, 0xff, 0xff, 0xfe,
				0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
			if !bytes.Equal(b[:20], head) {
				t.Errorf("header = % x", b[:20])
			}
			if !bytes.Equal(b[20:], tc.tail) {
				t.Errorf("pattern = % x, want % x", b[20:], tc.tail)
			}
			h, ok := parsePayloadHeader(b)
			if !ok || h.OpID != 0x0a0b0c0d || h.Seq != 0xfffffffe || h.SentNs != sent.UnixNano() {
				t.Errorf("parsed = %+v %v", h, ok)
			}
		})
	}
	if _, ok := parsePayloadHeader(make([]byte, 19)); ok {
		t.Error("short header accepted")
	}
	if _, ok := parsePayloadHeader(make([]byte, 20)); ok {
		t.Error("zero magic accepted")
	}
}

// TestOnWireSize pins the Cisco packet sizes: IPv4 is 36 + request-data-size
// (64 bytes by default) and the limits keep the datagram within 65535 bytes.
func TestOnWireSize(t *testing.T) {
	for _, size := range []int{MinDataSize, 100, 1400} {
		msg := buildICMPEcho(false, 1, 1, buildPayload(size, 1, 1, DefaultPattern, time.Unix(0, 0)))
		if got := ipv4MinHdrLen + len(msg); got != 36+size {
			t.Errorf("ipv4 packet for size %d = %d bytes, want %d", size, got, 36+size)
		}
		msg6 := buildICMPEcho(true, 1, 1, buildPayload(size, 1, 1, DefaultPattern, time.Unix(0, 0)))
		if got := ipv6HdrLen + len(msg6); got != 56+size {
			t.Errorf("ipv6 packet for size %d = %d bytes, want %d", size, got, 56+size)
		}
	}
	if got := ipv4MinHdrLen + icmpHeaderLen + payloadLen(MinDataSize); got != 64 {
		t.Errorf("default ipv4 packet = %d bytes, want 64", got)
	}
	if maxPayloadDataSize4 != 65499 || maxPayloadDataSize6 != 65519 {
		t.Errorf("limits = %d / %d, want 65499 / 65519", maxPayloadDataSize4, maxPayloadDataSize6)
	}
	if ipv4MinHdrLen+icmpHeaderLen+payloadLen(maxPayloadDataSize4) != 65535 ||
		icmpHeaderLen+payloadLen(maxPayloadDataSize6) != 65535 {
		t.Error("limits do not fill a 65535-byte datagram")
	}
}
