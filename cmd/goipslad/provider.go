package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"goipsla/internal/api"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/manager"
	"goipsla/internal/probe"
	"goipsla/internal/react"
	"goipsla/internal/stats"
)

// provider implements api.Provider on top of the manager (operations,
// states, schedule, control) and the statistics store (everything else).
// The fields the metrics and SNMP views (view.go) and the counting sink
// (sink.go) read come first; the rest are the provider's own.
//
//declscope:package // the metrics and SNMP views and the counting sink read it
type provider struct {
	store *stats.Store
	mgr   *manager.Manager

	// P5: reactions, tracks and events; nil until SetEvents.
	bus     *event.Bus
	react   *react.Engine
	tracker *react.Tracker

	// eng is the ICMP engine, for the probe diagnostics of the metrics view;
	// nil until SetProbe.
	eng probe.Engine

	countsMu sync.Mutex
	counts   map[sinkCountKey]uint64 // delivered / failed per kind and sink (sink.go)

	// version is reported by Health.
	//
	//declscope:private
	version string
	// startedAt is reported by Health.
	//
	//declscope:private
	startedAt time.Time
	// configPath is the file Reload reads.
	//
	//declscope:private
	configPath string
	// load reads and validates the configuration (config.Load; tests replace it).
	//
	//declscope:private
	load func(path string) (*config.Config, error)
	//declscope:private
	logger *slog.Logger
	// reloadMu serializes reloads from SIGHUP and the API.
	//
	//declscope:private
	reloadMu sync.Mutex
}

var (
	_ api.Provider      = (*provider)(nil)
	_ api.EventProvider = (*provider)(nil)
)

// newProvider is the provider's constructor; main builds the provider with
// it and hands it to the API server, the metrics exporter and SIGHUP.
//
//declscope:package // main wires the provider into the daemon
func newProvider(version string, store *stats.Store, mgr *manager.Manager, configPath string, startedAt time.Time, logger *slog.Logger) *provider {
	return &provider{
		store:      store,
		mgr:        mgr,
		version:    version,
		startedAt:  startedAt,
		configPath: configPath,
		load:       config.Load,
		logger:     logger,
	}
}

func (p *provider) Health() api.Health {
	h := api.Health{Version: p.version, StartedAt: p.startedAt.UTC(), ConfigPath: p.configPath}
	n := p.mgr.Counts()
	h.Operations.Total = n.Total
	h.Operations.Active = n.Active
	h.Operations.Pending = n.Pending
	h.Operations.Inactive = n.Inactive
	return h
}

// fill sets the fields of a row that come from the manager: the current
// configuration (a reload may have changed the tag since the statistics row
// was created), the state and the schedule information. It reports false
// if the manager no longer has the operation.
func (p *provider) fill(row *api.OperationRow) bool {
	c, ok := p.mgr.Config(row.ID)
	if !ok {
		return false
	}
	info, ok := p.mgr.Info(row.ID)
	if !ok {
		return false
	}
	// Seconds are rounded up, so that a life with 0.3 s left is not shown as
	// 0 while the operation is still active.
	secs := func(d time.Duration) *int64 {
		n := int64((d + time.Second - 1) / time.Second)
		return &n
	}
	row.Tag = c.Tag
	row.VRF = c.VRF
	row.State = info.State
	if info.LifeLeft != nil {
		row.LifeLeftS = secs(*info.LifeLeft)
	}
	if !info.NextStart.IsZero() {
		t := info.NextStart.UTC()
		row.NextStart = &t
	}
	if info.AgeoutLeft != nil {
		row.Ageout = secs(*info.AgeoutLeft)
	}
	return true
}

func (p *provider) Operations() []api.OperationRow {
	sum := p.store.Summary()
	rows := make([]api.OperationRow, 0, len(sum))
	for _, r := range sum {
		row := api.NewOperationRow(r)
		if p.fill(&row) {
			rows = append(rows, row)
		}
	}
	return rows
}

func (p *provider) Operation(id int, opts stats.SnapshotOptions) (*api.OperationDetail, bool) {
	c, ok := p.mgr.Config(id)
	if !ok {
		return nil, false
	}
	snap, ok := p.store.Snapshot(id, opts)
	if !ok {
		return nil, false
	}
	d, err := api.NewOperationDetail(snap, c)
	if err != nil {
		// Only EffectiveJSON can fail, on a configuration that passed
		// validation; report the operation as missing rather than half-built.
		p.logger.Error("api: building operation detail", "op", id, "err", err)
		return nil, false
	}
	if !p.fill(&d.OperationRow) {
		return nil, false
	}
	return d, true
}

// Reload implements api.Provider: POST /v1/reload (goipsla reload).
func (p *provider) Reload(_ context.Context) (*api.ReloadResult, error) {
	return p.reload("api")
}

