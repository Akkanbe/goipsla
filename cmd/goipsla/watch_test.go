package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goipsla/internal/api/apitest"
)

func TestWatch(t *testing.T) {
	sock := apitest.Serve(t, apitest.NewProvider())
	out, errOut, err := runCtl(t, "--socket", sock, "watch", "--interval", "10ms", "--count", "2", "--tag", "wan")
	if err != nil || errOut != "" {
		t.Fatalf("err %v stderr %q", err, errOut)
	}
	frames := strings.Split(out, clearWatchScreen)
	if len(frames) != 3 || frames[0] != "" {
		t.Fatalf("want 2 frames, got %d:\n%q", len(frames)-1, out)
	}
	golden(t, "watch_frame", frames[1])
	if frames[1] != frames[2] {
		t.Error("frames differ")
	}
}

func TestWatchSurvivesDaemonDown(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "gone.sock")
	out, _, err := runCtl(t, "--socket", sock, "watch", "--interval", "10ms", "--count", "2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "error: cannot connect to") != 2 {
		t.Errorf("got %q", out)
	}
}
