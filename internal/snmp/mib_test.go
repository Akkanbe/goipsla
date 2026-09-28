package snmp

import (
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/snmp/agentx"
	"goipsla/internal/stats"
)

func TestGet(t *testing.T) {
	fake := clock.NewFake(base.Add(10 * time.Minute))
	m := newTestMIB(newFake(), fake)
	tests := []struct{ oid, want string }{
		// rttMonAppl
		{"1.1.1.0", `STRING "goipslad 1.2.3 (Round Trip Time MIB 2.2.0 compatible, ICMP only)"`},
		{"1.1.2.0", "INTEGER 16384"},
		{"1.1.4.0", "INTEGER 1000"},
		{"1.1.5.0", "INTEGER 1"},
		{"1.1.10.0", "INTEGER 997"},
		{"1.1.3.0", "noSuchObject"}, // TimeOfLastSet: not served
		{"1.1.7.1.2.1", "INTEGER 1"},
		{"1.1.7.1.2.9", "INTEGER 2"},
		{"1.1.7.1.2.16", "INTEGER 1"},
		{"1.1.8.1.2.2", "INTEGER 1"},
		{"1.1.8.1.2.34", "INTEGER 1"},
		{"1.1.8.1.2.3", "noSuchInstance"},
		// rttMonCtrlAdminTable
		{"1.2.1.1.2.11", `STRING "noc"`},
		{"1.2.1.1.3.11", `STRING "wan-uplink-prima"`},
		{"1.2.1.1.12.11", `STRING "wan-uplink-primary-01"`},
		{"1.2.1.1.4.11", "INTEGER 1"},
		{"1.2.1.1.4.31", "INTEGER 16"},
		{"1.2.1.1.5.11", "INTEGER 300"},
		{"1.2.1.1.6.11", "INTEGER 10"},
		{"1.2.1.1.7.11", "INTEGER 2000"},
		{"1.2.1.1.8.11", "INTEGER 2"},
		{"1.2.1.1.9.11", "INTEGER 1"},
		{"1.2.1.1.10.11", "INTEGER 1"},
		{"1.2.1.1.11.11", "noSuchObject"},
		{"1.2.1.1.4", "noSuchInstance"}, // a column without an index
		{"1.1.4", "noSuchInstance"},     // a scalar without .0
		{"1.1.4.0.1", "noSuchInstance"}, // a scalar with a wrong index
		{"1.2.1.1.11", "noSuchObject"},  // a column not served
		{"1.2.1.1", "noSuchObject"},     // the entry itself
		{"1.2.1", "noSuchObject"},       // the table itself
		{"1.2.2.1.3", "noSuchInstance"}, // a column that exists for some rows only
		{"1.2.1.1.4.99", "noSuchInstance"},
		// rttMonEchoAdminTable
		{"1.2.2.1.1.11", "INTEGER 2"},
		{"1.2.2.1.1.31", "INTEGER 34"},
		{"1.2.2.1.2.11", "STRING \"\\nd\\x01\\v\""}, // 0A 64 01 0B
		{"1.2.2.1.3.11", "INTEGER 28"},
		{"1.2.2.1.3.31", "noSuchInstance"}, // request-data-size: echo only
		{"1.2.2.1.6.11", "noSuchInstance"}, // no source-ip
		{"1.2.2.1.9.11", "INTEGER 184"},
		{"1.2.2.1.9.12", "INTEGER 32"}, // IPv6: traffic class
		{"1.2.2.1.17.31", "INTEGER 20"},
		{"1.2.2.1.17.11", "noSuchInstance"},
		{"1.2.2.1.18.31", "INTEGER 10"},
		{"1.2.2.1.26.11", `STRING "blue"`},
		{"1.2.2.1.37.11", "INTEGER 1"},
		{"1.2.2.1.57.11", "INTEGER 46"},
		// rttMonScheduleAdminTable
		{"1.2.5.1.1.11", "INTEGER 2147483647"},
		{"1.2.5.1.1.12", "INTEGER 3600"},
		{"1.2.5.1.2.11", "TimeTicks 6000"},
		{"1.2.5.1.2.12", "TimeTicks 0"},
		{"1.2.5.1.4.11", "INTEGER 2"},
		{"1.2.5.1.6.11", "INTEGER 2"},
		{"1.2.5.1.6.12", "INTEGER 1"},
		// rttMonStatisticsAdminTable / rttMonHistoryAdminTable
		{"1.2.7.1.1.11", "INTEGER 2"},
		{"1.2.7.1.4.11", "INTEGER 2"},
		{"1.2.7.1.5.11", "INTEGER 5"},
		{"1.2.8.1.1.11", "INTEGER 1"},
		{"1.2.8.1.2.11", "INTEGER 15"},
		{"1.2.8.1.4.11", "INTEGER 2"},
		// rttMonCtrlOperTable
		{"1.2.9.1.3.11", "TimeTicks 6000"},
		{"1.2.9.1.6.11", "INTEGER 2"},
		{"1.2.9.1.8.11", "INTEGER 9"},
		{"1.2.9.1.9.11", "INTEGER 2147483647"},
		{"1.2.9.1.9.12", "INTEGER 5400"},
		{"1.2.9.1.10.11", "INTEGER 6"},
		{"1.2.9.1.10.12", "INTEGER 4"},
		// rttMonLatestRttOperTable
		{"1.2.10.1.1.11", "Gauge32 3"},
		{"1.2.10.1.2.11", "INTEGER 1"},
		{"1.2.10.1.4.11", `STRING "OK"`},
		{"1.2.10.1.5.11", "TimeTicks 12000"},
		{"1.2.10.1.2.12", "noSuchInstance"}, // never attempted
		// rttMonReactTable
		{"1.2.19.1.2.11.1", "INTEGER 1"},
		{"1.2.19.1.2.11.2", "INTEGER 7"},
		{"1.2.19.1.3.11.1", "INTEGER 3"},
		{"1.2.19.1.4.11.1", "INTEGER 2"},
		{"1.2.19.1.4.11.2", "INTEGER 1"},
		{"1.2.19.1.5.11.1", "INTEGER 300"},
		{"1.2.19.1.7.11.1", "INTEGER 3"},
		{"1.2.19.1.7.11.2", "INTEGER 2"},
		{"1.2.19.1.8.11.2", "INTEGER 4"},
		{"1.2.19.1.9.11.1", "INTEGER 350"},
		{"1.2.19.1.10.11.1", "INTEGER 1"},
		{"1.2.19.1.10.11.2", "INTEGER 2"},
		// rttMonStatsCaptureTable (id, start 6000, path 1, hop 1, dist)
		{"1.3.1.1.5.11.6000.1.1.1", "INTEGER 7"},
		{"1.3.1.1.7.11.6000.1.1.1", "Gauge32 19"},
		{"1.3.1.1.8.11.6000.1.1.1", "Gauge32 55"},
		{"1.3.1.1.9.11.6000.1.1.1", "Gauge32 1"},
		{"1.3.1.1.10.11.6000.1.1.2", "Gauge32 5"},
		{"1.3.1.1.6.11.6000.1.1.1", "INTEGER 0"},
		{"1.3.1.1.6.11.6000.1.1.2", "INTEGER 1"},      // over thresholds of the bucket
		{"1.3.1.1.5.31.6000.1.1.1", "noSuchInstance"}, // icmp-jitter has no distribution
		// rttMonStatsCollectTable / TotalsTable
		{"1.3.2.1.2.11.6000.1.1", "INTEGER 1"},
		{"1.3.2.1.3.11.6000.1.1", "INTEGER 2"},
		{"1.3.2.1.6.11.6000.1.1", "INTEGER 1"},
		{"1.3.3.1.1.11.6000", "INTEGER 54000"}, // 9 minutes since the group started
		{"1.3.3.1.2.11.6000", "INTEGER 9"},
		// rttMonHistoryCollectionTable
		{"1.4.1.1.6.11.1.9.1", "Gauge32 3"},
		{"1.4.1.1.7.11.1.8.1", "INTEGER 4"},
		{"1.4.1.1.9.11.1.8.1", `STRING "Timeout"`},
		{"1.4.1.1.4.11.1.9.1", "TimeTicks 12000"},
		// rttMonLatestIcmpJitterOperTable
		{"1.5.4.1.1.31", "Gauge32 10"},
		{"1.5.4.1.7.31", "Gauge32 3"},
		{"1.5.4.1.23.31", "Gauge32 2"},
		{"1.5.4.1.26.31", "Gauge32 1"},
		{"1.5.4.1.31.31", "INTEGER 1"},
		{"1.5.4.1.44.31", "Gauge32 1"},
		{"1.5.4.1.1.11", "noSuchInstance"},
		// rttMonIcmpJitterStatsTable
		{"1.3.8.1.2.31.6000", "Counter32 3"},
		{"1.3.8.1.4.31.6000", "Counter32 30"},
		{"1.3.8.1.6.31.6000", "Counter32 7"},
		{"1.3.8.1.7.31.6000", "Counter32 2"},
		{"1.3.8.1.12.31.6000", "Counter32 12"},
		{"1.3.8.1.40.31.6000", "Counter32 1"},
		{"1.3.8.1.59.31.6000", "Counter32 4"},
		{"1.3.8.1.4.11.6000", "noSuchInstance"},
	}
	for _, tt := range tests {
		got := m.Get(mibOID(tt.oid)).String()
		if got != tt.want {
			t.Errorf("%s = %s, want %s", tt.oid, got, tt.want)
		}
	}
	if got := m.Get(agentx.ParseOID("1.3.6.1.2.1.1.1.0")).String(); got != "noSuchObject" {
		t.Errorf("outside the subtree: %s", got)
	}
}

