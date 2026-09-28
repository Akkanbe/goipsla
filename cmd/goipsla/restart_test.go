package main

import (
	"errors"
	"testing"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
)

func TestRestart(t *testing.T) {
	p := apitest.NewProvider()
	sock := apitest.Serve(t, p)
	out, _, err := runCtl(t, "--socket", sock, "restart", "101")
	if err != nil || out != "operation 101 restarted\n" {
		t.Errorf("restart 101: err %v, out %q", err, out)
	}
	if _, _, err := runCtl(t, "--socket", sock, "restart", "999"); err == nil || !errors.Is(err, api.ErrNotFound) {
		t.Errorf("restart 999: err %v, want not found", err)
	}
	p.RestartErr = errors.New("operation 101 is pending: only an active operation can be restarted")
	if _, _, err := runCtl(t, "--socket", sock, "restart", "101"); err == nil || err.Error() != p.RestartErr.Error() {
		t.Errorf("restart pending: err %v", err)
	}
	var ee *exitError
	if _, _, err := runCtl(t, "--socket", sock, "restart"); !errors.As(err, &ee) || ee.Code != 2 {
		t.Errorf("restart without id: err %v, want exit 2", err)
	}
}
