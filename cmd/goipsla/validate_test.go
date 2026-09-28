package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFileToValidate(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "goipslad.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const yamlToValidate = `
templates:
  wan: { type: icmp-echo, frequency: 10s, timeout: 2000ms, threshold: 300ms, tag: wan }
operations:
  - { id: 2, type: icmp-jitter, target: 192.0.2.2 }
  - { template: wan, targets: { 101: 192.0.2.11, 102: "2001:db8::12" } }
`

func TestValidateOK(t *testing.T) {
	out, errOut, err := runCtl(t, "validate", writeFileToValidate(t, yamlToValidate))
	if err != nil || out != "OK: 3 operations\n" || errOut != "" {
		t.Fatalf("out %q err %q: %v", out, errOut, err)
	}
}

func TestValidatePrintTable(t *testing.T) {
	out, _, err := runCtl(t, "validate", "--print", writeFileToValidate(t, yamlToValidate))
	if err != nil {
		t.Fatal(err)
	}
	want := "" +
		"ID   TYPE         TARGET        FREQUENCY  TIMEOUT  THRESHOLD  TAG\n" +
		"2    icmp-jitter  192.0.2.2     60s        5000ms   5000ms     \n" +
		"101  icmp-echo    192.0.2.11    10s        2000ms   300ms      wan\n" +
		"102  icmp-echo    2001:db8::12  10s        2000ms   300ms      wan\n" +
		"OK: 3 operations\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestValidatePrintJSON(t *testing.T) {
	out, _, err := runCtl(t, "validate", "--print", "-o", "json", writeFileToValidate(t, yamlToValidate))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Global struct {
			APISocket string `json:"api-socket"`
		} `json:"global"`
		Operations []map[string]any `json:"operations"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if v.Global.APISocket != "/run/goipslad/goipslad.sock" || len(v.Operations) != 3 {
		t.Fatalf("got %+v", v)
	}
	op := v.Operations[1]
	checks := map[string]any{
		"id": 101.0, "type": "icmp-echo", "target": "192.0.2.11", "template": "wan",
		"frequency": "10s", "timeout": "2000ms", "threshold": "300ms", "data-pattern": "0xABCDABCD", "tos": 0.0,
	}
	for k, want := range checks {
		if op[k] != want {
			t.Errorf("operations[1].%s = %v, want %v", k, op[k], want)
		}
	}
	if _, ok := v.Operations[0]["interval"]; !ok {
		t.Error("icmp-jitter should show interval")
	}
	if _, ok := v.Operations[0]["request-data-size"]; ok {
		t.Error("icmp-jitter should not show request-data-size")
	}
	if _, ok := v.Operations[2]["traffic-class"]; !ok {
		t.Error("IPv6 operation should show traffic-class")
	}
}

func TestValidateErrors(t *testing.T) {
	p := writeFileToValidate(t, "operations:\n  - { id: 1, type: icmp-echo, target: 192.0.2.1, timeout: 7s, frequency: 5s, tos: 300 }\n")
	out, errOut, err := runCtl(t, "validate", p)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("got %v, want exit status 1", err)
	}
	want := "operations[0].tos: must be between 0 and 255\noperations(id=1).timeout: must not exceed frequency (5s)\n"
	if out != "" || errOut != want {
		t.Errorf("stdout %q\nstderr %q\nwant stderr %q", out, errOut, want)
	}
}

func TestValidateSyntaxError(t *testing.T) {
	_, errOut, err := runCtl(t, "validate", writeFileToValidate(t, "operations: [\n"))
	var ee *exitError
	if !errors.As(err, &ee) || !strings.HasPrefix(errOut, "yaml: ") {
		t.Fatalf("err %v stderr %q", err, errOut)
	}
}

func TestValidateMissingFile(t *testing.T) {
	_, _, err := runCtl(t, "validate", filepath.Join(t.TempDir(), "nope.yaml"))
	var ee *exitError
	if err == nil || errors.As(err, &ee) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
}

const yamlWithSecretsToValidate = `
global:
  snmp:
    agentx: /var/agentx/master
    traps: [ { host: 192.0.2.10, community: s3cret } ]
templates:
  unused: { type: icmp-echo, frequency: 10s }
operations:
  - { id: 1, type: icmp-echo, target: 192.0.2.1 }
actions:
  - { on: [threshold-exceeded], webhook: "https://user:pw@hooks.example/T0/B1?token=abc" }
`

