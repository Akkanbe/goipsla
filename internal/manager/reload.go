// Reload is a method of Manager filed apart from manager.go: it rebuilds the
// operation set under the manager's lock with the state machine of
// lifecycle.go. One device, one namespace.
//
//declscope:namespace manager

package manager

import (
	"fmt"
	"slices"
	"time"

	"goipsla/internal/config"
)

// ReloadResult reports how a reload changed the operations. Every list is
// sorted and never nil.
type ReloadResult struct {
	Added     []int    // new IDs, created with the initial transition
	Removed   []int    // IDs no longer configured, stopped and deleted
	Restarted []int    // a measurement setting changed: rebuilt with new statistics
	Updated   []int    // only tag / owner / react changed: statistics kept
	Warnings  []string // global settings that need a daemon restart
}

// Reload applies a new, validated configuration (implementation plan 3.6).
// Operations are matched by ID against the operations currently running
// (an operation deleted by ageout counts as absent, so it is re-added):
//
//   - an ID that is new is created with the initial schedule transition;
//   - an ID that disappeared is stopped and its statistics removed;
//   - an ID whose measurement settings changed is rebuilt and its
//     statistics start a new life (Cisco requires deleting and re-creating
//     a scheduled operation to change it);
//   - an ID where only tag, owner or react changed keeps measuring and keeps
//     its statistics; its configuration is swapped;
//   - an unchanged ID is left alone.
//
// Changes to the global section are not applied; they are reported as
// warnings. The caller serializes reloads with other reloads.
func (m *Manager) Reload(cfg *config.Config) (*ReloadResult, error) {
	m.lockLife()
	defer m.unlockLife()
	if !m.running {
		return nil, errNotRunning
	}
	now := m.clk.Now()
	res := &ReloadResult{Added: []int{}, Removed: []int{}, Restarted: []int{}, Updated: []int{}, Warnings: []string{}}

	next := make(map[int]*config.Operation, len(cfg.Operations))
	for _, c := range cfg.Operations {
		next[c.ID] = c
	}
	for _, id := range sortedIDs(m.ops) {
		if _, ok := next[id]; !ok {
			m.deleteLocked(m.ops[id])
			res.Removed = append(res.Removed, id)
		}
	}
	for _, c := range cfg.Operations {
		o, ok := m.ops[c.ID]
		if !ok {
			o = m.newOperation(c)
			m.ops[c.ID] = o
			m.initLocked(o, now)
			res.Added = append(res.Added, c.ID)
			continue
		}
		old := o.config()
		if diff := measurementDiff(old, c, now); len(diff) > 0 {
			m.logger.Debug("reload: measurement settings changed", "op", c.ID, "fields", diff)
			m.stopLocked(o)
			o = m.newOperation(c)
			m.ops[c.ID] = o
			m.initLocked(o, now)
			res.Restarted = append(res.Restarted, c.ID)
			continue
		}
		if diff := notificationDiff(old, c); len(diff) > 0 {
			m.logger.Debug("reload: settings changed without affecting measurement", "op", c.ID, "fields", diff)
			o.cfg.Store(c)
			if m.react != nil {
				if slices.Contains(diff, "react") {
					// New reaction rows start from occurred false without
					// a notification.
					m.react.Configure(c)
				} else if slices.Contains(diff, "tag") {
					// A change of tag (or owner) alone keeps the reaction
					// state, which Configure would reset (and so repeat a
					// notification); only the tag of later events changes.
					m.react.SetTag(c.ID, c.Tag)
				}
			}
			if m.store != nil {
				// Keep the statistics; only tag / owner change in the row.
				m.store.SetConfig(c)
			}
			res.Updated = append(res.Updated, c.ID)
		}
	}
	// The global section and the actions are compared with the settings in
	// force since startup, not with the previous file: a change that was not
	// applied keeps being reported until the file matches again or goipslad
	// restarts.
	warnings := globalDiff(m.effective.global, cfg.Global)
	if !sameActions(m.effective.actions, cfg.Actions) {
		warnings = append(warnings, "actions changed; the webhook and exec sinks are built at startup, so this requires a restart of goipslad to take effect")
	}
	for _, w := range warnings {
		m.logger.Warn("reload: " + w)
		res.Warnings = append(res.Warnings, w)
	}
	// Settings of the new file that are accepted but have no effect.
	for _, w := range cfg.Warnings {
		m.logger.Warn("config warning", "warning", w)
		res.Warnings = append(res.Warnings, w)
	}
	m.cfg = cfg
	m.configureTracksLocked()
	for _, l := range [][]int{res.Added, res.Removed, res.Restarted, res.Updated} {
		slices.Sort(l)
	}
	// The caller (the provider) logs the result, with where the reload came
	// from (API or SIGHUP).
	return res, nil
}

