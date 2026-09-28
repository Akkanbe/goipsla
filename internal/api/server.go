package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"goipsla/internal/config"
	"goipsla/internal/stats"
)

// Timeouts of the HTTP server.
const (
	serverReadHeaderTimeout = 5 * time.Second
	serverShutdownTimeout   = 5 * time.Second
	// serverIdleTimeout closes keep-alive connections a client left open.
	serverIdleTimeout = 60 * time.Second
)

// Server serves the API on a Unix domain socket.
type Server struct {
	path string
	ln   net.Listener
	srv  *http.Server
	p    Provider
	// logger is also the request log's (access.go).
	//
	//declscope:package // the request log writes to it
	logger *slog.Logger

	lock *os.File // flock'ed <socket>.lock, held until Close

	closeOnce sync.Once
	closeErr  error
}

// NewServer listens on socketPath. The parent directory is created with
// mode 0750 if missing. mode is applied to the socket file, and group, if not
// empty, becomes its group (a name or a numeric gid).
//
// Two daemons must never share a socket. NewServer first takes an exclusive
// flock on "<socketPath>.lock" (created if needed, kept afterwards) and holds
// it until Close; a second daemon fails right there. Only under that lock does
// it look at an existing socket file: it is removed only when connecting to
// it is refused (ECONNREFUSED: nobody listens). A successful connection, a
// permission error, a timeout or any other error leaves the file alone and
// fails, because the socket may belong to a live daemon.
func NewServer(socketPath string, mode os.FileMode, group string, p Provider, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	gid := -1
	if group != "" {
		g, err := lookupSocketGroup(group)
		if err != nil {
			return nil, err
		}
		gid = g
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	lock, err := lockSocket(socketPath)
	if err != nil {
		return nil, err
	}
	unlock := func(err error) (*Server, error) {
		lock.Close() // releases the flock
		return nil, err
	}
	if err := removeStaleSocket(socketPath); err != nil {
		return unlock(err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return unlock(fmt.Errorf("listen on %s: %w", socketPath, err))
	}
	// Closing the listener removes the socket file. Under the lock, the file
	// at the path is ours.
	ln.(*net.UnixListener).SetUnlinkOnClose(true)
	fail := func(err error) (*Server, error) {
		ln.Close()
		return unlock(err)
	}
	if err := os.Chmod(socketPath, mode); err != nil {
		return fail(fmt.Errorf("chmod %s: %w", socketPath, err))
	}
	if gid >= 0 {
		if err := os.Chown(socketPath, -1, gid); err != nil {
			return fail(fmt.Errorf("chgrp %s: %w", socketPath, err))
		}
	}
	s := &Server{path: socketPath, ln: ln, p: p, logger: logger, lock: lock}
	s.srv = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Addr returns the socket path.
func (s *Server) Addr() string { return s.path }

// Serve serves requests until ctx is done, then shuts down gracefully. It
// returns nil after a normal shutdown or Close.
func (s *Server) Serve(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(s.ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		err := s.srv.Shutdown(sctx)
		<-errc
		if err != nil {
			s.logger.Warn("api shutdown timed out; closing the remaining connections", "timeout", serverShutdownTimeout.String(), "err", err)
			s.srv.Close()
		}
		return nil
	}
}

// Close stops the server immediately, removes the socket file and releases
// the lock. Call it also after Serve returns.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.srv.Close()
		if err := s.ln.Close(); err != nil && s.closeErr == nil && !errors.Is(err, net.ErrClosed) {
			s.closeErr = err
		}
		if err := s.lock.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// ---- handlers ----

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.method(http.MethodGet, s.noQuery(s.health)))
	mux.HandleFunc("/v1/operations", s.method(http.MethodGet, s.operations))
	mux.HandleFunc("/v1/operations/{id}", s.method(http.MethodGet, s.operation))
	mux.HandleFunc("/v1/operations/{id}/restart", s.method(http.MethodPost, s.noQuery(s.restart)))
	mux.HandleFunc("/v1/reload", s.method(http.MethodPost, s.noQuery(s.reload)))
	mux.HandleFunc("/v1/reset", s.method(http.MethodPost, s.noQuery(s.reset)))
	mux.HandleFunc("/v1/reactions", s.method(http.MethodGet, s.reactions))
	mux.HandleFunc("/v1/tracks", s.method(http.MethodGet, s.tracks))
	mux.HandleFunc("/v1/events", s.method(http.MethodGet, s.events))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.error(w, r, http.StatusNotFound, fmt.Sprintf("no such endpoint: %s", r.URL.Path))
	})
	return s.access(mux)
}

// noQuery rejects any query parameter with 400.
func (s *Server) noQuery(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkQuery(r, nil); err != nil {
			s.error(w, r, http.StatusBadRequest, err.Error())
			return
		}
		h(w, r)
	}
}

// method rejects other methods with 405 and a JSON body.
func (s *Server) method(m string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != m {
			w.Header().Set("Allow", m)
			s.error(w, r, http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed; use %s", r.Method, m))
			return
		}
		h(w, r)
	}
}

// json writes v as the JSON body of a response with status.
//
//declscope:package // the request log answers a panic with it
func (s *Server) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.logger.Debug("api: write response", "err", err)
	}
}

