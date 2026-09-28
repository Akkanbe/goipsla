// A test of the operation list; it shares its namespace.
//
//declscope:namespace operations

package main

import (
	"testing"

	"goipsla/internal/api"
)

// TestLossCountsSentPackets: LOSS divides by the packets sent, as the result
// detail does; skipped packets are in neither count.
func TestLossCountsSentPackets(t *testing.T) {
	j := &api.JitterResultJSON{NumPackets: 10, Sent: 8, Skipped: 2, PktLoss: 1}
	if got := fmtOperationsLoss(j); got != "1/8" {
		t.Errorf("got %q, want 1/8", got)
	}
	if got := fmtOperationsLoss(nil); got != "-" {
		t.Errorf("echo: got %q, want -", got)
	}
}
