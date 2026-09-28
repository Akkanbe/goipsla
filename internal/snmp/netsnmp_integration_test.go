//go:build integration

//declscope:namespace mib

package snmp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"goipsla/internal/config"
)

// TestNetSNMP runs the subagent against a real net-snmp snmpd and reads it
// back with snmpget / snmpwalk inside the snmpd container:
//
//	docker run -d --name goipsla-p7-interop -p 127.0.0.1:17705:705 \
//	  -v $PWD/staging/snmp:/etc/snmp-staging:ro -v $PWD/mibs:/mibs:ro \
//	  ipsla-staging-snmp:latest snmpd -f -Lo -C -c /etc/snmp-staging/snmpd.conf
//	GOIPSLA_AGENTX=tcp:127.0.0.1:17705 GOIPSLA_SNMPD_CONTAINER=goipsla-p7-interop \
//	  go test -tags integration -run TestNetSNMP -v ./internal/snmp
func TestNetSNMP(t *testing.T) {
	addr, container := os.Getenv("GOIPSLA_AGENTX"), os.Getenv("GOIPSLA_SNMPD_CONTAINER")
	if addr == "" || container == "" {
		t.Skip("set GOIPSLA_AGENTX and GOIPSLA_SNMPD_CONTAINER")
	}
	snmp := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("docker", append([]string{"exec", container}, args...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %v\n%s%s", args, err, out, stderr.String())
		}
		return string(out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, &config.SNMPConfig{AgentX: addr}, newFake(), Options{Version: "it", Start: time.Now().Add(-time.Hour), Logger: quiet()})
	}()

	// Wait for the registration.
	deadline := time.Now().Add(20 * time.Second)
	for {
		out := snmp("snmpget", "-v2c", "-c", "public", "-On", "localhost", "1.3.6.1.4.1.9.9.42.1.1.4.0")
		if strings.Contains(out, "INTEGER: 1000") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subagent never answered: %s", out)
		}
		time.Sleep(500 * time.Millisecond)
	}

	get := snmp("snmpget", "-v2c", "-c", "public", "localhost",
		"CISCO-RTTMON-MIB::rttMonCtrlAdminRttType.11",
		"CISCO-RTTMON-MIB::rttMonCtrlAdminRttType.31",
		"CISCO-RTTMON-MIB::rttMonEchoAdminTargetAddress.11",
		"CISCO-RTTMON-MIB::rttMonLatestRttOperSense.11",
		"CISCO-RTTMON-MIB::rttMonCtrlOperState.12",
		"CISCO-RTTMON-ICMP-MIB::rttMonLatestIcmpJitterNumRTT.31",
		"CISCO-RTTMON-MIB::rttMonEchoAdminPktDataRequestSize.31",
	)
	t.Logf("snmpget:\n%s", get)
	for _, want := range []string{
		"rttMonCtrlAdminRttType.11 = INTEGER: echo(1)",
		"rttMonCtrlAdminRttType.31 = INTEGER: icmpjitter(16)",
		"rttMonEchoAdminTargetAddress.11 = Hex-STRING: 0A 64 01 0B",
		"rttMonLatestRttOperSense.11 = INTEGER: ok(1)",
		"rttMonCtrlOperState.12 = INTEGER: pending(4)",
		"rttMonLatestIcmpJitterNumRTT.31 = Gauge32: 10",
		"rttMonEchoAdminPktDataRequestSize.31 = No Such Instance",
	} {
		if !strings.Contains(get, want) {
			t.Errorf("missing %q", want)
		}
	}

	walk := snmp("snmpwalk", "-v2c", "-c", "public", "localhost", "CISCO-RTTMON-MIB::ciscoRttMonMIB")
	lines := strings.Count(walk, "\n")
	t.Logf("snmpwalk: %d lines; first lines:\n%s", lines, strings.Join(strings.SplitN(walk, "\n", 6)[:5], "\n"))
	for _, want := range []string{
		"rttMonApplVersion.0 = STRING: goipslad it (Round Trip Time MIB 2.2.0 compatible, ICMP only)",
		"rttMonReactVar.11.1 = INTEGER: rtt(1)",
		"rttMonReactOccurred.11.1 = INTEGER: true(1)",
		"rttMonStatsCaptureCompletions.11.",
		"rttMonHistoryCollectionSense.11.1.9.1 = INTEGER: ok(1)",
		"rttMonIcmpJitterStatsNumRTTs.31.",
	} {
		if !strings.Contains(walk, want) {
			t.Errorf("walk missing %q", want)
		}
	}
	if strings.Contains(walk, "OID not increasing") || strings.Contains(walk, "No Such") {
		t.Errorf("walk reported an error:\n%s", walk)
	}
	bulk := snmp("snmpbulkwalk", "-v2c", "-c", "public", "-Cr50", "localhost", "CISCO-RTTMON-MIB::ciscoRttMonMIB")
	if strings.Count(bulk, "\n") != lines {
		t.Errorf("bulkwalk %d lines, walk %d", strings.Count(bulk, "\n"), lines)
	}
	set := exec.Command("docker", "exec", container, "snmpset", "-v2c", "-c", "public", "localhost", "CISCO-RTTMON-MIB::rttMonCtrlAdminOwner.11", "s", "x")
	out, _ := set.CombinedOutput()
	t.Logf("snmpset: %s", out)
	if !strings.Contains(string(out), "notWritable") && !strings.Contains(string(out), "noAccess") && !strings.Contains(string(out), "authorizationError") {
		t.Errorf("snmpset was not refused: %s", out)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestNetSNMP1000 times walks over 1,000 operations through snmpd.
func TestNetSNMP1000(t *testing.T) {
	addr, container := os.Getenv("GOIPSLA_AGENTX"), os.Getenv("GOIPSLA_SNMPD_CONTAINER")
	if addr == "" || container == "" {
		t.Skip("set GOIPSLA_AGENTX and GOIPSLA_SNMPD_CONTAINER")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, &config.SNMPConfig{AgentX: addr}, bigSource(), Options{Version: "it", Start: base, Logger: quiet()})
	}()
	time.Sleep(2 * time.Second)
	for _, tc := range []struct{ cmd, obj string }{
		{"snmpwalk", "CISCO-RTTMON-MIB::rttMonLatestRttOperTable"},
		{"snmpwalk", "CISCO-RTTMON-MIB::rttMonCtrlAdminTable"},
		{"snmpbulkwalk", "CISCO-RTTMON-MIB::rttMonLatestRttOperTable"},
		{"snmpbulkwalk", "CISCO-RTTMON-MIB::rttMonCtrlAdminTable"},
		{"snmpbulkwalk", "CISCO-RTTMON-MIB::ciscoRttMonMIB"},
	} {
		start := time.Now()
		out, err := exec.Command("docker", "exec", container, tc.cmd, "-v2c", "-c", "public", "-Cr50", "-On", "localhost", tc.obj).Output()
		if tc.cmd == "snmpwalk" {
			out, err = exec.Command("docker", "exec", container, tc.cmd, "-v2c", "-c", "public", "-On", "localhost", tc.obj).Output()
		}
		d := time.Since(start)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.cmd, tc.obj, err)
		}
		t.Logf("%-12s %-45s %7d instances in %v", tc.cmd, tc.obj, strings.Count(string(out), "\n"), d.Round(time.Millisecond))
		// The whole MIB is ~380,000 instances, one AgentX round trip each
		// between snmpd and the subagent: record it, check the tables.
		if d > 30*time.Second && !strings.HasSuffix(tc.obj, "ciscoRttMonMIB") {
			t.Errorf("%s %s took %v", tc.cmd, tc.obj, d)
		}
	}
}
