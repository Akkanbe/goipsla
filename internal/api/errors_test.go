// A test of the server's error mapping; it shares the server tests' namespace.
//
//declscope:namespace server

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/config"
)

func TestServerNotActiveAndInvalidConfig(t *testing.T) {
	p := apitest.NewProvider()
	p.RestartErr = fmt.Errorf("operation 103 is pending: %w", api.ErrNotActive)
	p.ReloadErr = fmt.Errorf("reload: %w", &config.ValidationError{Errors: []config.FieldError{
		{Path: "operations[0].timeout", Msg: "must be at least 1ms"},
		{Path: "operations(id=5).frequency", Msg: "must be greater than timeout"},
	}})
	sock := apitest.Serve(t, p)

	status, body := rawDo(t, sock, "POST", "/v1/operations/103/restart")
	if status != 409 || errMsg(t, body) != "operation 103 is pending: operation is not active" {
		t.Errorf("restart pending: %d %s", status, body)
	}

	status, body = rawDo(t, sock, "POST", "/v1/reload")
	var eb struct {
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil || status != 400 {
		t.Fatalf("reload: %d %s", status, body)
	}
	want := []string{"operations[0].timeout: must be at least 1ms", "operations(id=5).frequency: must be greater than timeout"}
	if eb.Error != "invalid configuration" || !cmp.Equal(eb.Errors, want) {
		t.Errorf("reload body = %+v", eb)
	}

	// Ordinary 400s carry no "errors".
	_, body = rawDo(t, sock, "GET", "/v1/operations?state=x")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["errors"]; ok {
		t.Errorf("errors present in %s", body)
	}

	c := api.NewClient(sock)
	ctx := context.Background()
	err := c.Restart(ctx, 103)
	if !errors.Is(err, api.ErrNotActive) || errors.Is(err, api.ErrNotFound) {
		t.Errorf("client restart: %v", err)
	}
	_, err = c.Reload(ctx)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != "invalid configuration" || !cmp.Equal(ae.Errors, want) {
		t.Fatalf("client reload: %#v", err)
	}
	if got := err.Error(); got != "invalid configuration: 2 errors: "+want[0]+"; "+want[1] {
		t.Errorf("Error() = %q", got)
	}
	one := &api.Error{Status: 400, Message: "invalid configuration", Errors: want[:1]}
	if got := one.Error(); got != "invalid configuration: "+want[0] {
		t.Errorf("one error: %q", got)
	}
}
