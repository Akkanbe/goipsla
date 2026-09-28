package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"goipsla/internal/stats"
)

const clientTimeout = 5 * time.Second

// clientBaseURL is a placeholder host; the transport always dials the socket.
const clientBaseURL = "http://goipslad"

// Client talks to goipslad over its Unix socket.
type Client struct {
	path string
	hc   *http.Client
}

// NewClient returns a client for the daemon listening on socketPath.
func NewClient(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &Client{path: socketPath, hc: &http.Client{Transport: tr, Timeout: clientTimeout}}
}

// clientMaxBody bounds a response body. A larger one is an error rather
// than a truncated JSON document.
const clientMaxBody = 64 << 20

// clientErrorExcerpt is how much of a non-JSON error body is quoted.
const clientErrorExcerpt = 512

// clientBodyExcerpt quotes the start of a non-JSON error body (a proxy's
// HTML page, say): at most 512 bytes, control characters dropped.
func clientBodyExcerpt(body []byte) string {
	body = bytes.TrimSpace(body)
	cut := len(body) > clientErrorExcerpt
	if cut {
		body = body[:clientErrorExcerpt]
	}
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7F || r == utf8.RuneError {
			return -1
		}
		return r
	}, string(body))
	if cut {
		s += "..."
	}
	return s
}

// do sends the request and decodes a 2xx JSON body into out (if not nil).
func (c *Client) do(ctx context.Context, method, path string, q url.Values, out any) error {
	u := clientBaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.connError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, clientMaxBody+1))
	if err != nil {
		return fmt.Errorf("read response from %s: %w", c.path, err)
	}
	if len(body) > clientMaxBody {
		return fmt.Errorf("response from %s exceeds %d MiB; ask for fewer operations or without include", c.path, clientMaxBody>>20)
	}
	if resp.StatusCode/100 != 2 {
		var eb errorBody
		msg := ""
		if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
			msg = eb.Error
		} else {
			msg = resp.Status + ": " + clientBodyExcerpt(body)
			eb.Errors = nil
		}
		return &Error{Status: resp.StatusCode, Message: msg, Errors: eb.Errors}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response from %s: %w", c.path, err)
	}
	return nil
}

// connError turns a transport error into one line naming the socket.
func (c *Client) connError(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		reason := opErr.Err
		var se interface{ Unwrap() error }
		if errors.As(reason, &se) && se.Unwrap() != nil {
			reason = se.Unwrap()
		}
		return fmt.Errorf("cannot connect to %s: %v (is goipslad running?)", c.path, reason)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("no response from %s within %s (is goipslad running?)", c.path, clientTimeout)
		}
		return fmt.Errorf("request to %s failed: %v", c.path, ue.Err)
	}
	return err
}

// Health returns the daemon status.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	if err := c.do(ctx, http.MethodGet, "/v1/health", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// clientFilterQuery is the query of GET /v1/operations for f.
func clientFilterQuery(f OperationFilter) url.Values {
	q := url.Values{}
	for k, v := range map[string]string{"tag": f.Tag, "state": f.State, "rc": f.RC, "type": f.Type} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

// Operations lists the operations that match f.
func (c *Client) Operations(ctx context.Context, f OperationFilter) ([]OperationRow, error) {
	q := clientFilterQuery(f)
	var rows []OperationRow
	if err := c.do(ctx, http.MethodGet, "/v1/operations", q, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// OperationDetails returns the detail of every operation that matches f,
// with the parts selected by opts, in one request.
func (c *Client) OperationDetails(ctx context.Context, f OperationFilter, opts stats.SnapshotOptions) ([]OperationDetail, error) {
	q := clientFilterQuery(f)
	inc := includeQuery(opts)
	if inc == "" {
		inc = "detail"
	}
	q.Set("include", inc)
	var ds []OperationDetail
	if err := c.do(ctx, http.MethodGet, "/v1/operations", q, &ds); err != nil {
		return nil, err
	}
	return ds, nil
}

// Operation returns one operation with the parts selected by opts.
func (c *Client) Operation(ctx context.Context, id int, opts stats.SnapshotOptions) (*OperationDetail, error) {
	q := url.Values{}
	if inc := includeQuery(opts); inc != "" {
		q.Set("include", inc)
	}
	var d OperationDetail
	if err := c.do(ctx, http.MethodGet, "/v1/operations/"+strconv.Itoa(id), q, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Reload asks the daemon to reload its configuration file.
func (c *Client) Reload(ctx context.Context) (*ReloadResult, error) {
	var r ReloadResult
	if err := c.do(ctx, http.MethodPost, "/v1/reload", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Restart discards the statistics of one operation and starts a new life.
func (c *Client) Restart(ctx context.Context, id int) error {
	return c.do(ctx, http.MethodPost, "/v1/operations/"+strconv.Itoa(id)+"/restart", nil, nil)
}

// Reset discards all statistics and starts new lives.
func (c *Client) Reset(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/reset", nil, nil)
}

// Reactions returns the reaction rows of operation id, or of every operation
// when id is 0.
func (c *Client) Reactions(ctx context.Context, id int) ([]ReactionJSON, error) {
	var out []ReactionJSON
	return out, c.do(ctx, http.MethodGet, "/v1/reactions", clientIDQuery(id), &out)
}

// Tracks returns track id, or every track when id is 0.
func (c *Client) Tracks(ctx context.Context, id int) ([]TrackJSON, error) {
	var out []TrackJSON
	return out, c.do(ctx, http.MethodGet, "/v1/tracks", clientIDQuery(id), &out)
}

// Events returns up to limit of the latest events, oldest first (limit 0:
// the server's default of DefaultEventLimit).
func (c *Client) Events(ctx context.Context, limit int) ([]EventJSON, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []EventJSON
	return out, c.do(ctx, http.MethodGet, "/v1/events", q, &out)
}

// clientIDQuery is the optional ?id= of /v1/reactions and /v1/tracks.
func clientIDQuery(id int) url.Values {
	q := url.Values{}
	if id > 0 {
		q.Set("id", strconv.Itoa(id))
	}
	return q
}
