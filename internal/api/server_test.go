package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/stats"
)

// rawGet sends a request over the socket and returns status and body.
func rawDo(t *testing.T, sock, method, path string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	req, _ := http.NewRequest(method, "http://x"+path, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("%s %s: content type %q", method, path, resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, string(b)
}

func errMsg(t *testing.T, body string) string {
	t.Helper()
	var e struct{ Error string }
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not an error body: %q", body)
	}
	return e.Error
}

func TestServerStatusAndErrors(t *testing.T) {
	p := apitest.NewProvider()
	p.ResetErr = errors.New("disk on fire")
	sock := apitest.Serve(t, p)
	tests := []struct {
		method, path string
		status       int
		errMsg       string // for error statuses
	}{
		{"GET", "/v1/health", 200, ""},
		{"GET", "/v1/operations", 200, ""},
		{"GET", "/v1/operations/101", 200, ""},
		{"GET", "/v1/operations/101?include=hours,history,enhanced", 200, ""},
		{"GET", "/v1/operations/999", 404, "operation 999 not found"},
		{"GET", "/v1/operations/abc", 400, `invalid operation id "abc": must be a positive integer`},
		{"GET", "/v1/operations/0", 400, `invalid operation id "0": must be a positive integer`},
		{"GET", "/v1/operations/101?include=hours,stats", 400, `unknown include "stats": must be hours, history or enhanced`},
		{"GET", "/v1/operations/101?details=1", 400, `unknown query parameter "details": must be include`},
		{"GET", "/v1/operations/101?include=hours&include=unknown", 400, `unknown include "unknown": must be hours, history or enhanced`},
		{"GET", "/v1/operations/101?include=hours,,history", 400, `empty element in include "hours,,history": must be hours, history or enhanced`},
		{"GET", "/v1/operations/101?include=", 400, `empty element in include "": must be hours, history or enhanced`},
		{"GET", "/v1/operations/101?include=hours&include=history", 200, ""},
		{"GET", "/v1/health?verbose=1", 400, `unknown query parameter "verbose": /v1/health takes no parameters`},
		{"POST", "/v1/reload?force=1", 400, `unknown query parameter "force": /v1/reload takes no parameters`},
		{"POST", "/v1/reset?all", 400, `unknown query parameter "all": /v1/reset takes no parameters`},
		{"POST", "/v1/operations/101/restart?now=1", 400, `unknown query parameter "now": /v1/operations/101/restart takes no parameters`},
		{"GET", "/v1/operations?tag=a&tag=b", 400, `query parameter "tag" given more than once`},
		{"GET", "/v1/operations?zeta=1&alpha=2", 400, `unknown query parameter "alpha": must be tag, state, rc, type or include`},
		{"GET", "/v1/operations?state=running", 400, `invalid state "running": must be pending, inactive or active`},
		{"GET", "/v1/operations?rc=fine", 400, `unknown return code "fine"`},
		{"GET", "/v1/operations?type=udp-jitter", 400, `invalid type "udp-jitter": must be icmp-echo or icmp-jitter`},
		{"GET", "/v1/operations?tags=wan", 400, `unknown query parameter "tags": must be tag, state, rc, type or include`},
		{"POST", "/v1/operations", 405, "method POST not allowed; use GET"},
		{"GET", "/v1/reload", 405, "method GET not allowed; use POST"},
		{"GET", "/v2/health", 404, "no such endpoint: /v2/health"},
		{"POST", "/v1/reload", 501, "not implemented"},
		{"POST", "/v1/operations/101/restart", 204, ""},
		{"POST", "/v1/operations/999/restart", 404, "operation 999 not found"},
		{"POST", "/v1/operations/x/restart", 400, `invalid operation id "x": must be a positive integer`},
		{"POST", "/v1/reset", 500, "disk on fire"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			status, body := rawDo(t, sock, tt.method, tt.path)
			if status != tt.status {
				t.Fatalf("status %d, want %d; body %s", status, tt.status, body)
			}
			if tt.errMsg != "" {
				if got := errMsg(t, body); got != tt.errMsg {
					t.Errorf("error %q, want %q", got, tt.errMsg)
				}
			}
		})
	}
}

func TestServerFilters(t *testing.T) {
	sock := apitest.Serve(t, apitest.NewProvider())
	tests := []struct {
		query string
		want  []int
	}{
		{"", []int{1, 101, 102, 103}},
		{"tag=wan", []int{101, 102}},
		{"state=pending", []int{103}},
		{"rc=timeout", []int{102}},
		{"rc=OK", []int{1, 101}}, // case-insensitive
		{"rc=other", []int{103}},
		{"type=icmp-jitter", []int{1}},
		{"tag=wan&rc=ok", []int{101}},
		{"tag=nope", []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			status, body := rawDo(t, sock, "GET", "/v1/operations?"+tt.query)
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			var rows []api.OperationRow
			if err := json.Unmarshal([]byte(body), &rows); err != nil {
				t.Fatal(err)
			}
			got := []int{}
			for _, r := range rows {
				got = append(got, r.ID)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Error(diff)
			}
			if len(rows) == 0 && strings.TrimSpace(body) != "[]" {
				t.Errorf("empty list must be [], got %s", body)
			}
		})
	}
}

