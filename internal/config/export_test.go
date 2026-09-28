//declscope:namespace json

package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEffectiveJSON(t *testing.T) {
	cfg := mustParse(t, "valid/full.yaml")
	byID := map[int]*Operation{}
	for _, op := range cfg.Operations {
		byID[op.ID] = op
	}

	raw, err := EffectiveJSON(byID[5]) // icmp-echo, IPv6, template, schedule
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":5,"type":"icmp-echo","target":"2001:db8::5","target-name":"2001:db8::5","template":"base",` +
		`"source-ip":"2001:db8::1","traffic-class":32,"flow-label":1048575,"frequency":"30s","timeout":"1000ms","threshold":"200ms",` +
		`"request-data-size":100,"data-pattern":"0x01020304","verify-data":true,"tag":"v6 echo","owner":"noc",` +
		`"history":{"lives-kept":1,"buckets-kept":60,"filter":"failures","hours-of-statistics-kept":25,"distributions-of-statistics-kept":20,"statistics-distribution-interval":"10ms","enhanced":{"interval":"60s","buckets":10}},` +
		`"schedule":{"life":"forever","start-time":"pending","ageout":"7200s","recurring":false},` +
		`"react":[{"element":"rtt","threshold-type":"xofy","count":5,"x":3,"y":10,"upper":5000,"lower":3000,"action":"trap-and-syslog"},` +
		`{"element":"verifyError","threshold-type":"consecutive","count":2,"x":5,"y":5,"upper":0,"lower":0,"action":"syslog"}]}`
	if string(raw) != want {
		t.Errorf("got  %s\nwant %s", raw, want)
	}

	raw, err = EffectiveJSON(byID[2]) // icmp-jitter, IPv4, after
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"request-data-size", "data-pattern", "verify-data", "traffic-class", "flow-label", "template"} {
		if _, ok := m[k]; ok {
			t.Errorf("icmp-jitter IPv4 should not have %s", k)
		}
	}
	if m["interval"] != "50ms" || m["num-packets"] != 100.0 || m["tos"] != 184.0 || m["one-way-delay"] != true {
		t.Errorf("jitter keys: %v", m)
	}
	if s := m["schedule"].(map[string]any); s["start-time"] != "after 600s" || s["life"] != "43200s" {
		t.Errorf("schedule %v", s)
	}

	// No schedule, no react: null and [].
	op := mustParse(t, "valid/defaults.yaml").Operations[0]
	raw, _ = EffectiveJSON(op)
	m = nil
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["schedule"] != nil || len(m["react"].([]any)) != 0 {
		t.Errorf("defaults: %s", raw)
	}

	// start-time "at" is RFC 3339.
	op.Schedule = &Schedule{Forever: true, Start: StartAt, At: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	raw, _ = EffectiveJSON(op)
	m = nil
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["schedule"].(map[string]any)["start-time"] != "2026-10-01T00:00:00Z" {
		t.Errorf("at: %s", raw)
	}

	// A daily start-time is shown as the time of day it was written as.
	op.Schedule = &Schedule{Life: time.Hour, Start: StartAt, At: time.Date(2026, 10, 1, 1, 30, 0, 0, time.UTC), Daily: true, Recurring: true}
	raw, _ = EffectiveJSON(op)
	m = nil
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["schedule"].(map[string]any)["start-time"] != "01:30:00" {
		t.Errorf("daily: %s", raw)
	}
}

// TestEffectiveJSONFractionalStart keeps the fraction of an RFC 3339 start
// time (audit B9).
func TestEffectiveJSONFractionalStart(t *testing.T) {
	cfg, err := parse([]byte(`operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1, schedule: { start-time: "2026-10-01T09:00:00.5+09:00" } } ]`), parseTime)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EffectiveJSON(cfg.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if got := m["schedule"].(map[string]any)["start-time"]; got != "2026-10-01T09:00:00.5+09:00" {
		t.Errorf("start-time %v", got)
	}
}

// TestEffectiveConfigJSON covers the whole-configuration view and its
// redaction of secrets.
func TestEffectiveConfigJSON(t *testing.T) {
	in := `
global:
  syslog: { facility: local3 }
  snmp: { agentx: "tcp:127.0.0.1:705", traps: [ { host: "192.0.2.9:1162", community: s3cret } ] }
operations: [ { id: 1, type: icmp-echo, target: 192.0.2.1 } ]
tracks: [ { id: 7, operation: 1, delay: { down: 3m } } ]
actions:
  - { on: [track-down], track: 7, webhook: "https://user:pw@hooks.example/services/T0/PATHTOKEN?token=abc" }
  - { on: [threshold-exceeded], exec: /usr/bin/logger }
`
	cfg, err := parse([]byte(in), parseTime)
	if err != nil {
		t.Fatal(err)
	}
	op, err := EffectiveJSON(cfg.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	head := `{"global":{"api-socket":"/run/goipslad/goipslad.sock","metrics-listen":"127.0.0.1:9818","log":{"format":"text","level":"info"},` +
		`"syslog":{"facility":"local3"},"snmp":{"agentx":"tcp:127.0.0.1:705","traps":[{"host":"192.0.2.9:1162","community":%q}]}},` +
		`"operations":[` + string(op) + `],` +
		`"tracks":[{"id":7,"operation":1,"mode":"state","delay":{"up":"0s","down":"180s"}}],` +
		`"actions":[{"on":["track-down"],"track":7,"webhook":%q},{"on":["threshold-exceeded"],"exec":"/usr/bin/logger"}]}`

	raw, err := EffectiveConfigJSON(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(head, "***", "https://hooks.example/***"); string(raw) != want {
		t.Errorf("redacted:\ngot  %s\nwant %s", raw, want)
	}
	for _, secret := range []string{"s3cret", "PATHTOKEN", "T0", "pw", "token=abc"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("redacted JSON carries %q: %s", secret, raw)
		}
	}
	raw, err = EffectiveConfigJSON(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(head, "s3cret", "https://user:pw@hooks.example/services/T0/PATHTOKEN?token=abc"); string(raw) != want {
		t.Errorf("not redacted:\ngot  %s\nwant %s", raw, want)
	}
}

func TestJSONRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"https://hooks.example":  "https://hooks.example",
		"https://hooks.example/": "https://hooks.example/",
		"https://hooks.example/services/T0/SECRET": "https://hooks.example/***", // the path is the secret
		"https://hooks.example/x":                  "https://hooks.example/***",
		"http://u:p@h:8080/p?q=1":                  "http://h:8080/***",
		"https://h/p?token=abc#frag":               "https://h/***",
		"https://h?token=abc":                      "https://h/***",
		"https://u:p@h":                            "https://h/***",
		"://not a url":                             "***",
	} {
		if got := jsonRedactURL(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		60 * time.Second: "60s", 1500 * time.Millisecond: "1500ms", 0: "0s", 1500 * time.Microsecond: "1.5ms",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %s, want %s", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		5 * time.Second: "5000ms", 20 * time.Millisecond: "20ms", 1500 * time.Microsecond: "1.5ms",
	} {
		if got := FormatMillis(d); got != want {
			t.Errorf("FormatMillis(%v) = %s, want %s", d, got, want)
		}
	}
}
