//declscope:core // the CISCO-RTTMON-MIB tree is the subject of the package doc: the unit the snmp package is named for

package snmp

import (
	"fmt"
	"net/netip"
	"time"
	"unicode/utf8"

	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/snmp/agentx"
	"goipsla/internal/stats"
)

// This file defines the columns of every table served, after
// CISCO-RTTMON-MIB and CISCO-RTTMON-ICMP-MIB. Columns that goipslad cannot
// fill are left out (their instances do not exist); see docs/snmp.md.

const (
	maxLife          = 2147483647 // rttMonScheduleAdminRttLife "forever"
	applVersionFmt   = "goipslad %s (Round Trip Time MIB 2.2.0 compatible, ICMP only)"
	rttTypeEcho      = 1
	rttTypeIcmpJit   = 16
	protoIcmpEcho    = 2
	protoIcmpJitter  = 34
	numRttTypes      = 27 // RttMonRttType: echo(1) .. pathJitter(27)
	rowStatusActive  = 1
	precisionMs      = 1
	applResetReady   = 1
	operStatePending = 4
	operStateInact   = 5
	operStateActive  = 6
)

// rttMonMIB is the subtree the subagent registers (ciscoRttMonMIB). The
// ICMP-MIB tables live inside it (rttMonStats 8, rttMonLatestOper 4).
//
//declscope:package // Run registers it and the trap sink names objects under it
var rttMonMIB = agentx.ParseOID("1.3.6.1.4.1.9.9.42")

// mibOID names an object under rttMonMIB, e.g. mibOID("1.2.1.1.4").
//
//declscope:package // the trap sink names the notification's objects with it
func mibOID(sub string) agentx.OID { return rttMonMIB.Join(agentx.ParseOID(sub)...) }

func rttMonTables() []*table {
	return []*table{
		applTable(),
		supportedRttTypesTable(),
		supportedProtocolsTable(),
		ctrlAdminTable(),
		echoAdminTable(),
		scheduleAdminTable(),
		statisticsAdminTable(),
		historyAdminTable(),
		ctrlOperTable(),
		latestRttOperTable(),
		reactTable(),
		statsCaptureTable(),
		statsCollectTable(),
		statsTotalsTable(),
		icmpJitterStatsTable(),
		historyCollectionTable(),
		latestIcmpJitterTable(),
	}
}

// ---- row sets ----

// opRows is one row per operation, indexed by rttMonCtrlAdminIndex.
func opRows(v *view) []row {
	rs := make([]row, 0, len(v.ids))
	for _, id := range v.ids {
		rs = append(rs, row{idx: agentx.OID{uint32(id)}, op: v.ops[id]})
	}
	return rs
}

// hourRows is one row per hour group, indexed by (id, StartTimeIndex).
func hourRows(v *view) []row {
	var rs []row
	for _, id := range v.ids {
		d := v.ops[id]
		if d.snap == nil {
			continue
		}
		for i := range d.snap.Hours {
			h := &d.snap.Hours[i]
			rs = append(rs, row{idx: agentx.OID{uint32(id), v.m.opts.ticks(h.Start)}, op: d, hour: h})
		}
	}
	return rs
}

// ---- helpers ----

func addrBytes(a netip.Addr) []byte {
	if !a.IsValid() {
		return nil
	}
	return a.AsSlice()
}

func ms(d time.Duration) uint64 {
	if d < 0 {
		return 0
	}
	return uint64(d / time.Millisecond)
}

func isJitter(r *row) bool { return r.op.cfg.Type == config.ICMPJitter }

func dsByte(c *config.Operation) uint8 {
	if c.Target.Is4() {
		return c.TOS
	}
	return c.TrafficClass
}

// low32 and high32 split a 64-bit accumulator into the MIB's two words.
func low32(n uint64) agentx.Value  { return agentx.Gauge(n & 0xFFFFFFFF) }
func high32(n uint64) agentx.Value { return agentx.Gauge(n >> 32) }

func has(v agentx.Value) func(*view, *row) (agentx.Value, bool) {
	return func(*view, *row) (agentx.Value, bool) { return v, true }
}

// ---- rttMonAppl (scalars) ----

