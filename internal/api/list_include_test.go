// A test of the server's operation list; it shares the server tests'
// namespace.
//
//declscope:namespace server

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/stats"
)

func TestListInclude(t *testing.T) {
	p := apitest.NewProvider()
	sock := apitest.Serve(t, p)
	for _, tc := range []struct {
		path  string
		ids   []int
		calls []string // Operation calls after the listing
		hours bool
	}{
		{"/v1/operations?include=hours,history,enhanced", []int{1, 101, 102, 103}, opCalls([]int{1, 101, 102, 103}, stats.SnapshotOptions{Hours: true, History: true, Enhanced: true}), true},
		{"/v1/operations?include=detail", []int{1, 101, 102, 103}, opCalls([]int{1, 101, 102, 103}, stats.SnapshotOptions{}), false},
		{"/v1/operations?include=detail,hours&tag=wan", []int{101, 102}, opCalls([]int{101, 102}, stats.SnapshotOptions{Hours: true}), true},
		{"/v1/operations?include=hours&include=history&state=pending", []int{103}, opCalls([]int{103}, stats.SnapshotOptions{Hours: true, History: true}), true},
		{"/v1/operations?include=detail&tag=none", []int{}, nil, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			p.Calls = nil
			status, body := rawDo(t, sock, "GET", tc.path)
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			var ds []api.OperationDetail
			if err := json.Unmarshal([]byte(body), &ds); err != nil {
				t.Fatalf("%v: %s", err, body)
			}
			ids := []int{}
			for _, d := range ds {
				ids = append(ids, d.ID)
				if len(d.Config) == 0 || d.LifeIndex != 1 {
					t.Errorf("op %d is not a detail: %+v", d.ID, d)
				}
			}
			if !cmp.Equal(ids, tc.ids) {
				t.Errorf("ids %v, want %v", ids, tc.ids)
			}
			want := append([]string{"operations"}, tc.calls...)
			if diff := cmp.Diff(want, p.Calls); diff != "" {
				t.Errorf("provider calls (-want +got):\n%s", diff)
			}
			for _, d := range ds {
				if d.ID == 101 && tc.hours != (len(d.Hours) > 0) {
					t.Errorf("hours present = %v", len(d.Hours) > 0)
				}
			}
		})
	}

	for _, tc := range []struct{ path, msg string }{
		{"/v1/operations?include=", `empty element in include "": must be detail, hours, history or enhanced`},
		{"/v1/operations?include=hours,,history", `empty element in include "hours,,history": must be detail, hours, history or enhanced`},
		{"/v1/operations?include=stats", `unknown include "stats": must be detail, hours, history or enhanced`},
		{"/v1/operations?include=hours&state=up", `invalid state "up": must be pending, inactive or active`},
	} {
		status, body := rawDo(t, sock, "GET", tc.path)
		if status != 400 || errMsg(t, body) != tc.msg {
			t.Errorf("%s: %d %s", tc.path, status, body)
		}
	}
	// "detail" belongs to the list only.
	if status, body := rawDo(t, sock, "GET", "/v1/operations/101?include=detail"); status != 400 {
		t.Errorf("single operation with detail: %d %s", status, body)
	}
}

func opCalls(ids []int, opts stats.SnapshotOptions) []string {
	var c []string
	for _, id := range ids {
		c = append(c, fmt.Sprint("operation ", id, " ", opts))
	}
	return c
}

func TestClientOperationDetails(t *testing.T) {
	p := apitest.NewProvider()
	c := api.NewClient(apitest.Serve(t, p))
	ctx := context.Background()

	ds, err := c.OperationDetails(ctx, api.OperationFilter{Type: "icmp-echo", Tag: "wan"}, stats.SnapshotOptions{History: true})
	if err != nil || len(ds) != 2 || ds[0].ID != 101 || ds[1].ID != 102 || len(ds[0].History) != 3 || ds[0].Hours != nil {
		t.Fatalf("details %+v, %v", ds, err)
	}
	p.Calls = nil
	ds, err = c.OperationDetails(ctx, api.OperationFilter{}, stats.SnapshotOptions{})
	if err != nil || len(ds) != 4 || ds[1].History != nil || ds[0].TotalsJitter == nil {
		t.Fatalf("plain details %+v, %v", ds, err)
	}
	if len(p.Calls) != 5 || p.Calls[0] != "operations" {
		t.Errorf("calls %v", p.Calls)
	}
	if _, err := c.OperationDetails(ctx, api.OperationFilter{State: "bogus"}, stats.SnapshotOptions{}); err == nil {
		t.Error("bad filter accepted")
	}
}