// walk collects every instance under root with next.
func walk(m *mib, root agentx.OID) []agentx.OID {
	var out []agentx.OID
	cur, include := root, false
	for {
		name, _, ok := m.Next(cur, include, nil)
		if !ok || !name.HasPrefix(root) {
			return out
		}
		out = append(out, name)
		cur, include = name, false
	}
}

func TestWalk(t *testing.T) {
	m := newTestMIB(newFake(), clock.NewFake(base.Add(10*time.Minute)))
	names := walk(m, rttMonMIB)
	if len(names) < 200 {
		t.Fatalf("walk returned %d instances", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1].Compare(names[i]) >= 0 {
			t.Fatalf("not increasing at %d: %s then %s", i, names[i-1], names[i])
		}
	}
	// Every instance a walk returns is readable with Get.
	for _, n := range names {
		if v := m.Get(n); v.Exception() {
			t.Errorf("walked %s but Get says %s", n, v.String())
		}
	}
	if first := names[0].String(); first != "1.3.6.1.4.1.9.9.42.1.1.1.0" {
		t.Errorf("first instance %s", first)
	}
	// The scalars and the two tables nested in rttMonAppl interleave.
	var appl []string
	for _, n := range names {
		if n.HasPrefix(mibOID("1.1")) {
			appl = append(appl, n[len(rttMonMIB):].String())
		}
	}
	if len(appl) != 5+27+2 || appl[4] != "1.1.7.1.2.1" || appl[len(appl)-1] != "1.1.10.0" {
		t.Errorf("rttMonAppl walk: %v", appl)
	}

	// include, end bound, and a start inside a column.
	if n, _, _ := m.Next(mibOID("1.2.1.1.4.11"), true, nil); n.String() != mibOID("1.2.1.1.4.11").String() {
		t.Errorf("include: %s", n)
	}
	if n, _, _ := m.Next(mibOID("1.2.1.1.4.11"), false, nil); n.String() != mibOID("1.2.1.1.4.12").String() {
		t.Errorf("next row: %s", n)
	}
	if n, _, _ := m.Next(mibOID("1.2.1.1.4.31"), false, nil); n.String() != mibOID("1.2.1.1.5.11").String() {
		t.Errorf("next column: %s", n)
	}
	if n, _, _ := m.Next(mibOID("1.2.2.1.3.11"), false, nil); n.String() != mibOID("1.2.2.1.3.12").String() {
		t.Errorf("skip jitter row: %s", n) // 31 has no request-data-size
	}
	if _, _, ok := m.Next(mibOID("1.2.1.1.4.31"), false, mibOID("1.2.1.1.5")); ok {
		t.Error("end bound not honored")
	}
	if _, _, ok := m.Next(agentx.ParseOID("1.3.6.1.4.1.9.9.43"), false, nil); ok {
		t.Error("walked past the subtree")
	}
}