func applTable() *table {
	return &table{
		entry: mibOID("1.1"),
		rows:  func(*view) []row { return []row{{idx: agentx.OID{0}}} },
		cols: []column{
			{1, func(v *view, _ *row) (agentx.Value, bool) {
				return agentx.OctetString(fmt.Sprintf(applVersionFmt, v.m.opts.Version)), true
			}},
			{2, has(agentx.Integer(config.MaxRequestDataSize))},
			{4, has(agentx.Integer(config.MaxOperations))},
			{5, has(agentx.Integer(applResetReady))},
			{10, func(v *view, _ *row) (agentx.Value, bool) {
				// Integer32 (1..2147483647): a full daemon still says 1.
				n := max(config.MaxOperations-len(v.ids), 1)
				return agentx.Integer(int64(n)), true
			}},
		},
	}
}

func supportedRttTypesTable() *table {
	return &table{
		entry: mibOID("1.1.7.1"),
		rows: func(*view) []row {
			rs := make([]row, 0, numRttTypes)
			for t := uint32(1); t <= numRttTypes; t++ {
				rs = append(rs, row{idx: agentx.OID{t}, n: t})
			}
			return rs
		},
		cols: []column{
			{2, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.Truth(r.n == rttTypeEcho || r.n == rttTypeIcmpJit), true
			}},
		},
	}
}

func supportedProtocolsTable() *table {
	return &table{
		entry: mibOID("1.1.8.1"),
		rows: func(*view) []row {
			return []row{{idx: agentx.OID{protoIcmpEcho}, n: protoIcmpEcho}, {idx: agentx.OID{protoIcmpJitter}, n: protoIcmpJitter}}
		},
		cols: []column{{2, has(agentx.Truth(true))}},
	}
}

// ---- rttMonCtrlAdminTable ----

func ctrlAdminTable() *table {
	return &table{
		entry: mibOID("1.2.1.1"),
		rows:  opRows,
		cols: []column{
			{2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.OctetString(r.op.cfg.Owner), true }},
			{3, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.OctetString(octetPrefix(r.op.cfg.Tag, 16)), true
			}},
			{4, func(_ *view, r *row) (agentx.Value, bool) {
				if isJitter(r) {
					return agentx.Integer(rttTypeIcmpJit), true
				}
				return agentx.Integer(rttTypeEcho), true
			}},
			{5, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(ms(r.op.cfg.Threshold)), true }},
			{6, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.ClampInt(uint64(r.op.cfg.Frequency / time.Second)), true
			}},
			{7, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(ms(r.op.cfg.Timeout)), true }},
			{8, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Truth(r.op.cfg.VerifyData), true }},
			{9, has(agentx.Integer(rowStatusActive))},
			{10, has(agentx.Truth(true))},
			{12, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.OctetString(octetPrefix(r.op.cfg.Tag, 128)), true
			}},
		},
	}
}

// ---- rttMonEchoAdminTable ----

func echoAdminTable() *table {
	return &table{
		entry: mibOID("1.2.2.1"),
		rows:  opRows,
		cols: []column{
			{1, func(_ *view, r *row) (agentx.Value, bool) {
				if isJitter(r) {
					return agentx.Integer(protoIcmpJitter), true
				}
				return agentx.Integer(protoIcmpEcho), true
			}},
			{2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Octets(addrBytes(r.op.cfg.Target)), true }},
			{3, func(_ *view, r *row) (agentx.Value, bool) {
				if isJitter(r) {
					return agentx.Value{}, false
				}
				return agentx.Integer(int64(r.op.cfg.RequestDataSize)), true
			}},
			{6, func(_ *view, r *row) (agentx.Value, bool) {
				if !r.op.cfg.SourceIP.IsValid() {
					return agentx.Value{}, false
				}
				return agentx.Octets(addrBytes(r.op.cfg.SourceIP)), true
			}},
			{9, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(int64(dsByte(r.op.cfg))), true }},
			{17, func(_ *view, r *row) (agentx.Value, bool) {
				if !isJitter(r) {
					return agentx.Value{}, false
				}
				return agentx.ClampInt(ms(r.op.cfg.Interval)), true
			}},
			{18, func(_ *view, r *row) (agentx.Value, bool) {
				if !isJitter(r) {
					return agentx.Value{}, false
				}
				return agentx.Integer(int64(r.op.cfg.NumPackets)), true
			}},
			{26, func(_ *view, r *row) (agentx.Value, bool) { return agentx.OctetString(r.op.cfg.VRF), true }},
			{37, has(agentx.Integer(precisionMs))},
			{57, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(int64(dsByte(r.op.cfg) >> 2)), true }},
		},
	}
}

