//declscope:namespace access

package api

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"

	"goipsla/internal/clock"
)

// This file logs the requests: every changing request (POST: reload,
// restart, reset) at info so that the operator sees what came in through
// the API, reads at debug, and a panicking handler at error with its stack
// (answered with 500 instead of a dropped connection).

// access wraps h with the request log and the panic recovery.
//
//declscope:package // routes wraps the whole mux with it
func (s *Server) access(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := clock.Real().Now()
		aw := &accessWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler { //nolint:errorlint // the sentinel value net/http panics with
					panic(p)
				}
				s.logger.Error("api handler panicked", "method", r.Method, "path", r.URL.Path,
					"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if !aw.wrote {
					s.json(aw, http.StatusInternalServerError, errorBody{Error: "internal error"})
				}
				aw.status = http.StatusInternalServerError
			}
			attrs := []any{"method", r.Method, "path", r.URL.Path, "status", aw.status,
				"duration_ms", float64(clock.Real().Now().Sub(start).Microseconds()) / 1000}
			if id := accessOpID(r.URL.Path); id != "" {
				attrs = append(attrs, "op", id)
			}
			if r.Method == http.MethodPost {
				s.logger.Info("api request", attrs...)
			} else {
				s.logger.Debug("api request", attrs...)
			}
		}()
		h.ServeHTTP(aw, r)
	})
}

// accessWriter records the status of a response.
type accessWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *accessWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status, w.wrote = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// accessOpID is the operation ID of /v1/operations/{id}[/restart], or "".
func accessOpID(path string) string {
	rest, ok := strings.CutPrefix(path, "/v1/operations/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	if _, err := strconv.Atoi(id); err != nil {
		return ""
	}
	return id
}
