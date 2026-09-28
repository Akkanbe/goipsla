# SNMP reference

`goipslad` connects to net-snmp's snmpd as an **AgentX subagent** (RFC 2741) and exposes the ICMP-related tables of CISCO-RTTMON-MIB and CISCO-RTTMON-ICMP-MIB **read-only**.
Threshold reaction notifications are sent as **SNMPv2c traps** (`rttMonNotificationV2`).
The MIB files themselves are not part of the repository; see "MIB files" below for where to obtain them and how the staging environment uses them.

## Configuration

```yaml
global:
  snmp:
    agentx: tcp:10.100.1.20:705        # default /var/agentx/master
    traps:
      - { host: 192.0.2.10, community: public }          # default port 162
      - { host: "[2001:db8::10]:1162", community: nms }
```

- `agentx` uses the same syntax as net-snmp's `agentXSocket`: one of `/path`, `unix:/path`, `tcp:host:port`, or `host:port`. `traps[].host` is one of `host`, `host:port`, `[IPv6]:port`, `[IPv6]`, or an IPv6 address without brackets. Both are checked when the configuration is loaded (`docs/config.md`).
- If `global.snmp` is not set, neither the subagent nor traps run.
- **Failing to connect to snmpd does not stop goipslad from starting.** It reconnects every 10 seconds. After reconnecting, it starts over from Open and Register. The logs are as follows.
  - `agentx session open` (info): emitted each time a session is opened. Carries `session`, the number of failures so far `failures`, and the time without a session `down_for`.
  - `agentx session lost; reconnecting` (warn): emitted each time an open session is lost (for example, when snmpd restarts). Carries `session`, the time the session was open `up_for`, and `err`.
  - `agentx master not reachable; retrying` (warn): when it cannot connect. Emitted only the first time and then once every 30 times.
  - `agentx registration refused` (error, with the same thinning): when snmpd rejects Open or Register with an error (for example `register 1.3.6.1.4.1.9.9.42: master answered duplicateRegistration (263)`). This is a configuration problem and retrying does not fix it.
  - On shutdown, it closes the connection immediately, even while waiting for a response to Open or Register.
- **Traps are sent to every entry in `traps`.** There is no retry. Targets that could not be sent to are reported as an event delivery failure (warn `event delivery failed`, sink `snmp-trap`, with the per-target errors joined; thinned to at most once per minute). Per-target failures and successful sends are logged at debug.
- **The community is not logged.** gosnmp's debug output (whose sent packets include the community) is not connected.

### Example snmpd configuration

```
master agentx
agentXSocket tcp:0.0.0.0:705      # or /var/agentx/master (default)
agentXTimeout 5
rocommunity public default
```

- goipslad registers the `1.3.6.1.4.1.9.9.42` (ciscoRttMonMIB) subtree with priority 127.
- The two ICMP-MIB tables are also inside this subtree (`rttMonStats.8`, `rttMonLatestOper.4`).
- SET is not accepted (TestSet returns `notWritable`). With snmpd's `rocommunity`, SET does not reach the subagent in the first place. As RFC 2741 specifies, there is no response to CleanupSet.
- Only the default context is registered. Requests for a non-default context get `unsupportedContext`.
- GetBulk responses are cut off at 1,000 varbinds in total.

## Time base

TimeStamp and TimeTicks (`rttMonStatsCaptureStartTimeIndex`, `rttMonLatestRttOperTime`, and so on) count **hundredths of a second since goipslad started**. The sysUpTime carried in AgentX Responses uses the same base.

This differs from the base of snmpd's own `sysUpTime.0` (**deviation**). This is because snmpd and goipslad start at different times; to find the difference, compare with the start time in `goipsla health`.

## Indexes

