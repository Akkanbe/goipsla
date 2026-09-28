//declscope:namespace json

package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// EffectiveJSON encodes the effective configuration of o with the key names
// of the YAML file (kebab-case) and durations written as in the file ("60s",
// "5000ms"). goipsla validate --print and the API's "config" field share it.
//
// Keys that do not apply are left out: echo-only keys for icmp-jitter and
// vice versa, tos for IPv6 targets, traffic-class and flow-label for IPv4
// targets. "schedule" is null when the operation starts at load time and runs
// forever.
func EffectiveJSON(o *Operation) (json.RawMessage, error) {
	b, err := json.Marshal(newOpJSON(o))
	if err != nil {
		return nil, fmt.Errorf("encode operation %d: %w", o.ID, err)
	}
	return b, nil
}

type opJSON struct {
	ID              int           `json:"id"`
	Type            string        `json:"type"`
	Target          string        `json:"target"`
	TargetName      string        `json:"target-name"`
	Template        string        `json:"template,omitempty"`
	SourceIP        string        `json:"source-ip,omitempty"`
	SourceInterface string        `json:"source-interface,omitempty"`
	VRF             string        `json:"vrf,omitempty"`
	TOS             *uint8        `json:"tos,omitempty"`
	TrafficClass    *uint8        `json:"traffic-class,omitempty"`
	FlowLabel       *uint32       `json:"flow-label,omitempty"`
	Frequency       string        `json:"frequency"`
	Timeout         string        `json:"timeout"`
	Threshold       string        `json:"threshold"`
	RequestDataSize *int          `json:"request-data-size,omitempty"`
	DataPattern     string        `json:"data-pattern,omitempty"`
	VerifyData      *bool         `json:"verify-data,omitempty"`
	Interval        string        `json:"interval,omitempty"`
	NumPackets      *int          `json:"num-packets,omitempty"`
	OneWayDelay     *bool         `json:"one-way-delay,omitempty"`
	Tag             string        `json:"tag"`
	Owner           string        `json:"owner"`
	History         historyJSON   `json:"history"`
	Schedule        *scheduleJSON `json:"schedule"` // null: start at load time, run forever
	React           []reactJSON   `json:"react"`
}

type historyJSON struct {
	LivesKept    int           `json:"lives-kept"`
	BucketsKept  int           `json:"buckets-kept"`
	Filter       string        `json:"filter"`
	HoursKept    int           `json:"hours-of-statistics-kept"`
	DistKept     int           `json:"distributions-of-statistics-kept"`
	DistInterval string        `json:"statistics-distribution-interval"`
	Enhanced     *enhancedJSON `json:"enhanced"`
}

type enhancedJSON struct {
	Interval string `json:"interval"`
	Buckets  int    `json:"buckets"`
}

type scheduleJSON struct {
	Life      string `json:"life"`
	StartTime string `json:"start-time"`
	Ageout    string `json:"ageout"`
	Recurring bool   `json:"recurring"`
}

type reactJSON struct {
	Element       string `json:"element"`
	ThresholdType string `json:"threshold-type"`
	Count         int    `json:"count"`
	X             int    `json:"x"`
	Y             int    `json:"y"`
	Upper         int    `json:"upper"`
	Lower         int    `json:"lower"`
	Action        string `json:"action"`
}

func newOpJSON(op *Operation) opJSON {
	v := opJSON{
		ID:              op.ID,
		Type:            string(op.Type),
		Target:          op.Target.String(),
		TargetName:      op.TargetName,
		Template:        op.Template,
		SourceInterface: op.SourceInterface,
		VRF:             op.VRF,
		Frequency:       FormatDuration(op.Frequency),
		Timeout:         FormatMillis(op.Timeout),
		Threshold:       FormatMillis(op.Threshold),
		Tag:             op.Tag,
		Owner:           op.Owner,
		History: historyJSON{
			LivesKept:    op.History.Lives,
			BucketsKept:  op.History.Buckets,
			Filter:       string(op.History.Filter),
			HoursKept:    op.Stats.HoursKept,
			DistKept:     op.Stats.DistBuckets,
			DistInterval: FormatMillis(op.Stats.DistInterval),
		},
		React: []reactJSON{},
	}
	if op.SourceIP.IsValid() {
		v.SourceIP = op.SourceIP.String()
	}
	if op.Target.Is4() {
		v.TOS = &op.TOS
	} else {
		v.TrafficClass = &op.TrafficClass
		v.FlowLabel = &op.FlowLabel
	}
	switch op.Type {
	case ICMPEcho:
		v.RequestDataSize = &op.RequestDataSize
		v.DataPattern = fmt.Sprintf("0x%08X", op.DataPattern)
		v.VerifyData = &op.VerifyData
	case ICMPJitter:
		v.Interval = FormatMillis(op.Interval)
		v.NumPackets = &op.NumPackets
		v.OneWayDelay = &op.OneWayDelay
	}
	if e := op.Enhanced; e != nil {
		v.History.Enhanced = &enhancedJSON{Interval: FormatDuration(e.Interval), Buckets: e.Buckets}
	}
	if s := op.Schedule; s != nil {
		sv := &scheduleJSON{Life: "forever", StartTime: string(s.Start), Ageout: FormatDuration(s.Ageout), Recurring: s.Recurring}
		if !s.Forever {
			sv.Life = FormatDuration(s.Life)
		}
		switch s.Start {
		case StartAfter:
			sv.StartTime = "after " + FormatDuration(s.After)
		case StartAt:
			sv.StartTime = s.At.Format(time.RFC3339Nano)
			if s.Daily {
				sv.StartTime = s.At.Format("15:04:05") // as written: a time of day
			}
		}
		v.Schedule = sv
	}
	for _, r := range op.React {
		v.React = append(v.React, reactJSON(r))
	}
	return v
}

