package main

import (
	"errors"
	"testing"
	"time"

	"goipsla/internal/api"
	"goipsla/internal/api/apitest"
	"goipsla/internal/event"
	"goipsla/internal/op"
	"goipsla/internal/react"
)

// showEventsProvider adds reactions, tracks and events to the fake provider.
type showEventsProvider struct {
	*apitest.Provider
	limit int
}

func (p *showEventsProvider) Reactions(id int) ([]api.ReactionJSON, bool) {
	change := apitest.Base.Add(-47 * time.Second)
	rows := map[int][]api.ReactionJSON{
		13: {
			api.NewReactionJSON(13, react.ReactionState{Element: "rtt", ThresholdType: "immediate", Upper: 100, Lower: 50, Action: "syslog", Value: 1}),
			api.NewReactionJSON(13, react.ReactionState{Element: "timeout", ThresholdType: "consecutive", Count: 2, Action: "syslog",
				Occurred: true, Value: 1, LastChange: change, Changes: 1}),
		},
		21: {api.NewReactionJSON(21, react.ReactionState{Element: "rtt", ThresholdType: "xofy", X: 3, Y: 5, Upper: 100, Lower: 50,
			Action: "trap-and-syslog", Occurred: true, Value: 150, LastChange: change, Changes: 3})},
	}
	switch id {
	case 0:
		return append(append([]api.ReactionJSON{}, rows[13]...), rows[21]...), true
	case 13, 21:
		return rows[id], true
	case 22:
		return []api.ReactionJSON{}, true
	}
	return nil, false
}

func (p *showEventsProvider) Tracks(id int) ([]api.TrackJSON, bool) {
	change := apitest.Base.Add(-47 * time.Second)
	all := []api.TrackJSON{
		api.NewTrackJSON(react.TrackState{ID: 1, Operation: 13, Mode: "reachability", State: "up", Changes: 2, LastChange: change,
			LatestRC: op.RCOK, LatestRTT: 4 * time.Millisecond, DelayUp: 5 * time.Second}),
		api.NewTrackJSON(react.TrackState{ID: 2, Operation: 21, Mode: "state", State: "down", Pending: "up", Changes: 1,
			LastChange: apitest.Base.Add(-time.Hour - 2*time.Minute), LatestRC: op.RCOverThreshold, LatestRTT: 150 * time.Millisecond}),
		api.NewTrackJSON(react.TrackState{ID: 3, Operation: 99, Mode: "state", State: "unknown"}),
	}
	if id == 0 {
		return all, true
	}
	for _, t := range all {
		if t.ID == id {
			return []api.TrackJSON{t}, true
		}
	}
	return nil, false
}

func (p *showEventsProvider) Events(limit int) []api.EventJSON {
	p.limit = limit
	t := apitest.Base.Add(-time.Minute)
	return []api.EventJSON{
		{Time: t, Kind: event.TrackDown, OpID: 13, Target: "10.100.1.13", TrackID: 1, TrackState: "down",
			Message: "%TRACK-6-STATE: 1 ip sla 13 reachability Up -> Down"},
		{Time: t.Add(5 * time.Second), Kind: event.ThresholdExceeded, OpID: 13, Target: "10.100.1.13", Element: "timeout",
			Message: "%RTT-4-OPER_TIMEOUT: IP SLAs(13): Threshold exceeded for timeout"},
	}
}

func TestShowP5Golden(t *testing.T) {
	p := &showEventsProvider{Provider: apitest.NewProvider()}
	sock := apitest.Serve(t, p)
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"reactions", []string{"show", "reactions"}},
		{"reactions_13", []string{"show", "reactions", "13"}},
		{"reactions_json", []string{"show", "reactions", "21", "-o", "json"}},
		{"track", []string{"show", "track"}},
		{"track_1", []string{"show", "track", "1"}},
		{"track_2", []string{"show", "track", "2"}},
		{"track_3", []string{"show", "track", "3"}},
		{"track_1_json", []string{"show", "track", "1", "-o", "json"}},
		{"events", []string{"show", "events", "--limit", "5"}},
		{"events_json", []string{"show", "events", "-o", "json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, errOut, err := runCtl(t, append([]string{"--socket", sock}, tt.args...)...)
			if err != nil || errOut != "" {
				t.Fatalf("err %v, stderr %q", err, errOut)
			}
			golden(t, tt.name, out)
		})
	}
	if p.limit != api.DefaultEventLimit {
		t.Errorf("show events without --limit asked for %d, want %d", p.limit, api.DefaultEventLimit)
	}
}

func TestShowP5Errors(t *testing.T) {
	sock := apitest.Serve(t, &showEventsProvider{Provider: apitest.NewProvider()})
	var ee *exitError
	for _, tt := range []struct {
		args   []string
		code   int // 0 success, -1 plain error, else exit code
		stderr string
	}{
		{[]string{"show", "reactions", "22"}, 0, "no reactions\n"},
		{[]string{"show", "reactions", "99"}, -1, ""},
		{[]string{"show", "track", "9"}, -1, ""},
		{[]string{"show", "track", "1001"}, -1, ""},       // above Cisco's 1000: asked to the daemon (not found there)
		{[]string{"show", "track", "2147483647"}, -1, ""}, // the largest configurable track
		{[]string{"show", "track", "0"}, 2, "goipsla: invalid track number \"0\": must be between 1 and 2147483647\nRun 'goipsla show track --help' for usage.\n"},
		{[]string{"show", "track", "2147483648"}, 2, "goipsla: invalid track number \"2147483648\": must be between 1 and 2147483647\nRun 'goipsla show track --help' for usage.\n"},
		{[]string{"show", "events", "--limit", "0"}, 2, "goipsla: invalid --limit 0: must be between 1 and 1000\nRun 'goipsla show events --help' for usage.\n"},
	} {
		_, errOut, err := runCtl(t, append([]string{"--socket", sock}, tt.args...)...)
		switch tt.code {
		case 0:
			if err != nil {
				t.Errorf("%v: %v", tt.args, err)
			}
		case -1:
			if !errors.Is(err, api.ErrNotFound) {
				t.Errorf("%v: err %v, want not found", tt.args, err)
			}
		default:
			if !errors.As(err, &ee) || ee.Code != tt.code {
				t.Errorf("%v: err %v, want exit %d", tt.args, err, tt.code)
			}
		}
		if errOut != tt.stderr {
			t.Errorf("%v: stderr %q, want %q", tt.args, errOut, tt.stderr)
		}
	}
	// A daemon without P5 (plain provider) answers 501.
	plain := apitest.Serve(t, apitest.NewProvider())
	if _, _, err := runCtl(t, "--socket", plain, "show", "track"); !errors.Is(err, api.ErrNotImplemented) {
		t.Errorf("show track against a provider without tracks: %v", err)
	}
}