// measurementDiff lists the settings that change how an operation measures
// or schedules. Any of them rebuilds the operation.
func measurementDiff(a, b *config.Operation, now time.Time) []string {
	var d []string
	add := func(changed bool, name string) {
		if changed {
			d = append(d, name)
		}
	}
	add(a.Type != b.Type, "type")
	add(a.Target != b.Target, "target")
	add(a.SourceIP != b.SourceIP, "source-ip")
	add(a.SourceInterface != b.SourceInterface, "source-interface")
	add(a.VRF != b.VRF, "vrf")
	add(a.TOS != b.TOS, "tos")
	add(a.TrafficClass != b.TrafficClass, "traffic-class")
	add(a.FlowLabel != b.FlowLabel, "flow-label")
	add(a.Frequency != b.Frequency, "frequency")
	add(a.Timeout != b.Timeout, "timeout")
	add(a.Threshold != b.Threshold, "threshold")
	add(a.RequestDataSize != b.RequestDataSize, "request-data-size")
	add(a.DataPattern != b.DataPattern, "data-pattern")
	add(a.VerifyData != b.VerifyData, "verify-data")
	add(a.Interval != b.Interval, "interval")
	add(a.NumPackets != b.NumPackets, "num-packets")
	add(a.OneWayDelay != b.OneWayDelay, "one-way-delay")
	add(a.Stats != b.Stats, "statistics")
	add(a.History != b.History, "history")
	add(!equalPtr(a.Enhanced, b.Enhanced), "history.enhanced")
	add(!sameSchedule(a.Schedule, b.Schedule, now), "schedule")
	return d
}

// notificationDiff lists the settings that do not affect measurement.
func notificationDiff(a, b *config.Operation) []string {
	var d []string
	if a.Tag != b.Tag {
		d = append(d, "tag")
	}
	if a.Owner != b.Owner {
		d = append(d, "owner")
	}
	if !slices.Equal(a.React, b.React) {
		d = append(d, "react")
	}
	return d
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// sameSchedule compares two schedules. start-time HH:MM[:SS] is resolved by
// the configuration loader to the next occurrence after the load, so a
// reload after the start time moves At by whole days although the file did
// not change. Two "at" starts with the same local time of day are therefore
// the same when the old one has already passed and the new one is still to
// come.
func sameSchedule(a, b *config.Schedule, now time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Forever != b.Forever || (!a.Forever && a.Life != b.Life) ||
		a.Start != b.Start || a.Ageout != b.Ageout || a.Recurring != b.Recurring {
		return false
	}
	switch a.Start {
	case config.StartAfter:
		return a.After == b.After
	case config.StartAt:
		if a.At.Equal(b.At) {
			return true
		}
		al, bl := a.At.Local(), b.At.Local()
		sameClock := al.Hour() == bl.Hour() && al.Minute() == bl.Minute() && al.Second() == bl.Second() &&
			al.Nanosecond() == bl.Nanosecond()
		return sameClock && !a.At.After(now) && b.At.After(now)
	}
	return true
}

// globalDiff lists the global settings that changed. They are only read at
// startup.
func globalDiff(a, b config.Global) []string {
	var w []string
	add := func(changed bool, key string) {
		if changed {
			w = append(w, fmt.Sprintf("global.%s changed; it requires a restart of goipslad to take effect", key))
		}
	}
	add(a.APISocket != b.APISocket, "api-socket")
	add(a.APISocketGroup != b.APISocketGroup, "api-socket-group")
	add(a.MetricsListen != b.MetricsListen, "metrics-listen")
	add(a.Log != b.Log, "log")
	add(!equalPtr(a.Syslog, b.Syslog), "syslog")
	add(!sameSNMP(a.SNMP, b.SNMP), "snmp")
	return w
}

func sameSNMP(a, b *config.SNMPConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AgentX == b.AgentX && slices.Equal(a.Traps, b.Traps)
}

// sameActions compares the external notification actions.
func sameActions(a, b []config.Action) bool {
	return slices.EqualFunc(a, b, func(x, y config.Action) bool {
		return slices.Equal(x.On, y.On) && x.Track == y.Track && x.Webhook == y.Webhook && x.Exec == y.Exec
	})
}
