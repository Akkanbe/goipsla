// Tests of the fixes of the 2026-09-27 audit (fix-audit.md B10-B13, B25):
// the logs of the server and the limits of the client. They share the
// server tests' namespace.
//
//declscope:namespace server

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/config"
)

// syncBuffer is a bytes.Buffer safe for the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serverLogs runs a server for p that logs JSON at debug into the returned
// buffer, and returns its socket.
func serverLogs(t *testing.T, p api.Provider) (string, *syncBuffer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "goipsla")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	buf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := api.NewServer(filepath.Join(dir, "s.sock"), 0o600, "", p, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
	})
	return srv.Addr(), buf
}

// serverLogLines returns the JSON log lines whose msg is msg.
func serverLogLines(t *testing.T, buf *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// A YAML syntax error has no path: the reload body must not start with
// ": " (B10).
func TestServerReloadSyntaxError(t *testing.T) {
	p := apitest.NewProvider()
	_, perr := config.Parse([]byte("operations: [ {"))
	p.ReloadErr = perr
	sock := apitest.Serve(t, p)
	_, err := api.NewClient(sock).Reload(context.Background())
	var ae *api.Error
	if !errors.As(err, &ae) || len(ae.Errors) != 1 {
		t.Fatalf("reload: %#v", err)
	}
	if strings.HasPrefix(ae.Errors[0], ":") || !strings.HasPrefix(ae.Errors[0], "yaml: ") {
		t.Errorf("errors[0] = %q", ae.Errors[0])
	}
	if strings.Contains(err.Error(), ": : ") {
		t.Errorf("Error() = %q", err.Error())
	}
}

// panickingProvider panics in Health.
type panickingProvider struct{ *apitest.Provider }

func (panickingProvider) Health() api.Health { panic("boom") }

// A panicking handler answers 500 and is logged at error with the request
// (B13).
func TestServerPanic(t *testing.T) {
	sock, logs := serverLogs(t, panickingProvider{apitest.NewProvider()})
	status, body := rawDo(t, sock, "GET", "/v1/health")
	if status != http.StatusInternalServerError || errMsg(t, body) != "internal error" {
		t.Fatalf("status %d body %s", status, body)
	}
	lines := serverLogLines(t, logs, "api handler panicked")
	if len(lines) != 1 {
		t.Fatalf("panic logs: %s", logs)
	}
	l := lines[0]
	if l["level"] != "ERROR" || l["method"] != "GET" || l["path"] != "/v1/health" || l["panic"] != "boom" ||
		!strings.Contains(l["stack"].(string), "panickingProvider") {
		t.Errorf("panic log %v", l)
	}
	// The server keeps serving.
	if status, _ := rawDo(t, sock, "GET", "/v1/operations"); status != http.StatusOK {
		t.Errorf("after panic: %d", status)
	}
}

// POSTs are logged at info with the operation, GETs at debug, 5xx at error
// (B13).
func TestServerRequestLog(t *testing.T) {
	p := apitest.NewProvider()
	p.ResetErr = errors.New("disk on fire")
	sock, logs := serverLogs(t, p)
	rawDo(t, sock, "POST", "/v1/operations/101/restart")
	rawDo(t, sock, "GET", "/v1/operations")
	rawDo(t, sock, "POST", "/v1/reset")

	lines := serverLogLines(t, logs, "api request")
	if len(lines) != 3 {
		t.Fatalf("request logs: %s", logs)
	}
	want := []struct {
		level, method, path, op string
		status                  float64
	}{
		{"INFO", "POST", "/v1/operations/101/restart", "101", 204},
		{"DEBUG", "GET", "/v1/operations", "", 200},
		{"INFO", "POST", "/v1/reset", "", 500},
	}
	for i, w := range want {
		l := lines[i]
		op, _ := l["op"].(string)
		if l["level"] != w.level || l["method"] != w.method || l["path"] != w.path || l["status"] != w.status || op != w.op {
			t.Errorf("line %d: %v, want %+v", i, l, w)
		}
		if _, ok := l["duration_ms"].(float64); !ok {
			t.Errorf("line %d: no duration_ms: %v", i, l)
		}
	}
	failed := serverLogLines(t, logs, "api request failed")
	if len(failed) != 1 || failed[0]["level"] != "ERROR" || failed[0]["err"] != "disk on fire" {
		t.Errorf("5xx log: %v", failed)
	}
}

// rawServer serves h on a Unix socket, for client tests against something
// that is not goipslad.
func rawServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "goipsla")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "raw.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

// A body over the limit is an explicit error, not a truncated document
// (B11).
func TestClientBodyLimit(t *testing.T) {
	sock := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":"`))
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for range 65 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}`))
	})
	_, err := api.NewClient(sock).Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 MiB") {
		t.Errorf("err = %v", err)
	}
}

// A non-JSON error body is quoted in part, without control characters
// (B12).
func TestClientNonJSONError(t *testing.T) {
	sock := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>\n\x1b[31mbad\tgateway</html>" + strings.Repeat("y", 2000)))
	})
	_, err := api.NewClient(sock).Health(context.Background())
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusBadGateway {
		t.Fatalf("err = %#v", err)
	}
	if !strings.HasPrefix(ae.Message, "502 Bad Gateway: <html> [31mbad gateway</html>yyy") ||
		!strings.HasSuffix(ae.Message, "...") || len(ae.Message) > 600 || strings.ContainsRune(ae.Message, 0x1b) {
		t.Errorf("message %q (%d bytes)", ae.Message, len(ae.Message))
	}
}

// 501 (ErrNotImplemented) is a deliberate answer, not a failure: it is in
// the request log but not logged as "api request failed" (R4, docs/api.md).
func TestServerNotImplementedIsNotAnError(t *testing.T) {
	p := apitest.NewProvider() // ReloadRes nil: Reload answers ErrNotImplemented
	sock, logs := serverLogs(t, p)
	if status, _ := rawDo(t, sock, "POST", "/v1/reload"); status != http.StatusNotImplemented {
		t.Fatalf("status %d", status)
	}
	if failed := serverLogLines(t, logs, "api request failed"); len(failed) != 0 {
		t.Errorf("501 logged as a failure: %v", failed)
	}
	reqs := serverLogLines(t, logs, "api request")
	if len(reqs) != 1 || reqs[0]["status"] != float64(http.StatusNotImplemented) || reqs[0]["level"] != "INFO" {
		t.Errorf("request log: %v", reqs)
	}
}
