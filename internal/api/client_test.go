package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/stats"
)

func TestClient(t *testing.T) {
	p := apitest.NewProvider()
	c := api.NewClient(apitest.Serve(t, p))
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(p.HealthV, *h); diff != "" {
		t.Error(diff)
	}

	rows, err := c.Operations(ctx, api.OperationFilter{Tag: "wan", RC: "timeout"})
	if err != nil || len(rows) != 1 || rows[0].ID != 102 {
		t.Fatalf("rows %+v, %v", rows, err)
	}
	if diff := cmp.Diff(p.Rows[2], rows[0]); diff != "" {
		t.Errorf("row round trip (-want +got):\n%s", diff)
	}

	d, err := c.Operation(ctx, 101, stats.SnapshotOptions{Hours: true, History: true, Enhanced: true})
	if err != nil {
		t.Fatal(err)
	}
	want := p.Details[101]
	if diff := cmp.Diff(want.Hours, d.Hours); diff != "" {
		t.Errorf("hours:\n%s", diff)
	}
	if len(d.History) != 3 || len(d.Enhanced) != 2 || d.Latest.RTTMs == nil || *d.Latest.RTTMs != 1.234 {
		t.Errorf("detail %+v", d)
	}
	var cfg struct{ Template string }
	if err := json.Unmarshal(d.Config, &cfg); err != nil || cfg.Template != "wan-echo" {
		t.Errorf("config %s", d.Config)
	}

	_, err = c.Operation(ctx, 999, stats.SnapshotOptions{})
	if !errors.Is(err, api.ErrNotFound) || err.Error() != "operation 999 not found" {
		t.Errorf("404: %v", err)
	}
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("not an *api.Error: %v", err)
	}

	_, err = c.Reload(ctx)
	if !errors.Is(err, api.ErrNotImplemented) {
		t.Errorf("501: %v", err)
	}
	p.ReloadRes = &api.ReloadResult{Added: []int{5}}
	if r, err := c.Reload(ctx); err != nil || r.Added[0] != 5 {
		t.Errorf("reload %+v %v", r, err)
	}
	if err := c.Restart(ctx, 101); err != nil {
		t.Errorf("restart: %v", err)
	}
	if err := c.Restart(ctx, 5); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("restart 404: %v", err)
	}
	if err := c.Reset(ctx); err != nil {
		t.Errorf("reset: %v", err)
	}
	_, err = c.Operations(ctx, api.OperationFilter{State: "bogus"})
	if err == nil || !strings.Contains(err.Error(), `invalid state "bogus"`) || errors.Is(err, api.ErrNotFound) {
		t.Errorf("400: %v", err)
	}
}

func TestClientNoServer(t *testing.T) {
	dir, _ := os.MkdirTemp("", "goipsla")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "none.sock")
	_, err := api.NewClient(path).Health(context.Background())
	want := "cannot connect to " + path + ": no such file or directory (is goipslad running?)"
	if err == nil || err.Error() != want {
		t.Fatalf("got  %v\nwant %s", err, want)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Error("error must be one line")
	}
}