// jsonRedacted replaces a secret in EffectiveConfigJSON.
const jsonRedacted = "***"

// EffectiveConfigJSON encodes the whole effective configuration (global,
// operations, tracks, actions) with the key names of the YAML file, for
// goipsla validate --print and show config. With redact, the secrets are
// replaced by "***": the SNMP trap communities, and everything of the
// webhook URLs after the host (the path, userinfo, query or fragment may
// carry a token): "https://hooks.example/***".
func EffectiveConfigJSON(cfg *Config, redact bool) (json.RawMessage, error) {
	g := cfg.Global
	v := configJSON{
		Global: globalJSON{
			APISocket:      g.APISocket,
			APISocketGroup: g.APISocketGroup,
			MetricsListen:  g.MetricsListen,
			Log:            logJSON{Format: g.Log.Format, Level: g.Log.Level},
		},
		Operations: []json.RawMessage{},
		Tracks:     []trackJSON{},
		Actions:    []actionJSON{},
	}
	if g.Syslog != nil {
		v.Global.Syslog = &syslogJSON{Facility: g.Syslog.Facility}
	}
	if g.SNMP != nil {
		s := &snmpJSON{AgentX: g.SNMP.AgentX, Traps: []trapJSON{}}
		for _, t := range g.SNMP.Traps {
			c := t.Community
			if redact {
				c = jsonRedacted
			}
			s.Traps = append(s.Traps, trapJSON{Host: t.Host, Community: c})
		}
		v.Global.SNMP = s
	}
	for _, op := range cfg.Operations {
		raw, err := EffectiveJSON(op)
		if err != nil {
			return nil, err
		}
		v.Operations = append(v.Operations, raw)
	}
	for _, t := range cfg.Tracks {
		v.Tracks = append(v.Tracks, trackJSON{
			ID: t.ID, Operation: t.Operation, Mode: t.Mode,
			Delay: delayJSON{Up: FormatDuration(t.DelayUp), Down: FormatDuration(t.DelayDown)},
		})
	}
	for _, a := range cfg.Actions {
		w := a.Webhook
		if redact {
			w = jsonRedactURL(w)
		}
		v.Actions = append(v.Actions, actionJSON{On: a.On, Track: a.Track, Webhook: w, Exec: a.Exec})
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode configuration: %w", err)
	}
	return b, nil
}

// jsonRedactURL keeps the scheme and the host of a URL and replaces the
// rest with "***": "https://hooks.example/***". The path may be the secret
// itself (https://hooks.example/services/TOKEN), as may the userinfo, the
// query and the fragment. A URL with nothing after the host is kept.
func jsonRedactURL(s string) string {
	if s == "" {
		return "" // no webhook (an exec action)
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return jsonRedacted
	}
	if u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	return u.Scheme + "://" + u.Host + "/" + jsonRedacted
}

type configJSON struct {
	Global     globalJSON        `json:"global"`
	Operations []json.RawMessage `json:"operations"`
	Tracks     []trackJSON       `json:"tracks"`
	Actions    []actionJSON      `json:"actions"`
}

type globalJSON struct {
	APISocket      string      `json:"api-socket"`
	APISocketGroup string      `json:"api-socket-group,omitempty"`
	MetricsListen  string      `json:"metrics-listen"`
	Log            logJSON     `json:"log"`
	Syslog         *syslogJSON `json:"syslog,omitempty"`
	SNMP           *snmpJSON   `json:"snmp,omitempty"`
}

type logJSON struct {
	Format string `json:"format"`
	Level  string `json:"level"`
}

type syslogJSON struct {
	Facility string `json:"facility"`
}

type snmpJSON struct {
	AgentX string     `json:"agentx"`
	Traps  []trapJSON `json:"traps"`
}

type trapJSON struct {
	Host      string `json:"host"`
	Community string `json:"community"`
}

type trackJSON struct {
	ID        int       `json:"id"`
	Operation int       `json:"operation"`
	Mode      string    `json:"mode"`
	Delay     delayJSON `json:"delay"`
}

type delayJSON struct {
	Up   string `json:"up"`
	Down string `json:"down"`
}

type actionJSON struct {
	On      []string `json:"on"`
	Track   int      `json:"track,omitempty"`
	Webhook string   `json:"webhook,omitempty"`
	Exec    string   `json:"exec,omitempty"`
}