func (s *Server) error(w http.ResponseWriter, r *http.Request, status int, msg string) {
	// 501 is ErrNotImplemented, a deliberate answer (a Provider without
	// events), not a failure to act on: it is not logged as one
	// (docs/api.md).
	if status >= 500 && status != http.StatusNotImplemented {
		s.logger.Error("api request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", msg)
	}
	s.json(w, status, errorBody{Error: msg})
}

// providerError maps a Provider error to a status.
//
//	ErrNotImplemented → 501, ErrNotFound → 404, ErrNotActive → 409,
//	*config.ValidationError → 400 with every error listed, anything else → 500.
func (s *Server) providerError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *config.ValidationError
	switch {
	case errors.Is(err, ErrNotImplemented):
		s.error(w, r, http.StatusNotImplemented, err.Error())
	case errors.Is(err, ErrNotFound):
		s.error(w, r, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNotActive):
		s.error(w, r, http.StatusConflict, err.Error())
	case errors.As(err, &ve):
		body := errorBody{Error: "invalid configuration", Errors: make([]string, 0, len(ve.Errors))}
		for _, fe := range ve.Errors {
			body.Errors = append(body.Errors, fe.String())
		}
		s.json(w, http.StatusBadRequest, body)
	default:
		s.error(w, r, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	s.json(w, http.StatusOK, s.p.Health())
}

func (s *Server) operations(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilterQuery(r)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if inc, ok := r.URL.Query()["include"]; ok {
		opts, err := parseListIncludeQuery(inc)
		if err != nil {
			s.error(w, r, http.StatusBadRequest, err.Error())
			return
		}
		s.json(w, http.StatusOK, s.details(f, opts))
		return
	}
	rows := []OperationRow{}
	for _, row := range s.p.Operations() {
		if f.match(row) {
			rows = append(rows, row)
		}
	}
	s.json(w, http.StatusOK, rows)
}

// details returns the detail of every operation that matches f, in ID
// order, with the parts selected by opts. An operation removed between the
// listing and its detail is skipped.
func (s *Server) details(f OperationFilter, opts stats.SnapshotOptions) []*OperationDetail {
	ds := []*OperationDetail{}
	for _, row := range s.p.Operations() {
		if !f.match(row) {
			continue
		}
		if d, ok := s.p.Operation(row.ID, opts); ok {
			ds = append(ds, d)
		}
	}
	return ds
}

// serverPathID reads the {id} of the /v1/operations/{id} routes.
func serverPathID(r *http.Request) (int, error) {
	raw := r.PathValue("id")
	id, err := strconv.Atoi(raw)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid operation id %q: must be a positive integer", raw)
	}
	return id, nil
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	id, err := serverPathID(r)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkQuery(r, []string{"include"}, "include"); err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	opts, err := parseIncludeQuery(r.URL.Query()["include"])
	if err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	d, ok := s.p.Operation(id, opts)
	if !ok {
		s.error(w, r, http.StatusNotFound, fmt.Sprintf("operation %d not found", id))
		return
	}
	s.json(w, http.StatusOK, d)
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	res, err := s.p.Reload(r.Context())
	if err != nil {
		s.providerError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, res)
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	id, err := serverPathID(r)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.p.Restart(r.Context(), id); err != nil {
		s.providerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	if err := s.p.Reset(r.Context()); err != nil {
		s.providerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- reactions, tracks and events (P5) ----

// eventProvider returns the Provider as an EventProvider, or answers 501.
func (s *Server) eventProvider(w http.ResponseWriter, r *http.Request) (EventProvider, bool) {
	ep, ok := s.p.(EventProvider)
	if !ok {
		s.providerError(w, r, ErrNotImplemented)
	}
	return ep, ok
}

func (s *Server) reactions(w http.ResponseWriter, r *http.Request) {
	id, err := queryID(r)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ep, ok := s.eventProvider(w, r)
	if !ok {
		return
	}
	rows, ok := ep.Reactions(id)
	if !ok {
		s.error(w, r, http.StatusNotFound, fmt.Sprintf("operation %d not found", id))
		return
	}
	if rows == nil {
		rows = []ReactionJSON{}
	}
	s.json(w, http.StatusOK, rows)
}

func (s *Server) tracks(w http.ResponseWriter, r *http.Request) {
	id, err := queryID(r)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ep, ok := s.eventProvider(w, r)
	if !ok {
		return
	}
	tracks, ok := ep.Tracks(id)
	if !ok {
		s.error(w, r, http.StatusNotFound, fmt.Sprintf("track %d not found", id))
		return
	}
	if tracks == nil {
		tracks = []TrackJSON{}
	}
	s.json(w, http.StatusOK, tracks)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r, []string{"limit"}); err != nil {
		s.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	limit := DefaultEventLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxEventLimit {
			s.error(w, r, http.StatusBadRequest, fmt.Sprintf("invalid limit %q: must be between 1 and %d", v, MaxEventLimit))
			return
		}
		limit = n
	}
	ep, ok := s.eventProvider(w, r)
	if !ok {
		return
	}
	evs := ep.Events(limit)
	if evs == nil {
		evs = []EventJSON{}
	}
	s.json(w, http.StatusOK, evs)
}