| Table | Index |
|---|---|
| Per-operation tables | `rttMonCtrlAdminIndex` = operation ID |
| `rttMonReactTable` | ID, `rttMonReactConfigIndex` (from 1, in the order of the `react` entries) |
| `rttMonStatsCaptureTable` | ID, StartTimeIndex (TimeStamp of the start of the hour group), PathIndex = 1, HopIndex = 1, DistIndex (distribution bucket number) |
| `rttMonStatsCollectTable` | ID, StartTimeIndex, PathIndex = 1, HopIndex = 1 |
| `rttMonStatsTotalsTable`, `rttMonIcmpJitterStatsTable` | ID, StartTimeIndex |
| `rttMonHistoryCollectionTable` | ID, LifeIndex, BucketIndex, SampleIndex = 1 |

## Address encoding

`RttMonTargetAddress` values are returned as 4 octets for IPv4 (for example `0A 64 01 0B`) and 16 octets for IPv6.

The MIB does not define the encoding of IPv6 targets (**deviation**; Cisco's implementation is also said to return 16 octets, but this is not documented).

## Exposed tables and columns

Columns not listed in the tables are **not returned** (Get returns noSuchObject / noSuchInstance, and they do not appear in a walk). Columns without a value are not filled with 0.

### rttMonAppl (scalars, `.1.1`)

| Column | Value |
|---|---|
| `rttMonApplVersion` (1) | `goipslad <version> (Round Trip Time MIB 2.2.0 compatible, ICMP only)` |
| `rttMonApplMaxPacketDataSize` (2) | 16384 |
| `rttMonApplNumCtrlAdminEntry` (4) | 1000 (the maximum number of operations that can be configured) |
| `rttMonApplReset` (5) | ready(1) |
| `rttMonApplProbeCapacity` (10) | 1000 − number of operations. With 1,000 operations it is 1, to stay in the MIB's range (1..2147483647) |
| `rttMonApplSupportedRttTypesTable` (7) | Rows for all 27 types. Only echo(1) and icmpjitter(16) are true |
| `rttMonApplSupportedProtocolsTable` (8) | Two rows, ipIcmpEcho(2) and icmpJitterAppl(34) (both true) |

Columns not returned: TimeOfLastSet, PreConfigedReset, PreConfigedTable, FreeMemLowWaterMark, LatestSetError, Responder, AuthTable, LpdGrpStatsReset.

### rttMonCtrlAdminTable (`.1.2.1`)

| Column | Value |
|---|---|
| Owner (2) | `owner` |
| Tag (3) | The first 16 octets of `tag` (it is not cut in the middle of a character, so a non-ASCII tag can be shorter than 16) |
| RttType (4) | echo(1) / icmpjitter(16) |
| Threshold (5) | `threshold` (ms) |
| Frequency (6) | `frequency` (seconds) |
| Timeout (7) | `timeout` (ms) |
| VerifyData (8) | `verify-data` |
| Status (9) | active(1) |
| Nvgen (10) | true |
| LongTag (12) | `tag` (to match the MIB's SIZE(0..128), anything beyond 128 octets is cut at a character boundary; the same applies to LongTag in traps) |

Columns not returned: GroupName (11).

### rttMonEchoAdminTable (`.1.2.2`)

| Column | Value |
|---|---|
| Protocol (1) | ipIcmpEcho(2) / icmpJitterAppl(34) |
| TargetAddress (2) | Target (4 / 16 octets) |
| PktDataRequestSize (3) | `request-data-size` (icmp-echo only) |
| SourceAddress (6) | `source-ip` (only when set) |
| TOS (9) | `tos` for IPv4, `traffic-class` for IPv6 (**deviation**: the column is named TOS, but the same DS byte is returned) |
| Interval (17), NumPackets (18) | `interval` (ms), `num-packets` (icmp-jitter only) |
| VrfName (26) | `vrf` (empty string if not set) |
| Precision (37) | milliseconds(1) |
| Dscp (57) | The upper 6 bits of TOS (or traffic-class) |

Columns not returned: all others (columns for UDP / HTTP / voice / MPLS / Ethernet, TargetAddressString, ControlEnable, ProbePakPriority, OWNTPSyncTol*, LSPSelector, and so on).

### rttMonScheduleAdminTable (`.1.2.5`)

| Column | Value |
|---|---|
| RttLife (1) | `life` in seconds. 2147483647 for `forever` or when `schedule` is omitted |
| RttStartTime (2) | 0 if pending. If `start-time` is a date and time that has not started yet, that time. Otherwise the start time of the current life (all as TimeTicks since startup) |
| RttRecurring (4) | `recurring` |
| ConceptRowAgeoutV2 (5) | `ageout` in seconds |
| StartType (6) | pending(1) / now(2) / after(4) / specific(5). now(2) when `schedule` is omitted |

Columns not returned: ConceptRowAgeout (3, deprecated), StartDelay (7, there is no random).

### rttMonStatisticsAdminTable (`.1.2.7`), rttMonHistoryAdminTable (`.1.2.8`)

| Table | Column | Value |
|---|---|---|
| StatisticsAdmin | NumHourGroups (1), NumDistBuckets (4), DistInterval (5) | `hours-of-statistics-kept`, `distributions-of-statistics-kept`, `statistics-distribution-interval` (ms) |
| HistoryAdmin | NumLives (1), NumBuckets (2), NumSamples (3), Filter (4) | `lives-kept`, `buckets-kept`, 1, none(1) / all(2) / overThreshold(3) / failures(4) |

Columns not returned: NumPaths, NumHops (for path-echo).

### rttMonCtrlOperTable (`.1.2.9`)

| Column | Value |
|---|---|
| ModificationTime (1), ResetTime (3) | Start time of the current life (the same value, because recreating the operation on a configuration change restarts the life) |
| OctetsInUse (4) | Approximate bytes used by statistics and history |
| ConnectionLostOccurred (5) | false (ICMP has no connections) |
| TimeoutOccurred (6), OverThresholdOccurred (7), VerifyErrorOccurred (11) | Whether the latest result is timeout / overThreshold / verifyError, respectively |
| NumRtts (8) | Number of attempts in the current life |
| RttLife (9) | Remaining life in seconds. 2147483647 for forever |
| State (10) | pending(4) / inactive(5) / active(6) |

Columns not returned: DiagText (2).

### rttMonLatestRttOperTable (`.1.2.10`)

An operation that has never made an attempt has no row.

| Column | Value |
|---|---|
| CompletionTime (1) | RTT (ms, truncated). 0 for anything other than ok / overThreshold |
| Sense (2) | Return code (RttResponseSense) |
| SenseDescription (4) | Display name such as `OK` or `Timeout` |
| Time (5) | Completion time (TimeStamp) |
| Address (6) | Target |

Columns not returned: ApplSpecificSense (3).

### rttMonReactTable (`.1.2.19`)

| Column | Value |
|---|---|
| Var (2) | Element (rtt(1), jitterSDAvg(2), jitterDSAvg(3), timeout(7), verifyError(9), jitterAvg(10), packetLateArrival(13), packetOutOfSequence(14), maxOfPositiveSD(15) ... maxOfNegativeDS(18), successivePacketLoss(24), maxOfLatencyDS(25), maxOfLatencySD(26), latencyDSAvg(27), latencySDAvg(28), packetLoss(29)) |
| ThresholdType (3) | never(1) / immediate(2) / consecutive(3) / xOfy(4) / average(5) |
| ActionType (4) | trapOnly(2) if `action` is trap / trap-and-syslog, none(1) if none / syslog (because in Cisco, syslog is a mechanism outside SNMP) |
| ThresholdRising (5), ThresholdFalling (6) | `upper`, `lower` |
| ThresholdCountX (7) | `x` for xofy, otherwise `count` |
| ThresholdCountY (8) | `y` |
| Value (9) | Last evaluated value |
| Occurred (10) | Whether the reaction is currently occurring |
| Status (11) | active(1) |

### Statistics: rttMonStatsCaptureTable (`.1.3.1`), CollectTable (`.1.3.2`), TotalsTable (`.1.3.3`)

| Table | Column | Value |
|---|---|---|
| Capture | Completions (5), OverThresholds (6), SumCompletionTime (7), SumCompletionTime2Low / High (8, 9), CompletionTimeMax / Min (10, 11) | Values of the distribution bucket (ms). The sum of squares is split from 64 bits into two 32-bit halves. icmp-echo only |
| Collect | NumDisconnects (1) = 0, Timeouts (2), Busies (3), NoConnections (4) = 0, Drops (5), SequenceErrors (6), VerifyErrors (7), Address (8) | Hour group counters |
| Totals | ElapsedTime (1), Initiations (2) | Time elapsed since the start of the hour group (TimeInterval, hundredths of a second), number of attempts |

OverThresholds (6) is the number of completions in that bucket that exceeded the threshold (a subset of Completions).

Columns not returned: the ControlEnableErrors / RetrieveErrors family in Collect (9 to 12).

### rttMonHistoryCollectionTable (`.1.4.1`)

| Column | Value |
|---|---|
| SampleTime (4) | Start time of the attempt |
| Address (5) | Target |
| CompletionTime (6) | RTT (ms; 0 for anything other than ok) |
| Sense (7) | Return code |
| SenseDescription (9) | Display name |

Columns not returned: ApplSpecificSense (8).

### CISCO-RTTMON-ICMP-MIB

| Table | Rows | Columns |
|---|---|---|
| `rttMonLatestIcmpJitterOperTable` (`.1.5.4`) | icmp-jitter operations that have completed at least one burst | All columns 1 to 50. IAJOut / IAJIn (49, 50) are 0 (values for UDP jitter, which ICMP does not have) |
| `rttMonIcmpJitterStatsTable` (`.1.3.8`) | One per hour group of icmp-jitter | All columns 2 to 59. Completions is the number of bursts, OverThresholds is the number of packets whose RTT exceeded the threshold, Errors (39) is 0, Busies (40) is the hour group's busy count, IAJOut / IAJIn (57, 58) are 0 |

- The one-way delay columns (OW*) have non-zero values only when `one-way-delay: true`.
- 64-bit sums of squares are split into two columns, Low / High.
- Gauge32 values saturate at 2^32 − 1. Counter32 values wrap at 2^32.

## Traps

Of the threshold reaction events `threshold-exceeded` / `threshold-cleared`, those whose reaction `action` is `trap` or `trap-and-syslog` are sent as SNMPv2c traps of `rttMonNotificationV2` (`1.3.6.1.4.1.9.9.42.2.0.8`). Tracking changes (track-up / track-down) are not sent as traps (Cisco's EOT uses a different MIB).

| varbind | Instance | Value |
|---|---|---|
| `sysUpTime.0` | | TimeTicks since startup |
| `snmpTrapOID.0` | | `rttMonNotificationV2` |
| `rttMonCtrlAdminLongTag` | ID | Tag |
| `rttMonHistoryCollectionAddress` | ID.1.1.1 | Target address |
| `rttMonReactVar` | ID.ConfigIndex | Element |
| `rttMonReactOccurred` | ID.ConfigIndex | true(1) for exceeded, false(2) for cleared |
| `rttMonReactValue` | ID.ConfigIndex | The value that decided the transition |
| `rttMonReactThresholdRising` / `Falling` | ID.ConfigIndex | `upper` / `lower` |
| `rttMonEchoAdminLSPSelector` | ID | Empty string |

- **Handling of `rttMonHistoryCollectionAddress` (deviation).** The target address is included even when history is not being collected. The instance is fixed to `ID.1.1.1`, because the history bucket cannot be determined from the event.
- **ConfigIndex.** The position of the reaction entry (from 1), the same number as in `rttMonReactTable`. The number used is the one at the time the event was created (the event's `reaction_index`), so even if a reload reorders the entries before delivery, it never points to a different entry.

How it looks when received by `snmptrapd` (with the MIBs loaded):

```
CISCO-RTTMON-MIB::rttMonNotificationV2
CISCO-RTTMON-MIB::rttMonCtrlAdminLongTag.21 = STRING: lab
CISCO-RTTMON-MIB::rttMonHistoryCollectionAddress.21.1.1.1 = Hex-STRING: 0A 64 02 0B
CISCO-RTTMON-MIB::rttMonReactVar.21.1 = INTEGER: rtt(1)
CISCO-RTTMON-MIB::rttMonReactOccurred.21.1 = INTEGER: true(1)
CISCO-RTTMON-MIB::rttMonReactValue.21.1 = INTEGER: 150
CISCO-RTTMON-MIB::rttMonReactThresholdRising.21.1 = INTEGER: 100
CISCO-RTTMON-MIB::rttMonReactThresholdFalling.21.1 = INTEGER: 50
CISCO-RTTMON-MIB::rttMonEchoAdminLSPSelector.21 = ""
```

## File layout

| File | Contents |
|---|---|
| `internal/snmp/snmp.go` | Package API (`Source`, `Options`, `Run`). `Run` passes the MIB tree to `agentx.RunSubagent` |
| `internal/snmp/mib.go` | The MIB tree: table order, the 1-second cache of the Source (view), Get / GetNext lookup (implementation of `agentx.Handler`) |
| `internal/snmp/tables.go` | Definitions of the exposed tables and columns (each table in "Exposed tables and columns" in this document) |
| `internal/snmp/trap.go` | Sending traps (`TrapSink`, `trapOIDs`) |
| `internal/snmp/agentx/agentx.go` | AgentX encoding (RFC 2741). `OID`, `Value` and the value constructors (`Integer`, `Gauge`, `Counter32`, `TimeTicks`, `OctetString`, and so on), the exception values `NoSuchObject` / `NoSuchInstance` |
| `internal/snmp/agentx/agent.go` | The AgentX session (`RunSubagent`: connect, Open, Register, respond to requests, reconnect) and the `Handler` interface |
| `*_test.go` | Table-driven MIB tests with a fake Source, session tests with a fake master (agentx), trap tests with a fake receiver |
| `netsnmp_integration_test.go` | `//go:build integration`. Integration tests through net-snmp's snmpd |

`internal/snmp/agentx` is the generic part that does not know goipslad's MIB, and it exports only the names listed above. In Open, the subagent identifies itself by the first subtree it registers (CISCO-RTTMON-MIB, `1.3.6.1.4.1.9.9.42`).

## MIB files

goipslad itself does not read MIB files: the OIDs are compiled in. MIB files are needed only by management tools (`snmpwalk`, `snmptranslate`, an NMS) to resolve names, and by the staging snmpd / snmptrapd containers (`staging/compose.snmp.yaml`), which mount `mibs/` at `/mibs`. The files are third-party (Cisco and IETF) and are **not included in the repository** (`mibs/` is ignored by git). Obtain them as follows.

Source: Cisco's GitHub repository `cisco/cisco-mibs` (`main` branch), `https://raw.githubusercontent.com/cisco/cisco-mibs/main/v2/<name>.my`. Only `RFC1213-MIB` and `TOKEN-RING-RMON-MIB` are taken from `v1/`, because they are not in `v2/`.

| File | Purpose |
|---|---|
| `CISCO-RTTMON-MIB.my` | The main MIB. Control, statistics, history, latest results, reactions, notifications |
| `CISCO-RTTMON-TC-MIB.my` | TEXTUAL-CONVENTIONs used by the main MIB (RttMonRttType, RttResponseSense, and so on) |
| `CISCO-RTTMON-ICMP-MIB.my` | icmp-jitter tables (rttMonLatestIcmpJitterOperTable, rttMonIcmpJitterStatsTable) |
| `CISCO-SMI.my` | Root of Cisco's OIDs |
| `SNMPv2-SMI.my`, `SNMPv2-TC.my`, `SNMPv2-CONF.my`, `SNMPv2-MIB.my`, `SNMP-FRAMEWORK-MIB.my`, `INET-ADDRESS-MIB.my`, `IF-MIB.my`, `IANAifType-MIB.my`, `RFC1213-MIB.my` | Standard MIBs IMPORTed by the MIBs above. Needed in the staging containers because the Debian slim image does not include the standard MIBs (they are in the non-free `snmp-mibs-downloader`) |
| `CISCO-ETHER-CFM-MIB.my`, `CISCO-QOS-PIB-MIB.my`, `DIFFSERV-DSCP-TC.my`, `Q-BRIDGE-MIB.my`, `P-BRIDGE-MIB.my`, `BRIDGE-MIB.my`, `RMON-MIB.my`, `RMON2-MIB.my`, `TOKEN-RING-RMON-MIB.my` | Needed through the chain of IMPORTs of `CISCO-RTTMON-MIB` (TCs for Ethernet CFM and VLANs) |

Two kinds of local edits are needed so that net-snmp 5.9 can load the files. The definitions (OIDs, SYNTAX, columns) stay unchanged.

1. **Remove an extra quotation mark in `CISCO-RTTMON-TC-MIB.my`.** In the middle of the DESCRIPTION of `RttMonRttType` (right after "a Fabric Path Network.") there is one extra closing quotation mark. With it, none of the following definitions can be read (`Textual convention doesn't map to real type`).
2. **Truncate DESCRIPTIONs longer than 4,000 bytes.** net-snmp's parser breaks when a quoted string exceeds its limit (4,096 bytes). Truncate such descriptions at a line break to about 3,800 bytes and add a note such as `[Description shortened to fit net-snmp's 4096-byte limit on quoted strings; see the original in cisco/cisco-mibs.]`. As of the 2026-09 versions there are 9 such places: `CISCO-RTTMON-MIB.my` (MODULE-IDENTITY and one object description), `CISCO-RTTMON-TC-MIB.my` (RttResponseSense, RttMonRttType, RttMonProtocol, RttMonReactVar), `IF-MIB.my`, `SNMP-FRAMEWORK-MIB.my`, `SNMPv2-TC.my`.

## Performance

- **The subagent caches values read from the Source for up to 1 second.** This keeps values from being mixed within a single walk, and avoids taking a snapshot on every GetNext.
- **Measurements with 1,000 operations** (2 hour groups × 20 distributions, 15 history) are as follows (net-snmp 5.9.4).
  - A full traversal inside the subagent takes about 0.5 seconds for about 380,000 instances.
  - Through snmpd, each instance costs one AgentX round trip (about 0.15 ms).
  - Per-table traversal: `rttMonLatestRttOperTable` (5,000 instances) takes 2.2 seconds with snmpwalk and 1.3 seconds with snmpbulkwalk. `rttMonCtrlAdminTable` (10,000 instances) takes 5.3 seconds and 2.0 seconds.
  - snmpbulkwalk of the whole MIB takes 57 seconds. The roughly 200,000 instances of `rttMonStatsCaptureTable` account for most of it.
- **For monitoring, fetch the tables you need, not the whole MIB.** For example: `rttMonLatestRttOperTable`, `rttMonCtrlOperTable`.
- The performance target is "per-table traversal within 30 seconds with 1,000 operations". Traversal of the whole MIB is not covered by this target (because the snmpd and AgentX round trips are proportional to the number of instances).
- To reduce the amount of traversal, set `history.hours-of-statistics-kept` and `history.distributions-of-statistics-kept` to only what you need. The number of rows in `rttMonStatsCaptureTable` is "number of operations × hours kept × number of distribution buckets", and it accounts for most of the instances in the whole MIB.

## Differences from Cisco (summary)

| Item | Cisco | goipslad |
|---|---|---|
| Writes | Operations can be created and modified over SNMP | Read-only (the configuration file is the source of truth) |
| TimeStamp base | The device's sysUpTime | goipslad startup |
| IPv6 targets | Not documented | 16 octets |
| `rttMonEchoAdminTOS` | IPv4 ToS | traffic-class for IPv6 |
| `rttMonHistoryCollectionAddress` in traps | History instance | Target at `ID.1.1.1` |
| syslog action | Separate from SNMP action-type | `rttMonReactActionType` is none(1) |
| Notification version | `rttMonNotification` (v1) and V2 | V2 only, SNMPv2c only |