// ---- rttMonScheduleAdminTable ----

// startType maps the schedule to RttMonScheduleStartType.
func startType(s *config.Schedule) int64 {
	if s == nil {
		return 2 // now
	}
	switch s.Start {
	case config.StartPending:
		return 1
	case config.StartAfter:
		return 4
	case config.StartAt:
		return 5 // specific
	}
	return 2
}

func scheduleAdminTable() *table {
	return &table{
		entry: mibOID("1.2.5.1"),
		rows:  opRows,
		cols: []column{
			{1, func(_ *view, r *row) (agentx.Value, bool) {
				s := r.op.cfg.Schedule
				if s == nil || s.Forever {
					return agentx.Integer(maxLife), true
				}
				return agentx.ClampInt(uint64(s.Life / time.Second)), true
			}},
			{2, func(v *view, r *row) (agentx.Value, bool) {
				s := r.op.cfg.Schedule
				switch {
				case s != nil && s.Start == config.StartPending:
					return agentx.TimeTicks(0), true
				case s != nil && s.Start == config.StartAt && r.op.state == "pending":
					return agentx.TimeTicks(v.m.opts.ticks(s.At)), true
				case r.op.snap != nil:
					return agentx.TimeTicks(v.m.opts.ticks(r.op.snap.LifeStart)), true
				}
				return agentx.TimeTicks(0), true
			}},
			{4, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.Truth(r.op.cfg.Schedule != nil && r.op.cfg.Schedule.Recurring), true
			}},
			{5, func(_ *view, r *row) (agentx.Value, bool) {
				if r.op.cfg.Schedule == nil {
					return agentx.Integer(0), true
				}
				return agentx.ClampInt(uint64(r.op.cfg.Schedule.Ageout / time.Second)), true
			}},
			{6, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(startType(r.op.cfg.Schedule)), true }},
		},
	}
}

// ---- rttMonStatisticsAdminTable, rttMonHistoryAdminTable ----

func statisticsAdminTable() *table {
	return &table{
		entry: mibOID("1.2.7.1"),
		rows:  opRows,
		cols: []column{
			{1, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.Integer(int64(r.op.cfg.Stats.HoursKept)), true
			}},
			{4, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.Integer(int64(r.op.cfg.Stats.DistBuckets)), true
			}},
			{5, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.ClampInt(ms(r.op.cfg.Stats.DistInterval)), true
			}},
		},
	}
}

func historyFilter(f config.HistoryFilter) int64 {
	switch f {
	case config.FilterAll:
		return 2
	case config.FilterOverThreshold:
		return 3
	case config.FilterFailures:
		return 4
	}
	return 1
}

func historyAdminTable() *table {
	return &table{
		entry: mibOID("1.2.8.1"),
		rows:  opRows,
		cols: []column{
			{1, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(int64(r.op.cfg.History.Lives)), true }},
			{2, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.Integer(int64(r.op.cfg.History.Buckets)), true
			}},
			{3, has(agentx.Integer(1))},
			{4, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.Integer(historyFilter(r.op.cfg.History.Filter)), true
			}},
		},
	}
}

// ---- rttMonCtrlOperTable ----

func operState(s string) int64 {
	switch s {
	case "pending":
		return operStatePending
	case "inactive":
		return operStateInact
	}
	return operStateActive
}

func latestIs(r *row, rc op.ReturnCode) bool {
	return r.op.snap != nil && r.op.snap.Latest.Valid && r.op.snap.Latest.Code == rc
}

// octetsInUse estimates the memory of an operation's statistics and history.
func octetsInUse(d *opData) uint64 {
	n := uint64(512)
	if d.snap != nil {
		for _, h := range d.snap.Hours {
			n += 128 + uint64(len(h.Dist))*48
		}
		n += uint64(len(d.snap.History)) * 48
	}
	return n
}