func TestViewCache(t *testing.T) {
	src := newFake()
	fake := clock.NewFake(base.Add(time.Hour))
	m := newTestMIB(src, fake)
	o := mibOID("1.2.1.1.2.11")
	if got := m.Get(o).String(); got != `STRING "noc"` {
		t.Fatal(got)
	}
	changed := *src.cfgs[11]
	changed.Owner = "ops"
	src.cfgs[11] = &changed
	fake.Advance(500 * time.Millisecond)
	if got := m.Get(o).String(); got != `STRING "noc"` {
		t.Errorf("view rebuilt too early: %s", got)
	}
	fake.Advance(600 * time.Millisecond)
	if got := m.Get(o).String(); got != `STRING "ops"` {
		t.Errorf("view not rebuilt: %s", got)
	}
}

// bigSource is 1,000 echo operations with 2 hour groups of 20 buckets and
// 15 history buckets each.
func bigSource() *fakeSource {
	f := &fakeSource{cfgs: map[int]*config.Operation{}, states: map[int]string{}, snaps: map[int]*stats.Snapshot{}}
	for i := 0; i < 1000; i++ {
		id := 1001 + i
		addr := netip.AddrFrom4([4]byte{10, 200, byte(i / 250), byte(i%250 + 1)})
		c := echoCfg(id, addr.String())
		f.ids = append(f.ids, id)
		f.cfgs[id] = c
		f.states[id] = "active"
		s := &stats.Snapshot{ID: id, LifeStart: base, Latest: stats.Latest{Valid: true, End: base, RTT: time.Millisecond, Code: op.RCOK}}
		for h := 0; h < 2; h++ {
			g := stats.HourGroup{Index: h + 1, Start: base.Add(time.Duration(h) * time.Hour)}
			for b := 0; b < 20; b++ {
				g.Dist = append(g.Dist, stats.DistBucket{Index: b + 1, Completions: 3, RTTSumMs: 9})
			}
			s.Hours = append(s.Hours, g)
		}
		for b := 0; b < 15; b++ {
			s.History = append(s.History, stats.HistoryBucket{Life: 1, Bucket: b + 1, Sample: 1, RTTMs: 1, Code: op.RCOK, Target: addr})
		}
		f.snaps[id] = s
	}
	return f
}

