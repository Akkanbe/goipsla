package op

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReturnCodeNames(t *testing.T) {
	tests := []struct {
		rc      ReturnCode
		value   int // RttResponseSense value in CISCO-RTTMON-TC-MIB
		mib     string
		display string
	}{
		{RCOther, 0, "other", "Unknown"},
		{RCOK, 1, "ok", "OK"},
		{RCDisconnected, 2, "disconnected", "Disconnected"},
		{RCOverThreshold, 3, "overThreshold", "Over Threshold"},
		{RCTimeout, 4, "timeout", "Timeout"},
		{RCBusy, 5, "busy", "Busy"},
		{RCNotConnected, 6, "notConnected", "Not Connected"},
		{RCDropped, 7, "dropped", "Dropped"},
		{RCSequenceError, 8, "sequenceError", "Sequence Error"},
		{RCVerifyError, 9, "verifyError", "Verify Error"},
		{RCApplicationSpecific, 10, "applicationSpecific", "Application Specific"},
		{RCError, 16, "error", "Internal Error"},
		{ReturnCode(13), 13, "ReturnCode(13)", "Unknown"},
		{ReturnCode(-1), -1, "ReturnCode(-1)", "Unknown"},
	}
	for _, tt := range tests {
		if int(tt.rc) != tt.value {
			t.Errorf("%s = %d, want %d", tt.mib, int(tt.rc), tt.value)
		}
		if got := tt.rc.String(); got != tt.mib {
			t.Errorf("ReturnCode(%d).String() = %q, want %q", tt.value, got, tt.mib)
		}
		if got := tt.rc.Display(); got != tt.display {
			t.Errorf("ReturnCode(%d).Display() = %q, want %q", tt.value, got, tt.display)
		}
	}
}

func TestReturnCodeJSON(t *testing.T) {
	type wrapper struct {
		RC ReturnCode `json:"rc"`
	}
	for rc := range returnCodeNames {
		b, err := json.Marshal(wrapper{rc})
		if err != nil {
			t.Fatalf("Marshal(%v): %v", rc, err)
		}
		if want := `{"rc":"` + rc.String() + `"}`; string(b) != want {
			t.Errorf("Marshal(%v) = %s, want %s", rc, b, want)
		}
		var got wrapper
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got.RC != rc {
			t.Errorf("round trip of %v gave %v", rc, got.RC)
		}
	}
	var w wrapper
	if err := json.Unmarshal([]byte(`{"rc":"nope"}`), &w); err == nil {
		t.Error("Unmarshal of an unknown label succeeded")
	}
}

func TestParseReturnCode(t *testing.T) {
	tests := []struct {
		in      string
		want    ReturnCode
		wantErr bool
	}{
		{"ok", RCOK, false},
		{"overThreshold", RCOverThreshold, false},
		{"overthreshold", RCOverThreshold, false},
		{"TIMEOUT", RCTimeout, false},
		{"sequenceError", RCSequenceError, false},
		{"error", RCError, false},
		{"Over Threshold", 0, true},
		{"4", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseReturnCode(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseReturnCode(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseReturnCode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestHasRTT(t *testing.T) {
	for rc := range returnCodeNames {
		if got, want := rc.HasRTT(), rc == RCOK || rc == RCOverThreshold; got != want {
			t.Errorf("%v.HasRTT() = %v, want %v", rc, got, want)
		}
	}
}

func TestRTTMillis(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want float64
	}{
		{0, 0},
		{1234567 * time.Nanosecond, 1.235}, // rounded to the microsecond
		{1234499 * time.Nanosecond, 1.234},
		{5 * time.Second, 5000},
		{-1500 * time.Nanosecond, -0.002},
	} {
		if got := RTTMillis(tt.d); got != tt.want {
			t.Errorf("RTTMillis(%v) = %v, want %v", tt.d, got, tt.want)
		}
	}
}
