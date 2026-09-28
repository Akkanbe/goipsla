package main

import (
	"errors"
	"strings"
	"testing"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/config"
)

func TestReload(t *testing.T) {
	p := apitest.NewProvider()
	p.ReloadRes = &api.ReloadResult{
		Added: []int{5}, Removed: []int{4}, Restarted: []int{2, 3}, Updated: []int{},
		Warnings: []string{"global.api-socket changed; it requires a restart of goipslad to take effect"},
	}
	sock := apitest.Serve(t, p)

	out, errOut, err := runCtl(t, "--socket", sock, "reload")
	if err != nil || errOut != "" {
		t.Fatalf("err %v, stderr %q", err, errOut)
	}
	want := "added:      [5]\nremoved:    [4]\nrestarted:  [2 3]\nupdated:    []\n" +
		"warning: global.api-socket changed; it requires a restart of goipslad to take effect\n"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}

	out, _, err = runCtl(t, "--socket", sock, "reload", "-o", "json")
	if err != nil || !strings.Contains(out, `"restarted": [`) || !strings.Contains(out, `"warnings": [`) {
		t.Errorf("json: err %v, out %s", err, out)
	}
}

func TestReloadInvalid(t *testing.T) {
	p := apitest.NewProvider()
	p.ReloadErr = &config.ValidationError{Errors: []config.FieldError{
		{Path: "operations[0].timeout", Msg: "must be at least 1ms"},
		{Path: "operations[1].frequency", Msg: "must be greater than timeout"},
	}}
	sock := apitest.Serve(t, p)
	_, errOut, err := runCtl(t, "--socket", sock, "reload")
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("err %v, want exit 1", err)
	}
	want := "goipsla: reload failed; goipslad keeps the running configuration: invalid configuration:\n" +
		"  operations[0].timeout: must be at least 1ms\n" +
		"  operations[1].frequency: must be greater than timeout\n"
	if errOut != want {
		t.Errorf("stderr\n%s\nwant\n%s", errOut, want)
	}
}