func TestWalk1000(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	m := newTestMIB(bigSource(), clock.NewFake(base.Add(3*time.Hour)))
	start := time.Now()
	names := walk(m, rttMonMIB)
	d := time.Since(start)
	t.Logf("in-process walk of 1000 operations: %d instances in %v", len(names), d)
	// 40,000 capture rows x 5 columns alone.
	if len(names) < 200000 {
		t.Errorf("only %d instances", len(names))
	}
}

// TestMIBAuditFixes covers audit B23 on the MIB side.
func TestMIBAuditFixes(t *testing.T) {
	// The tags are cut to octets on a character boundary.
	for _, tt := range []struct {
		in   string
		n    int
		want string
	}{
		{"wan-echo", 16, "wan-echo"},
		{"0123456789abcdefXYZ", 16, "0123456789abcdef"},
		{"東京大阪名古屋札幌", 16, "東京大阪名"},                              // 3-byte characters: 15 octets
		{strings.Repeat("é", 70), 128, strings.Repeat("é", 64)}, // 2-byte characters
	} {
		if got := octetPrefix(tt.in, tt.n); got != tt.want || !utf8.ValidString(got) {
			t.Errorf("octetPrefix(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}

	// rttMonApplProbeCapacity stays within 1..2147483647 when full.
	m := newTestMIB(bigSource(), clock.NewFake(base))
	if got := m.Get(mibOID("1.1.10.0")).String(); got != "INTEGER 1" {
		t.Errorf("capacity with 1000 operations: %s", got)
	}

	// An operation whose state cannot be read (removed meanwhile) has no row,
	// instead of a row that claims active(6).
	src := newFake()
	delete(src.states, 11)
	m = newTestMIB(src, clock.NewFake(base))
	if got := m.Get(mibOID("1.2.9.1.10.11")).String(); got != "noSuchInstance" {
		t.Errorf("rttMonCtrlOperState of a vanished operation: %s", got)
	}
}
