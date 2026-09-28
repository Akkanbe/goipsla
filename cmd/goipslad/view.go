package main

// view.go presents the provider to the metrics exporter and the SNMP
// subagent. Their interfaces (metrics.Source, snmp.Source) use method names
// that collide with api.EventProvider's (Tracks, Reactions), so each is a
// small view type over the provider rather than methods of it.

import (
	"sort"
	"time"

	"goipsla/internal/config"
	"goipsla/internal/metrics"
	"goipsla/internal/react"
	"goipsla/internal/snmp"
	"goipsla/internal/stats"
)

var (
	_ metrics.Source            = metricsView{}
	_ metrics.DiagnosticsSource = metricsView{}
	_ snmp.Source               = snmpView{}
)

// MetricsView returns the metrics.Source of the daemon: the operations of
// the provider plus its tracks, reactions and event counters.
func (p *provider) MetricsView() metrics.Source { return metricsView{p} }

type metricsView struct{ p *provider }

func (m metricsView) Tracks() []metrics.TrackRow {
	var out []metrics.TrackRow
	if m.p.tracker == nil {
		return out
	}
	for _, st := range m.p.tracker.All() {
		out = append(out, metrics.TrackRow{ID: st.ID, Operation: st.Operation, Mode: st.Mode, State: st.State})
	}
	return out
}

func (m metricsView) Reactions() []metrics.ReactionRow {
	var out []metrics.ReactionRow
	if m.p.react == nil {
		return out
	}
	for _, id := range m.p.mgr.IDs() {
		for _, st := range m.p.react.Snapshot(id) {
			out = append(out, metrics.ReactionRow{OpID: id, Element: st.Element, Occurred: st.Occurred})
		}
	}
	return out
}

// EventStats reports delivered and failed per kind and sink (counted by the
// countingSink wrappers), and dropped per sink (the bus does not tell kinds
// apart there; kind is empty). Events the bus itself dropped (main queue
// full) are reported for the sink "bus".
func (m metricsView) EventStats() []metrics.EventCount {
	var out []metrics.EventCount
	m.p.countsMu.Lock()
	for k, n := range m.p.counts {
		out = append(out, metrics.EventCount{Kind: k.kind, Sink: k.sink, Result: k.result, Count: n})
	}
	m.p.countsMu.Unlock()
	if m.p.bus != nil {
		st := m.p.bus.Stats()
		out = append(out, metrics.EventCount{Sink: "bus", Result: "dropped", Count: st.Dropped})
		for _, s := range st.Sinks {
			out = append(out, metrics.EventCount{Sink: s.Name, Result: "dropped", Count: s.Dropped})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Sink != b.Sink {
			return a.Sink < b.Sink
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Result < b.Result
	})
	return out
}

// Rows implements metrics.Source: one row per current operation, with the
// current configuration (tag, VRF) and the start of the current life.
func (m metricsView) Rows() []metrics.Row {
	p := m.p
	ids := p.mgr.IDs()
	rows := make([]metrics.Row, 0, len(ids))
	for _, id := range ids {
		c, ok := p.mgr.Config(id)
		if !ok {
			continue
		}
		state, ok := p.mgr.State(id)
		if !ok {
			continue
		}
		snap, ok := p.store.Snapshot(id, stats.SnapshotOptions{})
		if !ok {
			continue
		}
		rows = append(rows, metrics.Row{
			ID:        id,
			Type:      c.Type,
			Target:    c.Target.String(),
			Tag:       c.Tag,
			VRF:       c.VRF,
			State:     state,
			Latest:    snap.Latest,
			Totals:    snap.Totals,
			LifeStart: snap.LifeStart,
			// Per-packet counters of icmp-jitter; the summary rows do not
			// carry them, so they come from the snapshot.
			TotalsJitter: snap.TotalsJitter,
		})
	}
	return rows
}

// SNMPView returns the snmp.Source of the daemon: the operations, their
// schedule state, statistics and reaction states. Its method names collide
// with api.EventProvider's Reactions, so it is an adapter like metricsView.
func (p *provider) SNMPView() snmp.Source { return snmpView{p} }

type snmpView struct{ p *provider }

func (s snmpView) IDs() []int { return s.p.mgr.IDs() }

func (s snmpView) Config(id int) (*config.Operation, bool) { return s.p.mgr.Config(id) }

func (s snmpView) State(id int) (state string, lifeLeft time.Duration, forever bool, ok bool) {
	info, ok := s.p.mgr.Info(id)
	if !ok {
		return "", 0, false, false
	}
	if info.LifeLeft == nil {
		return info.State, 0, true, true
	}
	return info.State, *info.LifeLeft, false, true
}

func (s snmpView) Snapshot(id int, opts stats.SnapshotOptions) (*stats.Snapshot, bool) {
	if _, ok := s.p.mgr.Config(id); !ok {
		return nil, false
	}
	return s.p.store.Snapshot(id, opts)
}

func (s snmpView) Reactions(id int) []react.ReactionState {
	if s.p.react == nil {
		return nil
	}
	return s.p.react.Snapshot(id)
}

// Diagnostics implements metrics.DiagnosticsSource: the replies the ICMP
// engine dropped or saw late, and the results the store discarded because
// they belonged to an earlier life.
func (m metricsView) Diagnostics() metrics.Diagnostics {
	var d metrics.Diagnostics
	if m.p.eng != nil {
		d.Probe = m.p.eng.Stats()
	}
	d.ResultsDiscarded = m.p.store.Discarded()
	return d
}
