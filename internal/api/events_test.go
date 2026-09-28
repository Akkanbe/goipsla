package api_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/event"
	"goipsla/internal/op"
	"goipsla/internal/react"
)

// eventsProvider adds the P5 methods to the fake provider.
type eventsProvider struct {
	*apitest.Provider
	reactions map[int][]api.ReactionJSON
	tracks    []api.TrackJSON
	events    []api.EventJSON
	limit     int
}

func (p *eventsProvider) Reactions(id int) ([]api.ReactionJSON, bool) {
	if id == 0 {
		var all []api.ReactionJSON
		for _, i := range []int{101, 102} {
			all = append(all, p.reactions[i]...)
		}
		return all, true
	}
	rows, ok := p.reactions[id]
	return rows, ok
}

func (p *eventsProvider) Tracks(id int) ([]api.TrackJSON, bool) {
	if id == 0 {
		return p.tracks, true
	}
	for _, t := range p.tracks {
		if t.ID == id {
			return []api.TrackJSON{t}, true
		}
	}
	return nil, false
}

func (p *eventsProvider) Events(limit int) []api.EventJSON {
	p.limit = limit
	return p.events
}

var eventsBase = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func newEventsProvider() *eventsProvider {
	change := eventsBase.Add(-time.Minute)
	return &eventsProvider{
		Provider: apitest.NewProvider(),
		reactions: map[int][]api.ReactionJSON{
			101: {api.NewReactionJSON(101, react.ReactionState{Element: "rtt", ThresholdType: "immediate", Upper: 100, Lower: 50,
				Action: "syslog", Occurred: true, Value: 150, LastChange: change, Changes: 1})},
			102: {},
		},
		tracks: []api.TrackJSON{api.NewTrackJSON(react.TrackState{ID: 1, Operation: 101, Mode: "reachability", State: "up",
			Changes: 1, LastChange: change, LatestRC: op.RCOK, LatestRTT: 1500 * time.Microsecond, DelayUp: 5 * time.Second})},
		events: []api.EventJSON{{Time: change, Kind: event.TrackUp, OpID: 101, TrackID: 1, TrackState: "up", Message: "%TRACK-6-STATE: 1 ip sla 101 reachability Unknown -> Up"}},
	}
}

func TestEventEndpoints(t *testing.T) {
	p := newEventsProvider()
	c := api.NewClient(apitest.Serve(t, p))
	ctx := context.Background()

	all, err := c.Reactions(ctx, 0)
	if err != nil || len(all) != 1 || all[0].OpID != 101 || !all[0].Occurred || all[0].Value != 150 ||
		all[0].LastChange == nil || !all[0].LastChange.Equal(eventsBase.Add(-time.Minute)) {
		t.Errorf("Reactions(0) = %+v, %v", all, err)
	}
	if rows, err := c.Reactions(ctx, 102); err != nil || rows == nil || len(rows) != 0 {
		t.Errorf("Reactions(102) = %#v, %v; want an empty list", rows, err)
	}
	if _, err := c.Reactions(ctx, 999); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Reactions(999) error = %v, want ErrNotFound", err)
	}

	tracks, err := c.Tracks(ctx, 1)
	if err != nil || len(tracks) != 1 || tracks[0].State != "up" || tracks[0].DelayUp != "5s" || tracks[0].DelayDown != "0s" ||
		tracks[0].LatestRTTMs == nil || *tracks[0].LatestRTTMs != 1.5 {
		t.Errorf("Tracks(1) = %+v, %v", tracks, err)
	}
	if _, err := c.Tracks(ctx, 7); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Tracks(7) error = %v, want ErrNotFound", err)
	}

	evs, err := c.Events(ctx, 0)
	if err != nil || len(evs) != 1 || evs[0].Kind != event.TrackUp || p.limit != api.DefaultEventLimit {
		t.Errorf("Events(0) = %+v, %v (limit %d)", evs, err, p.limit)
	}
	if _, err := c.Events(ctx, 5); err != nil || p.limit != 5 {
		t.Errorf("Events(5): %v, limit %d", err, p.limit)
	}
}

func TestEventEndpointErrors(t *testing.T) {
	sock := apitest.Serve(t, newEventsProvider())
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	for _, tt := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/v1/reactions?id=abc", 400, "invalid id"},
		{"GET", "/v1/reactions?op=1", 400, "unknown query parameter"},
		{"GET", "/v1/tracks?id=0", 400, "invalid id"},
		{"GET", "/v1/events?limit=0", 400, "invalid limit"},
		{"GET", "/v1/events?limit=1001", 400, "invalid limit"},
		{"GET", "/v1/events?limit=1&limit=2", 400, "more than once"},
		{"POST", "/v1/events", 405, "not allowed"},
	} {
		req, _ := http.NewRequest(tt.method, "http://goipslad"+tt.path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tt.status || !strings.Contains(string(b), tt.body) {
			t.Errorf("%s %s = %d %s, want %d containing %q", tt.method, tt.path, resp.StatusCode, b, tt.status, tt.body)
		}
	}
}

// A provider without EventProvider answers 501.
func TestEventEndpointsNotImplemented(t *testing.T) {
	c := api.NewClient(apitest.Serve(t, apitest.NewProvider()))
	if _, err := c.Tracks(context.Background(), 0); !errors.Is(err, api.ErrNotImplemented) {
		t.Errorf("Tracks without EventProvider: %v, want ErrNotImplemented", err)
	}
}