func ctrlOperTable() *table {
	return &table{
		entry: mibOID("1.2.9.1"),
		rows:  opRows,
		cols: []column{
			{1, func(v *view, r *row) (agentx.Value, bool) { return agentx.TimeTicks(lifeStartTicks(v, r)), true }},
			{3, func(v *view, r *row) (agentx.Value, bool) { return agentx.TimeTicks(lifeStartTicks(v, r)), true }},
			{4, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(octetsInUse(r.op)), true }},
			{5, has(agentx.Truth(false))},
			{6, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Truth(latestIs(r, op.RCTimeout)), true }},
			{7, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Truth(latestIs(r, op.RCOverThreshold)), true }},
			{8, func(_ *view, r *row) (agentx.Value, bool) {
				if r.op.snap == nil {
					return agentx.Integer(0), true
				}
				return agentx.ClampInt(r.op.snap.Totals.Initiations), true
			}},
			{9, func(_ *view, r *row) (agentx.Value, bool) {
				if r.op.forever {
					return agentx.Integer(maxLife), true
				}
				return agentx.ClampInt(uint64(r.op.lifeLeft / time.Second)), true
			}},
			{10, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(operState(r.op.state)), true }},
			{11, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Truth(latestIs(r, op.RCVerifyError)), true }},
		},
	}
}

func lifeStartTicks(v *view, r *row) uint32 {
	if r.op.snap == nil {
		return 0
	}
	return v.m.opts.ticks(r.op.snap.LifeStart)
}

// ---- rttMonLatestRttOperTable ----

func latestRows(v *view) []row {
	var rs []row
	for _, r := range opRows(v) {
		if r.op.snap != nil && r.op.snap.Latest.Valid {
			rs = append(rs, r)
		}
	}
	return rs
}

// octetPrefix is the longest prefix of s of at most n bytes that ends on a
// character boundary: the tag is limited in characters by the
// configuration, but rttMonCtrlAdminTag and LongTag in octets (16 and 128).
//
//declscope:package // the trap's rttMonCtrlAdminLongTag is cut the same way
func octetPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func completionMs(code op.ReturnCode, rtt time.Duration) uint64 {
	if code.HasRTT() {
		return ms(rtt)
	}
	return 0
}

func latestRttOperTable() *table {
	return &table{
		entry: mibOID("1.2.10.1"),
		rows:  latestRows,
		cols: []column{
			{1, func(_ *view, r *row) (agentx.Value, bool) {
				l := r.op.snap.Latest
				return agentx.Gauge(completionMs(l.Code, l.RTT)), true
			}},
			{2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(int64(r.op.snap.Latest.Code)), true }},
			{4, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.OctetString(r.op.snap.Latest.Code.Display()), true
			}},
			{5, func(v *view, r *row) (agentx.Value, bool) {
				return agentx.TimeTicks(v.m.opts.ticks(r.op.snap.Latest.End)), true
			}},
			{6, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Octets(addrBytes(r.op.cfg.Target)), true }},
		},
	}
}

// ---- rttMonReactTable ----

// reactVars maps react elements to RttMonReactVar.
//
//declscope:package // the trap sink sends rttMonReactVar too
var reactVars = map[string]int64{
	"rtt": 1, "jitterSDAvg": 2, "jitterDSAvg": 3, "timeout": 7, "verifyError": 9, "jitterAvg": 10,
	"packetLateArrival": 13, "packetOutOfSequence": 14, "maxOfPositiveSD": 15, "maxOfNegativeSD": 16,
	"maxOfPositiveDS": 17, "maxOfNegativeDS": 18, "successivePacketLoss": 24, "maxOfLatencyDS": 25,
	"maxOfLatencySD": 26, "latencyDSAvg": 27, "latencySDAvg": 28, "packetLoss": 29,
}

var thresholdTypes = map[string]int64{
	config.ThresholdNever: 1, config.ThresholdImmediate: 2, config.ThresholdConsecutive: 3,
	config.ThresholdXofY: 4, config.ThresholdAverage: 5,
}

// actionType maps the action to rttMonReactActionType. syslog is not an
// SNMP action (Cisco sends it through the logging configuration).
func actionType(a string) int64 {
	if a == config.ActionTrap || a == config.ActionTrapAndSyslog {
		return 2 // trapOnly
	}
	return 1 // none
}