// ReloadOnSignal reloads on SIGHUP. It logs like Reload, with
// source=sighup.
func (p *provider) ReloadOnSignal() { _, _ = p.reload("sighup") }

// reload reads the configuration file again and applies it. A file that does
// not load or validate is not applied at all; the daemon keeps running the
// previous configuration. The API and SIGHUP both come here, serialized, and
// log the same three lines with where the request came from (source=api or
// sighup): the start, then the result or the failure.
func (p *provider) reload(source string) (*api.ReloadResult, error) {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	p.logger.Info("reloading the configuration", "source", source, "config", p.configPath)
	cfg, err := p.load(p.configPath)
	if err != nil {
		attrs := []any{"source", source, "config", p.configPath, "err", err}
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			// Every validation error, one per element, not only the joined line.
			lines := make([]string, 0, len(ve.Errors))
			for _, fe := range ve.Errors {
				lines = append(lines, fe.String())
			}
			attrs = append(attrs, "errors", lines)
		}
		p.logger.Error("reload failed; keeping the running configuration", attrs...)
		return nil, err
	}
	res, err := p.mgr.Reload(cfg)
	if err != nil {
		p.logger.Error("reload failed; keeping the running configuration", "source", source, "config", p.configPath, "err", err)
		return nil, err
	}
	p.logger.Info("configuration reloaded", "source", source, "config", p.configPath,
		"added", res.Added, "removed", res.Removed, "restarted", res.Restarted, "updated", res.Updated,
		"warnings", len(res.Warnings))
	return &api.ReloadResult{
		Added:     res.Added,
		Removed:   res.Removed,
		Restarted: res.Restarted,
		Updated:   res.Updated,
		Warnings:  res.Warnings,
	}, nil
}

func (p *provider) Restart(_ context.Context, id int) error {
	err := p.mgr.Restart(id)
	switch {
	case errors.Is(err, manager.ErrNotFound):
		return fmt.Errorf("operation %d %w", id, api.ErrNotFound)
	case errors.Is(err, manager.ErrNotActive):
		state, _ := p.mgr.State(id)
		return &providerNotActiveError{id: id, state: state}
	}
	return err
}

func (p *provider) Reset(context.Context) error { return p.mgr.Reset() }

// SetProbe gives the provider the ICMP engine, whose drop and late-reply
// counters the metrics view exports.
func (p *provider) SetProbe(eng probe.Engine) { p.eng = eng }

// SetEvents gives the provider the event bus, the reaction engine and the
// tracker it serves on /v1/reactions, /v1/tracks, /v1/events and /metrics.
func (p *provider) SetEvents(bus *event.Bus, e *react.Engine, t *react.Tracker) {
	p.bus, p.react, p.tracker = bus, e, t
}

// Reactions implements api.EventProvider. id 0 lists every operation's rows.
func (p *provider) Reactions(id int) ([]api.ReactionJSON, bool) {
	if id != 0 {
		if _, ok := p.mgr.Config(id); !ok {
			return nil, false
		}
	}
	rows := []api.ReactionJSON{}
	if p.react == nil {
		return rows, true
	}
	ids := []int{id}
	if id == 0 {
		ids = p.mgr.IDs()
	}
	for _, i := range ids {
		for _, st := range p.react.Snapshot(i) {
			rows = append(rows, api.NewReactionJSON(i, st))
		}
	}
	return rows, true
}

// Tracks implements api.EventProvider. id 0 lists every track.
func (p *provider) Tracks(id int) ([]api.TrackJSON, bool) {
	out := []api.TrackJSON{}
	if p.tracker == nil {
		return out, id == 0
	}
	if id != 0 {
		st, ok := p.tracker.Snapshot(id)
		if !ok {
			return nil, false
		}
		return append(out, api.NewTrackJSON(st)), true
	}
	for _, st := range p.tracker.All() {
		out = append(out, api.NewTrackJSON(st))
	}
	return out, true
}

// Events implements api.EventProvider.
func (p *provider) Events(limit int) []api.EventJSON {
	if p.bus == nil {
		return nil
	}
	return p.bus.Recent(limit)
}

// providerNotActiveError is a restart the provider refuses because the
// operation is not active. Its text is one sentence naming the state (the
// body of the API's 409 and what goipsla prints); it wraps api.ErrNotActive,
// by which the API server chooses 409 Conflict.
type providerNotActiveError struct {
	id    int
	state string
}

func (e *providerNotActiveError) Error() string {
	return fmt.Sprintf("operation %d is %s: only an active operation can be restarted", e.id, e.state)
}

func (e *providerNotActiveError) Unwrap() error { return api.ErrNotActive }