func TestServerInclude(t *testing.T) {
	p := apitest.NewProvider()
	sock := apitest.Serve(t, p)
	for _, q := range []string{"", "?include=hours", "?include=history,enhanced", "?include=hours,history,enhanced", "?include=hours&include=enhanced"} {
		rawDo(t, sock, "GET", "/v1/operations/101"+q)
	}
	want := []string{
		fmt.Sprint("operation 101 ", stats.SnapshotOptions{}),
		fmt.Sprint("operation 101 ", stats.SnapshotOptions{Hours: true}),
		fmt.Sprint("operation 101 ", stats.SnapshotOptions{History: true, Enhanced: true}),
		fmt.Sprint("operation 101 ", stats.SnapshotOptions{Hours: true, History: true, Enhanced: true}),
		fmt.Sprint("operation 101 ", stats.SnapshotOptions{Hours: true, Enhanced: true}),
	}
	if diff := cmp.Diff(want, p.Calls); diff != "" {
		t.Error(diff)
	}
	// Omitted parts are absent from the JSON.
	_, body := rawDo(t, sock, "GET", "/v1/operations/101")
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"hours", "history", "enhanced"} {
		if _, ok := top[k]; ok {
			t.Errorf("%s present without include", k)
		}
	}
	_, body = rawDo(t, sock, "GET", "/v1/operations/101?include=hours")
	if !strings.Contains(body, `"hours"`) || !strings.Contains(body, `"upper_ms": null`) {
		t.Errorf("hours missing:\n%s", body)
	}
}

func TestSocketMode(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "sub", "d.sock") // parent does not exist yet
	srv, err := api.NewServer(path, 0o660, fmt.Sprint(os.Getgid()), apitest.NewProvider(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
		t.Errorf("mode %v", fi.Mode())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o750 {
		t.Errorf("dir mode %v", di.Mode())
	}
	srv.Close()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket not removed on Close: %v", err)
	}
}

func TestSocketBadGroup(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "d.sock")
	_, err := api.NewServer(path, 0o660, "no-such-group-goipsla", apitest.NewProvider(), nil)
	if err == nil || !strings.Contains(err.Error(), `socket group "no-such-group-goipsla"`) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("socket left behind")
	}
}

func TestExistingSocket(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "d.sock")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A stale socket (nobody listening) is replaced.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}
	srv, err := api.NewServer(path, 0o600, "", apitest.NewProvider(), logger)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}

	// A live socket makes a second server fail and stay untouched.
	_, err = api.NewServer(path, 0o600, "", apitest.NewProvider(), logger)
	if err == nil || !strings.Contains(err.Error(), "is goipslad already running?") {
		t.Fatalf("live socket: got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	if h, err := api.NewClient(path).Health(context.Background()); err != nil || h.Version != "1.2.3" {
		t.Fatalf("first server no longer reachable: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	srv.Close()

	// A regular file is not removed.
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = api.NewServer(path, 0o600, "", apitest.NewProvider(), logger)
	if err == nil || !strings.Contains(err.Error(), "exists and is not a socket") {
		t.Fatalf("regular file: got %v", err)
	}
}

func TestSocketNotRemovedOnPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores socket permissions")
	}
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "d.sock")
	// A live listener whose socket we may not connect to (EACCES).
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, err = api.NewServer(path, 0o600, "", apitest.NewProvider(), nil)
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether "+path+" is in use, not removing it") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("socket was removed: %v", err)
	}
}

func TestSocketLock(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "d.sock")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	a, err := api.NewServer(path, 0o600, "", apitest.NewProvider(), logger)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path + ".lock")
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}

	// Simulate the race: the socket file vanished (another process removed
	// it between its check and our listen). The lock still keeps a second
	// server out, so it cannot take over the path.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err = api.NewServer(path, 0o600, "", apitest.NewProvider(), logger)
	if err == nil || !strings.Contains(err.Error(), path+".lock is locked by another process (is goipslad already running?)") {
		t.Fatalf("second server: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("second server created the socket")
	}

	// After Close the lock is released and the lock file stays.
	a.Close()
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock file removed: %v", err)
	}
	b, err := api.NewServer(path, 0o600, "", apitest.NewProvider(), logger)
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	b.Close()
}

func TestSocketConcurrentStart(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "d.sock")
	// Leave a stale socket so that every contender takes the removal path.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	const n = 8
	type res struct {
		srv *api.Server
		err error
	}
	results := make(chan res, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			s, err := api.NewServer(path, 0o600, "", apitest.NewProvider(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			results <- res{s, err}
		}()
	}
	close(start)
	var winners []*api.Server
	for i := 0; i < n; i++ {
		r := <-results
		if r.err == nil {
			winners = append(winners, r.srv)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d servers started, want exactly 1", len(winners))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- winners[0].Serve(ctx) }()
	if _, err := api.NewClient(path).Health(context.Background()); err != nil {
		t.Fatalf("winner not reachable: %v", err)
	}
	cancel()
	<-done
	winners[0].Close()
}