func reactRows(v *view) []row {
	var rs []row
	for _, id := range v.ids {
		d := v.ops[id]
		for i := range d.reacts {
			rs = append(rs, row{idx: agentx.OID{uint32(id), uint32(i + 1)}, op: d, react: &d.reacts[i]})
		}
	}
	return rs
}

func reactTable() *table {
	return &table{
		entry: mibOID("1.2.19.1"),
		rows:  reactRows,
		cols: []column{
			{2, func(_ *view, r *row) (agentx.Value, bool) {
				n, ok := reactVars[r.react.Element]
				return agentx.Integer(n), ok
			}},
			{3, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.Integer(thresholdTypes[r.react.ThresholdType]), true
			}},
			{4, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(actionType(r.react.Action)), true }},
			{5, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.ClampInt(uint64(max(r.react.Upper, 0))), true
			}},
			{6, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.ClampInt(uint64(max(r.react.Lower, 0))), true
			}},
			{7, func(_ *view, r *row) (agentx.Value, bool) {
				if r.react.ThresholdType == config.ThresholdXofY {
					return agentx.Integer(int64(r.react.X)), true
				}
				return agentx.Integer(int64(r.react.Count)), true
			}},
			{8, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(int64(r.react.Y)), true }},
			{9, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.ClampInt(uint64(max(r.react.Value, 0))), true
			}},
			{10, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Truth(r.react.Occurred), true }},
			{11, has(agentx.Integer(rowStatusActive))},
		},
	}
}

// ---- statistics: rttMonStatsCaptureTable, CollectTable, TotalsTable ----

func captureRows(v *view) []row {
	var rs []row
	for _, h := range hourRows(v) {
		for i := range h.hour.Dist {
			b := &h.hour.Dist[i]
			rs = append(rs, row{idx: h.idx.Join(1, 1, uint32(b.Index)), op: h.op, hour: h.hour, dist: b})
		}
	}
	return rs
}

func statsCaptureTable() *table {
	return &table{
		entry: mibOID("1.3.1.1"),
		rows:  captureRows,
		cols: []column{
			{5, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(r.dist.Completions), true }},
			{6, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(r.dist.OverThresholds), true }},
			{7, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(r.dist.RTTSumMs), true }},
			{8, func(_ *view, r *row) (agentx.Value, bool) { return low32(r.dist.RTTSum2Ms), true }},
			{9, func(_ *view, r *row) (agentx.Value, bool) { return high32(r.dist.RTTSum2Ms), true }},
			{10, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(r.dist.RTTMaxMs), true }},
			{11, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(r.dist.RTTMinMs), true }},
		},
	}
}

func collectRows(v *view) []row {
	rs := hourRows(v)
	for i := range rs {
		rs[i].idx = rs[i].idx.Join(1, 1)
	}
	return rs
}

func statsCollectTable() *table {
	return &table{
		entry: mibOID("1.3.2.1"),
		rows:  collectRows,
		cols: []column{
			{1, has(agentx.Integer(0))},
			{2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(r.hour.Counters.Timeouts), true }},
			{3, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(r.hour.Counters.Busies), true }},
			{4, has(agentx.Integer(0))},
			{5, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(r.hour.Counters.Drops), true }},
			{6, func(_ *view, r *row) (agentx.Value, bool) {
				return agentx.ClampInt(r.hour.Counters.SequenceErrors), true
			}},
			{7, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(r.hour.Counters.VerifyErrors), true }},
			{8, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Octets(addrBytes(r.op.cfg.Target)), true }},
		},
	}
}

func statsTotalsTable() *table {
	return &table{
		entry: mibOID("1.3.3.1"),
		rows:  hourRows,
		cols: []column{
			{1, func(v *view, r *row) (agentx.Value, bool) {
				// TimeInterval: hundredths of a second since the group started.
				d := v.at.Sub(r.hour.Start)
				if d < 0 {
					d = 0
				}
				return agentx.ClampInt(uint64(d / (10 * time.Millisecond))), true
			}},
			{2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.ClampInt(r.hour.Counters.Initiations), true }},
		},
	}
}

// ---- rttMonHistoryCollectionTable ----

