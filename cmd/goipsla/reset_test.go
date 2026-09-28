package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"goipsla/internal/api/apitest"
)

func TestReset(t *testing.T) {
	p := apitest.NewProvider()
	sock := apitest.Serve(t, p)

	// Not a terminal and no --yes: refused, nothing reset.
	var ee *exitError
	_, errOut, err := runCtl(t, "--socket", sock, "reset")
	if !errors.As(err, &ee) || ee.Code != 2 || !strings.Contains(errOut, "reset needs --yes when standard input is not a terminal") {
		t.Errorf("reset without --yes: err %v, stderr %q", err, errOut)
	}
	out, _, err := runCtl(t, "--socket", sock, "reset", "--yes")
	if err != nil || out != "all operations reset\n" {
		t.Errorf("reset --yes: err %v, out %q", err, out)
	}
	if n := strings.Count(strings.Join(p.Calls, ","), "reset"); n != 1 {
		t.Errorf("provider saw %d resets, want 1 (calls %v)", n, p.Calls)
	}

	// Interactive: the answer decides.
	defer func(f func(io.Reader) bool) { isTerminalForReset = f }(isTerminalForReset)
	isTerminalForReset = func(io.Reader) bool { return true }
	for _, tt := range []struct {
		answer string
		ok     bool
	}{{"y\n", true}, {"YES\n", true}, {"n\n", false}, {"\n", false}} {
		cmd := newRootCmd()
		var o, e strings.Builder
		cmd.SetOut(&o)
		cmd.SetErr(&e)
		cmd.SetIn(strings.NewReader(tt.answer))
		cmd.SetArgs([]string{"--socket", sock, "reset"})
		err := cmd.Execute()
		if tt.ok != (err == nil) || !strings.Contains(e.String(), "[y/N]") {
			t.Errorf("answer %q: err %v, stderr %q", tt.answer, err, e.String())
		}
	}
}