// TestValidatePrintJSONMasksSecrets: the JSON masks the SNMP communities and
// everything after the host of webhook URLs (the path can be the token)
// unless --show-secrets is given.
func TestValidatePrintJSONMasksSecrets(t *testing.T) {
	p := writeFileToValidate(t, yamlWithSecretsToValidate)
	out, _, err := runCtl(t, "validate", "--print", "-o", "json", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cret", "user:pw", "T0/B1", "token=abc"} {
		if strings.Contains(out, secret) {
			t.Errorf("masked output contains %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, `"community": "***"`) || !strings.Contains(out, `"webhook": "https://hooks.example/***"`) {
		t.Errorf("masked output:\n%s", out)
	}
	out, _, err = runCtl(t, "validate", "--print", "-o", "json", "--show-secrets", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{`"community": "s3cret"`, `"webhook": "https://user:pw@hooks.example/T0/B1?token=abc"`} {
		if !strings.Contains(out, secret) {
			t.Errorf("--show-secrets output lacks %s:\n%s", secret, out)
		}
	}
}

// TestValidateWarnings: settings without effect are warned about on stderr;
// the file is still valid.
func TestValidateWarnings(t *testing.T) {
	out, errOut, err := runCtl(t, "validate", writeFileToValidate(t, yamlWithSecretsToValidate))
	if err != nil || out != "OK: 1 operations\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	if !strings.HasPrefix(errOut, "warning: ") || !strings.Contains(errOut, "unused") {
		t.Errorf("stderr %q, want a warning about the unused template", errOut)
	}
}

// TestValidateJSONResult: -o json without --print prints the result as a
// JSON object on stdout, for success (status 0) and failure (status 1).
func TestValidateJSONResult(t *testing.T) {
	out, errOut, err := runCtl(t, "-o", "json", "validate", writeFileToValidate(t, yamlWithSecretsToValidate))
	if err != nil || errOut != "" {
		t.Fatalf("err %v stderr %q", err, errOut)
	}
	var ok struct {
		OK         bool     `json:"ok"`
		Operations *int     `json:"operations"`
		Warnings   []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &ok); err != nil || !ok.OK || ok.Operations == nil || *ok.Operations != 1 ||
		len(ok.Warnings) != 1 || !strings.Contains(ok.Warnings[0], "unused") {
		t.Errorf("success: %v\n%s", err, out)
	}

	// No warnings: an empty array, not a missing key.
	out, _, err = runCtl(t, "-o", "json", "validate", writeFileToValidate(t, yamlToValidate))
	if err != nil || !strings.Contains(out, `"warnings": []`) || !strings.Contains(out, `"operations": 3`) {
		t.Errorf("no warnings: err %v\n%s", err, out)
	}

	for _, tc := range []struct {
		name, content, want string
	}{
		{"validation", "operations:\n  - { id: 1, type: icmp-echo, target: 192.0.2.1, timeout: 7s, frequency: 5s, tos: 300 }\n",
			"operations[0].tos: must be between 0 and 255"},
		{"syntax", "operations: [\n", "yaml: "},
	} {
		out, errOut, err := runCtl(t, "-o", "json", "validate", writeFileToValidate(t, tc.content))
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code != 1 || errOut != "" {
			t.Errorf("%s: err %v stderr %q, want exit 1 and nothing on stderr", tc.name, err, errOut)
		}
		var failed struct {
			OK     *bool    `json:"ok"`
			Errors []string `json:"errors"`
		}
		if err := json.Unmarshal([]byte(out), &failed); err != nil || failed.OK == nil || *failed.OK ||
			len(failed.Errors) == 0 || !strings.HasPrefix(failed.Errors[0], tc.want) {
			t.Errorf("%s: %v\n%s", tc.name, err, out)
		}
	}

	// A file that cannot be read is a failure too.
	out, _, err = runCtl(t, "-o", "json", "validate", filepath.Join(t.TempDir(), "nope.yaml"))
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(out, `"ok": false`) || !strings.Contains(out, "no such file") {
		t.Errorf("missing file: err %v\n%s", err, out)
	}

	// With --print, -o json is still the effective configuration.
	out, _, err = runCtl(t, "-o", "json", "validate", "--print", writeFileToValidate(t, yamlToValidate))
	if err != nil || strings.Contains(out, `"ok"`) || !strings.Contains(out, `"operations": [`) {
		t.Errorf("--print: err %v\n%s", err, out)
	}
}