func historyRows(v *view) []row {
	var rs []row
	for _, id := range v.ids {
		d := v.ops[id]
		if d.snap == nil {
			continue
		}
		for i := range d.snap.History {
			b := &d.snap.History[i]
			rs = append(rs, row{idx: agentx.OID{uint32(id), uint32(b.Life), uint32(b.Bucket), uint32(b.Sample)}, op: d, hist: b})
		}
	}
	return rs
}

func historyCollectionTable() *table {
	return &table{
		entry: mibOID("1.4.1.1"),
		rows:  historyRows,
		cols: []column{
			{4, func(v *view, r *row) (agentx.Value, bool) {
				return agentx.TimeTicks(v.m.opts.ticks(r.hist.Start)), true
			}},
			{5, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Octets(addrBytes(r.hist.Target)), true }},
			{6, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(r.hist.RTTMs), true }},
			{7, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Integer(int64(r.hist.Code)), true }},
			{9, func(_ *view, r *row) (agentx.Value, bool) { return agentx.OctetString(r.hist.Code.Display()), true }},
		},
	}
}

// ---- CISCO-RTTMON-ICMP-MIB ----

// sideCols returns the Min, Max, Num, Sum, Sum2 columns of one jitter side,
// numbered from first, reading the side through get.
func sideCols(first uint32, get func(*row) (op.JitterSide, bool)) []column {
	f := func(pick func(op.JitterSide) uint64) func(*view, *row) (agentx.Value, bool) {
		return func(_ *view, r *row) (agentx.Value, bool) {
			s, ok := get(r)
			return agentx.Gauge(pick(s)), ok
		}
	}
	return []column{
		{first, f(func(s op.JitterSide) uint64 { return s.MinMs })},
		{first + 1, f(func(s op.JitterSide) uint64 { return s.MaxMs })},
		{first + 2, f(func(s op.JitterSide) uint64 { return s.Num })},
		{first + 3, f(func(s op.JitterSide) uint64 { return s.SumMs })},
		{first + 4, f(func(s op.JitterSide) uint64 { return s.Sum2Ms })},
	}
}

func latestJitter(r *row) (*op.JitterResult, bool) {
	if r.op.snap == nil || r.op.snap.Latest.Jitter == nil {
		return nil, false
	}
	return r.op.snap.Latest.Jitter, true
}

func sideAvg(s op.JitterSide) uint64 {
	if s.Num == 0 {
		return 0
	}
	return s.SumMs / s.Num
}

func latestIcmpJitterTable() *table {
	lj := func(pick func(*op.JitterResult) uint64) func(*view, *row) (agentx.Value, bool) {
		return func(_ *view, r *row) (agentx.Value, bool) {
			j, ok := latestJitter(r)
			if !ok {
				return agentx.Value{}, false
			}
			return agentx.Gauge(pick(j)), true
		}
	}
	side := func(pick func(*op.JitterResult) op.JitterSide) func(*row) (op.JitterSide, bool) {
		return func(r *row) (op.JitterSide, bool) {
			j, ok := latestJitter(r)
			if !ok {
				return op.JitterSide{}, false
			}
			return pick(j), true
		}
	}
	cols := []column{
		{1, lj(func(j *op.JitterResult) uint64 { return j.NumRTT })},
		{2, lj(func(j *op.JitterResult) uint64 { return j.RTTSumMs })},
		{3, lj(func(j *op.JitterResult) uint64 { return j.RTTSum2Ms })},
		{4, lj(func(j *op.JitterResult) uint64 { return j.RTTMinMs })},
		{5, lj(func(j *op.JitterResult) uint64 { return j.RTTMaxMs })},
	}
	// 6..25: PosSD, NegSD, PosDS, NegDS as Min, Max, Num, Sum, Sum2.
	cols = append(cols, sideCols(6, side(func(j *op.JitterResult) op.JitterSide { return j.PosSD }))...)
	cols = append(cols, sideCols(11, side(func(j *op.JitterResult) op.JitterSide { return j.NegSD }))...)
	cols = append(cols, sideCols(16, side(func(j *op.JitterResult) op.JitterSide { return j.PosDS }))...)
	cols = append(cols, sideCols(21, side(func(j *op.JitterResult) op.JitterSide { return j.NegDS }))...)
	cols = append(cols,
		column{26, lj(func(j *op.JitterResult) uint64 { return j.PktLoss })},
		column{27, lj(func(j *op.JitterResult) uint64 { return j.PktOutSeqBoth })},
		column{28, lj(func(j *op.JitterResult) uint64 { return j.PktOutSeqSD })},
		column{29, lj(func(j *op.JitterResult) uint64 { return j.PktOutSeqDS })},
		column{30, lj(func(j *op.JitterResult) uint64 { return uint64(max(j.Skipped, 0)) })},
		column{31, func(_ *view, r *row) (agentx.Value, bool) {
			if _, ok := latestJitter(r); !ok {
				return agentx.Value{}, false
			}
			return agentx.Integer(int64(r.op.snap.Latest.Code)), true
		}},
		column{32, lj(func(j *op.JitterResult) uint64 { return j.PktLateArrival })},
		column{33, lj(func(j *op.JitterResult) uint64 { return j.MinSucPktLoss })},
		column{34, lj(func(j *op.JitterResult) uint64 { return j.MaxSucPktLoss })},
		column{35, lj(func(j *op.JitterResult) uint64 { return j.OWSD.SumMs })},
		column{36, lj(func(j *op.JitterResult) uint64 { return j.OWSD.Sum2Ms })},
		column{37, lj(func(j *op.JitterResult) uint64 { return j.OWSD.MinMs })},
		column{38, lj(func(j *op.JitterResult) uint64 { return j.OWSD.MaxMs })},
		column{39, lj(func(j *op.JitterResult) uint64 { return j.OWDS.SumMs })},
		column{40, lj(func(j *op.JitterResult) uint64 { return j.OWDS.Sum2Ms })},
		column{41, lj(func(j *op.JitterResult) uint64 { return j.OWDS.MinMs })},
		column{42, lj(func(j *op.JitterResult) uint64 { return j.OWDS.MaxMs })},
		column{43, lj(func(j *op.JitterResult) uint64 { return j.NumOW })},
		column{44, lj(func(j *op.JitterResult) uint64 { return uint64(j.AvgJitterMs()) })},
		column{45, lj(func(j *op.JitterResult) uint64 { return uint64(j.AvgSDJitterMs()) })},
		column{46, lj(func(j *op.JitterResult) uint64 { return uint64(j.AvgDSJitterMs()) })},
		column{47, lj(func(j *op.JitterResult) uint64 { return sideAvg(j.OWSD) })},
		column{48, lj(func(j *op.JitterResult) uint64 { return sideAvg(j.OWDS) })},
		column{49, lj(func(*op.JitterResult) uint64 { return 0 })}, // IAJOut: UDP jitter only
		column{50, lj(func(*op.JitterResult) uint64 { return 0 })}, // IAJIn
	)
	return &table{entry: mibOID("1.5.4.1"), rows: opRows, cols: cols}
}

func jitterHourRows(v *view) []row {
	var rs []row
	for _, h := range hourRows(v) {
		if h.hour.Jitter != nil {
			rs = append(rs, h)
		}
	}
	return rs
}

func icmpJitterStatsTable() *table {
	jc := func(pick func(*stats.JitterCounters) uint64, mk func(uint64) agentx.Value) func(*view, *row) (agentx.Value, bool) {
		return func(_ *view, r *row) (agentx.Value, bool) { return mk(pick(r.hour.Jitter)), true }
	}
	side := func(first uint32, pick func(*stats.JitterCounters) op.JitterSide) []column {
		s := func(r *row) op.JitterSide { return pick(r.hour.Jitter) }
		return []column{
			{first, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(s(r).MinMs), true }},
			{first + 1, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(s(r).MaxMs), true }},
			{first + 2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(s(r).Num), true }},
			{first + 3, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(s(r).SumMs), true }},
			{first + 4, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(s(r).Sum2Ms & 0xFFFFFFFF), true }},
			{first + 5, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(s(r).Sum2Ms >> 32), true }},
		}
	}
	ow := func(first uint32, pick func(*stats.JitterCounters) op.JitterSide) []column {
		s := func(r *row) op.JitterSide { return pick(r.hour.Jitter) }
		return []column{
			{first, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(s(r).SumMs), true }},
			{first + 1, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(s(r).Sum2Ms & 0xFFFFFFFF), true }},
			{first + 2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(s(r).Sum2Ms >> 32), true }},
			{first + 3, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(s(r).MinMs), true }},
			{first + 4, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Gauge(s(r).MaxMs), true }},
		}
	}
	cols := []column{
		{2, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(r.hour.Counters.Completions), true }},
		{3, jc(func(j *stats.JitterCounters) uint64 { return j.NumOverThreshold }, agentx.Counter32)},
		{4, jc(func(j *stats.JitterCounters) uint64 { return j.NumRTT }, agentx.Counter32)},
		{5, jc(func(j *stats.JitterCounters) uint64 { return j.RTTSumMs }, agentx.Counter32)},
		{6, jc(func(j *stats.JitterCounters) uint64 { return j.RTTSum2Ms & 0xFFFFFFFF }, agentx.Counter32)},
		{7, jc(func(j *stats.JitterCounters) uint64 { return j.RTTSum2Ms >> 32 }, agentx.Counter32)},
		{8, jc(func(j *stats.JitterCounters) uint64 { return j.RTTMinMs }, agentx.Gauge)},
		{9, jc(func(j *stats.JitterCounters) uint64 { return j.RTTMaxMs }, agentx.Gauge)},
	}
	// 10..33: PosSD, NegSD, PosDS, NegDS as Min, Max, Num, Sum, Sum2Low, Sum2High.
	cols = append(cols, side(10, func(j *stats.JitterCounters) op.JitterSide { return j.PosSD })...)
	cols = append(cols, side(16, func(j *stats.JitterCounters) op.JitterSide { return j.NegSD })...)
	cols = append(cols, side(22, func(j *stats.JitterCounters) op.JitterSide { return j.PosDS })...)
	cols = append(cols, side(28, func(j *stats.JitterCounters) op.JitterSide { return j.NegDS })...)
	cols = append(cols,
		column{34, jc(func(j *stats.JitterCounters) uint64 { return j.PktLoss }, agentx.Counter32)},
		column{35, jc(func(j *stats.JitterCounters) uint64 { return j.PktOutSeqBoth }, agentx.Counter32)},
		column{36, jc(func(j *stats.JitterCounters) uint64 { return j.PktOutSeqSD }, agentx.Counter32)},
		column{37, jc(func(j *stats.JitterCounters) uint64 { return j.PktOutSeqDS }, agentx.Counter32)},
		column{38, jc(func(j *stats.JitterCounters) uint64 { return j.Skipped }, agentx.Counter32)},
		column{39, has(agentx.Counter32(0))},
		column{40, func(_ *view, r *row) (agentx.Value, bool) { return agentx.Counter32(r.hour.Counters.Busies), true }},
	)
	// 41..50: one-way SD and DS as Sum, Sum2Low, Sum2High, Min, Max.
	cols = append(cols, ow(41, func(j *stats.JitterCounters) op.JitterSide { return j.OWSD })...)
	cols = append(cols, ow(46, func(j *stats.JitterCounters) op.JitterSide { return j.OWDS })...)
	cols = append(cols,
		column{51, jc(func(j *stats.JitterCounters) uint64 { return j.NumOW }, agentx.Counter32)},
		column{52, jc(func(j *stats.JitterCounters) uint64 { return uint64(j.AvgJitterMs()) }, agentx.Gauge)},
		column{53, jc(func(j *stats.JitterCounters) uint64 { return uint64(j.AvgSDJitterMs()) }, agentx.Gauge)},
		column{54, jc(func(j *stats.JitterCounters) uint64 { return uint64(j.AvgDSJitterMs()) }, agentx.Gauge)},
		column{55, jc(func(j *stats.JitterCounters) uint64 { return j.MinSucPktLoss }, agentx.Gauge)},
		column{56, jc(func(j *stats.JitterCounters) uint64 { return j.MaxSucPktLoss }, agentx.Gauge)},
		column{57, has(agentx.Gauge(0))}, // IAJOut: UDP jitter only
		column{58, has(agentx.Gauge(0))}, // IAJIn
		column{59, jc(func(j *stats.JitterCounters) uint64 { return j.PktLateArrival }, agentx.Counter32)},
	)
	return &table{entry: mibOID("1.3.8.1"), rows: jitterHourRows, cols: cols}
}
