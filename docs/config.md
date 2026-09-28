# Configuration file reference

`goipslad` reads one YAML configuration file. This page describes every key you can write, with its type, default, range and validation rules.
After writing the file, you can check it with `goipsla validate <file>` before handing it to the daemon (see below).

- Keys are written in kebab case. The names reuse the Cisco IOS CLI vocabulary as is (`frequency`, `request-data-size`, `history buckets-kept` and so on).
- **Unknown keys are errors**, so that a misspelling is never silently ignored.
- Writing the same key twice in one mapping is an error.
- Only one YAML document may be written (a second document after `---` is an error).
- YAML anchors and aliases (`&name` / `*name`) can be used. The merge key `<<` cannot. Use `templates` for shared settings.
- An empty file is accepted as "zero operations, every global setting at its default".

## Changes (2026-09-27)

With the following changes, a configuration that used to load may now **fail**. Before updating, check it with `goipsla validate <file>` and fix the places that fail. For every change, the location of the error (see "How error locations are written") and its message point to the place to fix.

| Change | Before | Now | How to fix |
|---|---|---|---|
| `react[].upper` / `lower` must be given together | If only one was written, the other took the element's default | Only one is the error `upper and lower must be given together`. If both are omitted, the defaults apply as before | Write both. To keep the previous behavior, write the element's default (the table in "`react`") for the side you had left out. Cisco's `threshold-value` also takes two values |
| The lower bound of `timeout` is 1ms | `0ms` was accepted (the MIB range starts at 0) | `timeout: 0ms` is `must be at least 1ms` | Use 1ms or more (0 is meaningless as a measurement; see "Intentional deviations from Cisco") |
| `source-interface` / `vrf` are Linux interface names | Anything non-empty was accepted (and failed at run time) | 1..15 bytes, without `/`, whitespace or control characters | Write the name of an existing interface or VRF device |
| `global.snmp.traps[].host` is checked when the file is loaded | Anything non-empty was accepted, and failed only when a trap was sent | One of `host`, `host:port`, `[IPv6]:port`, `[IPv6]`, or an IPv6 address without brackets; the port is 1..65535 | Fix the syntax (put an IPv6 address in brackets when adding a port) |
| Other global syntax (also checked when the file is loaded) | Accepted, and failed at startup or at run time | `snmp.agentx` is `/path`, `unix:/path`, `tcp:host:port` or `host:port`; the host of `metrics-listen` is an IP address or a host name; `api-socket` is at most 107 bytes | Fix the syntax |

In addition, settings that are not errors but have no effect are now reported as "warnings" ("Warnings" under "Validation rules"). With warnings only, startup and reload succeed as before.

## Writing values

| Kind | Syntax | Examples |
|---|---|---|
| Duration | A string with a unit. The units are `ms`, `s`, `m`, `h` (the syntax of Go's `time.ParseDuration`) | `60s`, `5000ms`, `1h30m` |
| Integer | Decimal, or hexadecimal with `0x` | `184`, `0xB8` |
| Boolean | `true` / `false` | |
| IP address | An IPv4 or IPv6 literal | `10.100.1.11`, `fd00:100:2::11` |

A duration written as a number without a unit (`frequency: 60`) is the error `duration needs a unit such as 60s or 5000ms`.
Cisco writes `frequency` in seconds and `timeout` / `threshold` in milliseconds, so leaving out the unit invites mix-ups. This rule prevents them.

Values also have a granularity. `frequency`, `life`, `ageout`, `history.enhanced.interval` and `delay.up` / `delay.down` must be whole seconds (`1500ms` is an error). `timeout`, `threshold`, `interval` and `history.statistics-distribution-interval` must be whole milliseconds.

## Overall structure

```yaml
global:      # daemon-wide settings (optional)
templates:   # settings shared by operations (optional)
operations:  # the measurement operations
tracks:      # tracking (optional)
actions:     # external notifications (optional)
```

## `global`

| Key | Type | Default | Description |
|---|---|---|---|
| `api-socket` | Absolute path | `/run/goipslad/goipslad.sock` | The Unix socket `goipsla` connects to. At most 107 bytes, the limit of a Unix socket path |
| `api-socket-group` | Group name or numeric gid | `""` (unchanged) | Group of the API socket (chgrp). Write the group whose members, other than root, may use `goipsla`. If the group does not exist, goipslad stops at startup with an error |
| `metrics-listen` | `host:port` or `""` | `127.0.0.1:9818` | Listen address for Prometheus `/metrics`. `""` disables it. `host` is an IP address or a host name (empty, as in `:9818`, means all addresses); the port is 1..65535 |
| `log.format` | `text` / `json` | `text` | Log format |
| `log.level` | `debug` / `info` / `warn` / `error` | `info` | Log level |
| `syslog` | Mapping | Omitted (disabled) | Writing it enables output to syslog (P5). `syslog: {}` enables it with the default facility |
| `syslog.facility` | `kern`, `user`, `mail`, `daemon`, `auth`, `syslog`, `lpr`, `news`, `uucp`, `cron`, `authpriv`, `ftp`, `local0`..`local7` | `daemon` | syslog facility |
| `snmp` | Mapping | Omitted (disabled) | Writing it enables the AgentX subagent and traps. See `docs/snmp.md` |
| `snmp.agentx` | String | `/var/agentx/master` | Where the AgentX master (snmpd) is. One of `/path`, `unix:/path`, `tcp:host:port`, `host:port` (the same syntax as net-snmp's `agentXSocket`; `udp:` is not accepted). The syntax is checked when the file is loaded |
| `snmp.traps` | Array | None | Trap destinations |
| `snmp.traps[].host` | String | Required | Destination host. One of `host`, `host:port`, `[IPv6]:port`, `[IPv6]`, or an IPv6 address without brackets. `host` is an IP address or a host name; the port is 1..65535 (162 when omitted). Checked when the file is loaded (it never first fails when a trap is sent) |
| `snmp.traps[].community` | String | Required | Community name |
| `snmp.traps[].version` | `v2c` | `v2c` | Trap version. Only SNMPv2c for now |

## `operations`

Each item defines one or more measurement operations. There are two forms.

**Form A (single)**: Write `id` and `target`. `template` may be added; then the keys written in the item override the template's values.

```yaml
operations:
  - id: 1
    type: icmp-jitter
    target: 10.100.2.11
  - id: 5
    template: wan-echo
    target: 10.100.1.13
    timeout: 1000ms       # overrides the template's timeout
```

**Form B (a template and several targets)**: Write `template` and `targets`. `targets` is a mapping of "ID: target", and each line becomes one operation. The other keys written in the item apply to every target.

```yaml
operations:
  - template: wan-echo
    source-interface: eth1
    targets:
      101: 10.100.1.11
      102: 10.100.1.12
      103: fd00:100:2::11
```

`id` and `target` cannot be written in form B, and `template` is required in form B.

### IDs

An operation ID is an integer 1..2147483647 and must be unique across the configuration file. IDs are used by the CLI, as Prometheus labels and as SNMP indexes. They are written explicitly rather than assigned automatically, so that reordering the list never changes them.
There can be at most 1000 operations after expansion. Loaded operations are sorted by ascending ID.

### Operation keys

| Key | Type | Default | Range and notes | Cisco / MIB |
|---|---|---|---|---|
| `id` | Integer | Required (form A) | 1..2147483647 | `ip sla <id>` / `rttMonCtrlAdminIndex` |
| `type` | `icmp-echo` / `icmp-jitter` | Required | May come from a template | `icmp-echo` / `icmp-jitter` |
| `target` | IP address | Required (form A) | IP literals only. A host name is the error `hostnames are not supported yet`. Addresses with a zone (`fe80::1%eth0`) and `0.0.0.0` / `::` are not allowed. `::ffff:a.b.c.d` is treated as IPv4 | Target |
| `targets` | Mapping (ID → IP address) | — | Form B only. Cannot be empty | — |
| `template` | String | None | A name in `templates` | — |
| `source-ip` | IP address | None (the kernel chooses) | Same address family as the target | `source-ip` |
| `source-interface` | String | None | Source interface name (a Linux interface name: 1..15 bytes, without `/`, whitespace or control characters) | `source-interface` |
| `vrf` | String | None | VRF device name (same constraints as `source-interface`) | `vrf` |
| `tos` | Integer | 0 | 0..255. IPv4 targets only | `tos` / `rttMonEchoAdminTOS` |
| `traffic-class` | Integer | 0 | 0..255. IPv6 targets only | `traffic-class` |
| `flow-label` | Integer | 0 | 0..1048575 (0xFFFFF). IPv6 targets only | `flow-label` |
| `frequency` | Duration | `60s` | 1s..604800s, whole seconds | `frequency` / `rttMonCtrlAdminFrequency` |
| `timeout` | Duration | `5000ms` (see the note below) | 1..604800000ms (0 is not allowed; Cisco's MIB range starts at 0) | `timeout` / `rttMonCtrlAdminTimeout` |
| `threshold` | Duration | `5000ms` (see the note below) | 0..60000ms | `threshold` / `rttMonCtrlAdminThreshold` |
| `request-data-size` | Integer | 28 | 28..16384. icmp-echo only | `request-data-size` |
| `data-pattern` | Integer | `0xABCDABCD` | 0..0xFFFFFFFF. icmp-echo only | `data-pattern` |
| `verify-data` | Boolean | `false` | icmp-echo only | `verify-data` / `rttMonCtrlAdminVerifyData` |
| `tag` | String | None | 0..128 characters. Control characters such as newline and tab (U+0000 to U+001F, U+007F) are not allowed | `tag` / `rttMonCtrlAdminLongTag` |
| `owner` | String | None | 0..255 characters. Control characters are not allowed | `owner` / `rttMonCtrlAdminOwner` |
| `interval` | Duration | `20ms` | 1..60000ms. icmp-jitter only. Interval between packets | `icmp-jitter ... interval` |
| `num-packets` | Integer | 10 | 1..1000. icmp-jitter only. Packets sent per attempt | `icmp-jitter ... num-packets` |
| `one-way-delay` | Boolean | `false` | icmp-jitter only. When true, one-way delays (source to target, target to source) are accumulated. **Disabled by default because the target's clock synchronization cannot be guaranteed** (see the note below) | — (Cisco always accumulates them when NTP-synchronized) |
| `history` | Mapping | Table below | Statistics and history | `history ...` |
| `schedule` | Mapping | Omitted (start immediately, run forever) | Below | `ip sla schedule` |
| `react` | Array | None | Below | `ip sla reaction-configuration` |

**About the defaults of `timeout` and `threshold`**: Both default to 5000ms, but only when not written, they are lowered automatically to satisfy the following rules.

- If `timeout` is not written, it is the smaller of `frequency` (for icmp-jitter, `frequency − interval × num-packets`) and 5000ms.
- If `threshold` is not written, it is the smaller of `timeout` and 5000ms.

For example, writing only `frequency: 2s` makes `timeout` and `threshold` 2000ms. Only explicitly written values that break the rules are errors.

**About `one-way-delay`**: A one-way delay is computed from the difference between the target's time in the ICMP Timestamp reply and the source's time. Nothing guarantees that the target's clock is synchronized with the source's, and if they differ, the values are far off (they can even be negative delays). Therefore they are not accumulated by default. Set `one-way-delay: true` only when the target and the source are known to be synchronized to the same NTP server. While it is false, no one-way delay statistics are produced, and the reaction elements `maxOfLatencySD` / `maxOfLatencyDS` / `latencySDAvg` / `latencyDSAvg` cannot be written.

### `history`

| Key | Type | Default | Range | Cisco / MIB |
|---|---|---|---|---|
| `lives-kept` | Integer | 0 | 0..2. 0 means no history is kept | `history lives-kept` / `rttMonHistoryAdminNumLives` |
| `buckets-kept` | Integer | 15 | 1..60 | `history buckets-kept` / `rttMonHistoryAdminNumBuckets` |
| `filter` | `none` / `all` / `overThreshold` / `failures` | `none` | | `history filter` / `rttMonHistoryAdminFilter` |
| `hours-of-statistics-kept` | Integer | 2 | 0..25 | `history hours-of-statistics-kept` |
| `distributions-of-statistics-kept` | Integer | 1 | 1..20 | `history distributions-of-statistics-kept` |
| `statistics-distribution-interval` | Duration | `20ms` | 1..100ms | `history statistics-distribution-interval` |
| `enhanced` | Mapping | Omitted (disabled) | Writing it enables the enhanced history. `enhanced: {}` enables it with the defaults | `history enhanced` |
| `enhanced.interval` | Duration | `900s` | 1..3600s, whole seconds | `history enhanced interval` |
| `enhanced.buckets` | Integer | 100 | 1..100 | `history enhanced buckets` |

### `schedule`

| Key | Type | Default | Range and notes |
|---|---|---|---|
| `life` | Duration or `forever` | `forever` | 1s..2147483647s, whole seconds |
| `start-time` | Below | `now` | |
| `ageout` | Duration | `0s` (disabled) | 0..2073600s, whole seconds |
| `recurring` | Boolean | `false` | Start at the same time every day |

Values of `start-time`:

| Value | Meaning |
|---|---|
| `now` | Start immediately at load time |
| `pending` | Do not start (stay pending until a reload changes `start-time`; `goipsla restart` cannot start a pending operation) |
| `after <duration>` | Start the given time after loading. The duration is written with a unit, as in `after 90s`, or as in Cisco, `after 00:10:00` (hours:minutes:seconds) |
| `HH:MM` / `HH:MM:SS` | The next time the local clock shows that time (today or tomorrow). Quote the value (`"13:30"`) |
| RFC 3339 date and time | That date and time. For example `"2026-10-01T09:00:00+09:00"`. A date and time in the past is not an error |

When `recurring: true`, all three of the following must hold (the same rules as Cisco).

- `start-time` has the form `HH:MM[:SS]`
- `life` is shorter than 24 hours (`forever` is not allowed)
- `ageout` is `0s`, or `life + ageout` is longer than 24 hours

**Difference from Cisco**: An operation without `schedule` starts immediately at load time and runs forever. This differs from Cisco's defaults (`start-time pending`, `life 3600`). For a daemon whose configuration file is the source of truth, "if you write it, it runs" is more natural, so this is intentional. Writing `schedule: {}` means the same (`start-time now`, `life forever`), and the effective configuration treats it the same as omitting `schedule` (`null` in JSON). Therefore adding or removing `schedule: {}` does not rebuild the operation on reload. To get Cisco's behavior, write `schedule: { start-time: pending, life: 3600s }` explicitly.

`HH:MM` and `after` count from the time the configuration was loaded. On reload, the starting point changes to the time of the reload. A start time written as `HH:MM[:SS]` is treated as "that time every day", and the effective configuration, as in `goipsla show config`, shows it as a time only, such as `01:30:00` (a date and time written in RFC 3339 is shown as a date and time, including fractional seconds).

**Run-time semantics** (settled in P4; for details, see sections 7 and 8 of [scheduling.md](scheduling.md)):

- The states are `pending` / `active` / `inactive`. `start-time: now` is `active` at load time; `after` / a date and time / `HH:MM` are `pending` until the start time; `pending` stays `pending` until `start-time` is changed by a reload.
- When `life` runs out, the operation becomes `inactive` and stops attempting. The statistics are frozen and kept. With `recurring: true`, it becomes `active` again at the same time the next day and a new life starts (every 24 hours from the start time; daylight saving time changes are not taken into account).
- `ageout` counts down only while the operation is not `active`, and stops when it becomes `active`. An operation whose ageout reaches 0 is deleted along with its statistics. It stays in the configuration file, though, so the next reload recreates it (on Cisco, the configuration is deleted too).
- Changing anything in `schedule` and reloading rebuilds the operation, and its statistics start from a new life. A change in only the date that `HH:MM` resolves to is not considered a change.

### `react`

Watches the results of an operation and notifies when a threshold is crossed. At most one row per element (writing the same `element` twice is an error).

| Key | Type | Default | Range and notes |
|---|---|---|---|
| `element` | String | Required | Table below |
| `threshold-type` | `never` / `immediate` / `consecutive` / `xofy` / `average` | `never` | When a violation is recognized |
| `count` | Integer | 5 | 1..16. N of `consecutive` and `average`. An error with any other `threshold-type` |
| `x`, `y` | Integer | 5, 5 | 1..16, `x ≤ y`. `xofy` only. An error with any other `threshold-type` |
| `upper`, `lower` | Integer | Per element (table below) | 0..2147483647, `lower ≤ upper`. When written, write both (only one is `upper and lower must be given together`; Cisco's `threshold-value` also takes two values). When both are omitted, the defaults in the table below apply. Cannot be written for the boolean elements (`timeout`, `verifyError`) |
| `action` | `none` / `syslog` / `trap` / `trap-and-syslog` | `none` | How to notify |

Monitored elements and their default thresholds (`upper` / `lower`):

| Element | icmp-echo | icmp-jitter | Unit | Default upper / lower | Value monitored |
|---|---|---|---|---|---|
| `rtt` | Yes | Yes | ms | 5000 / 3000 | RTT of a completed attempt (for icmp-jitter, the mean over the burst). Not evaluated for failed attempts |
| `timeout` | Yes | Yes | Boolean | — (`average` is not allowed) | A timeout is a violation; any other attempt is a recovery |
| `verifyError` | Yes | — | Boolean | — (`average` is not allowed) | A verifyError is a violation; any other attempt is a recovery |
| `jitterAvg`, `jitterSDAvg`, `jitterDSAvg` | — | Yes | ms | 100 / 100 | Mean jitter (both directions / source to target / target to source) |
| `maxOfPositiveSD`, `maxOfNegativeSD`, `maxOfPositiveDS`, `maxOfNegativeDS` | — | Yes | ms | 10000 / 10000 | Maximum of positive and negative jitter |
| `packetLoss`, `packetLateArrival`, `packetOutOfSequence`, `successivePacketLoss` | — | Yes | Packets | 10000 / 10000 | Losses, late arrivals, reorderings (the sum of the 3 directions), longest run of successive losses |
| `maxOfLatencySD`, `maxOfLatencyDS`, `latencySDAvg`, `latencyDSAvg` | — | Yes (only with `one-way-delay: true`) | ms | 5000 / 3000 | Maximum and mean of one-way delays |

- The icmp-jitter elements follow the ICMP Jitter column of Cisco's "Supported Elements, by IP SLA Operation" table. `packetLossSD`, `packetLossDS` and `packetMIA` are for UDP jitter only and cannot be written, since ICMP cannot tell the direction of a loss (monitor losses with `packetLoss`).
- `verifyError` is not listed for icmp-echo in Cisco's CLI table, but the MIB allows it, and it can be used here.
- `verifyError` cannot be written for icmp-jitter. Cisco's table has Y in the ICMP Jitter column, but the ICMP Timestamp that icmp-jitter sends has no data part and `verify-data` cannot be set, so a verifyError cannot happen (**a deviation from Cisco**).
- For the evaluation rules (the boundaries of violation and recovery, the 4 kinds of `threshold-type`, when notifications are sent), see `docs/reactions.md`; for where notifications are delivered, see `docs/events.md`.

### Templates (`templates`)

```yaml
templates:
  wan-echo:
    type: icmp-echo
    frequency: 10s
    timeout: 2000ms
    threshold: 300ms
    tos: 0xB8
    tag: wan
    react:
      - { element: rtt, threshold-type: consecutive, count: 3, upper: 300, lower: 200, action: trap }
```

A template can hold the same keys as an operation, except `id`, `target`, `targets` and `template` (templates do not nest).

The values of a template and an operation are combined by the following rules.

- Ordinary keys: The key written on the operation wins.
- `history`, `history.enhanced`, `schedule`: Combined key by key (for example, both the template's `schedule.start-time` and the operation's `schedule.life` take effect).
- `react`: Writing it on the operation **replaces the template's list entirely**. Writing just `react:` (no value) removes the template's reactions.

**Mapping between address families**: `tos` (IPv4) and `traffic-class` (IPv6) are the same DS byte. Therefore a value written in a template, or as a shared key of form B, is mapped to the target's family.

- For an IPv6 target, the value of `tos` is used as `traffic-class` (if `traffic-class` is also written, it wins).
- For an IPv4 target, the value of `traffic-class` is used as `tos`. `flow-label` is ignored.

When written directly in a form A item, the value is not mapped, and a mismatch with the target's family is an error.

## `tracks`

Decides Up / Down from the latest return code of an operation (Cisco's `track <id> ip sla <op> [state|reachability]`). For the rules, see `docs/reactions.md`.

| Key | Type | Default | Range and notes |
|---|---|---|---|
| `id` | Integer | Required | 1..2147483647, unique |
| `operation` | Integer | Required | A defined operation ID |
| `mode` | `state` / `reachability` | `state` | `state` is Up only for OK; `reachability` is Up for OverThreshold too |
| `delay.up` | Duration | `0s` | 0..180s, whole seconds |
| `delay.down` | Duration | `0s` | 0..180s, whole seconds |

## `actions`

Sends reaction and tracking events to external receivers. For the event JSON, the conventions for calling webhooks and exec, and retries, see `docs/events.md`.

| Key | Type | Default | Range and notes |
|---|---|---|---|
| `on` | Array | Required | One or more of `threshold-exceeded`, `threshold-cleared`, `track-up`, `track-down`. No duplicates. Reactions of the `timeout` element also arrive as `threshold-exceeded` / `threshold-cleared` |
| `track` | Integer | Omitted (all tracks) | A defined track ID. Narrows only `track-*` events (`threshold-*` events do not belong to a track, so they are not narrowed) |
| `webhook` | URL | None | `http://` or `https://` |
| `exec` | Absolute path | None | Program to run |

At least one of `webhook` and `exec` is required.

## Validation rules

In addition to the ranges in the tables above, the following rules are checked.

- **Order of times**: `threshold ≤ timeout ≤ frequency`. These are the same two rules Cisco enforces.
- **icmp-jitter times**: `timeout + interval × num-packets ≤ frequency`. Cisco only recommends this; here it is an error, because an attempt that runs into the next start time is busy every time (**a deviation from Cisco**).
- **Address family**: icmp-jitter takes IPv4 targets only (ICMPv6 has no Timestamp message). `tos` is for IPv4 targets only; `traffic-class` and `flow-label` are for IPv6 targets only (the mapping in templates is described above). `source-ip` must be in the same family as the target.
- **Applicability per type**: Writing `request-data-size`, `data-pattern` or `verify-data` for icmp-jitter is `not applicable to icmp-jitter`. Writing `interval`, `num-packets` or `one-way-delay` for icmp-echo is `not applicable to icmp-echo`. Values that come from a template are treated the same way.
- **Templates**: An unknown name is `unknown template "<name>"`.
- **References**: `tracks[].operation` must be an existing operation and `actions[].track` an existing track.
- **Shape of the file**: The outermost level of the file must be a mapping (`the file must be a mapping (global, templates, operations, tracks, actions)`).

### Warnings

Settings that are not errors but have no effect are reported as "warnings" (`config.Config.Warnings`). `goipsla validate` prints them on standard error; `goipslad` logs them one per line as the warning `config warning` at startup and on reload. On reload, they are also in the `warnings` of the response (`docs/api.md`).

| Warning | Example |
|---|---|
| A reaction `action` with no receiver: `syslog` / `trap-and-syslog` without `global.syslog`, or `trap` / `trap-and-syslog` with an empty `global.snmp.traps` | `operations(id=3).react[0].action: sends a trap, but global.snmp.traps is empty: no trap is sent (10 reactions)` (the first place it applies and the count, on one line) |
| A template that is not used | `templates.spare: not used by any operation` |
| `flow-label` written in a template or as a shared key of form B is ignored for IPv4 targets | `templates.base.flow-label: ignored for the IPv4 targets` |
| `track` written in an action whose `on` has no track-* event (threshold-* events do not belong to a track, so narrowing has no effect) | `actions[0].track: has no effect: on lists no track-up / track-down event` |

### How error locations are written

`goipslad` and `goipsla validate` report **every** error they find, one per line, in the form `<location>: <message>`. Locations are written as follows.

| Notation | Meaning |
|---|---|
| `global.log.level` | That key |
| `templates.wan-echo.frequency` | A value written in a template |
| `operations[2].timeout` | A value written in the third item of `operations` (0-based). An error in the value itself (type, range, unknown key and so on) |
| `operations[1].targets.101` | The line for ID 101 in the `targets` of form B |
| `operations(id=101).timeout` | Operation 101 after templates are expanded. An error that depends on a combination with other keys (order of times, family, applicability and so on) |
| `tracks[0].operation`, `actions[1].track` | Items of tracks and actions |

An error in the range of a value is reported once, at the place the value was written (the template, if it was written in a template). An error in a combination is reported for each operation after expansion.

## Intentional deviations from Cisco

The points where goipslad intentionally behaves differently from Cisco IOS IP SLA are gathered here. For details, see each section.

| Item | Cisco | goipslad | Reason |
|---|---|---|---|
| `schedule` omitted | `start-time pending`, `life 3600` | Start immediately at load time and run forever | For a daemon whose configuration file is the source of truth, "if you write it, it runs" is more natural |
| Defaults of `timeout` and `threshold` | Always 5000ms. With a smaller `frequency` or `timeout`, the defaults break the rules | Only when not written, `timeout` is the smaller of `frequency` (for icmp-jitter, `frequency − interval × num-packets`) and 5000ms, and `threshold` is the smaller of `timeout` and 5000ms | So that writing just `frequency: 2s` is not an error. Explicit values are validated as usual |
| `tos` / `traffic-class` written in a template or as a shared key of form B | `tos` is for IPv4 only, `traffic-class` for IPv6 only | Each is mapped to the other to match the target's family. `flow-label` is ignored for IPv4 targets. When written directly in a form A item, the value is not mapped and a mismatch is an error | So that one template can serve both IPv4 and IPv6 targets (both are the same DS byte) |
| Lower bound of `timeout` | 0..604800000ms (MIB) | 1..604800000ms. `timeout: 0ms` is the error `must be at least 1ms` | With 0, every reply arrives after the timeout (sequenceError), which is meaningless as a measurement |
| icmp-jitter `timeout + interval × num-packets ≤ frequency` | Recommended | Error | An attempt that runs into the next start time is busy every time |
| Units of durations | `frequency` in seconds, `timeout` and others as numbers of milliseconds | Only strings with a unit are accepted | To prevent mixing up seconds and milliseconds |
| Reaction element `verifyError` for icmp-jitter | Allowed in the table | Cannot be written (`unknown element "verifyError" for icmp-jitter`) | ICMP Timestamp has no data part and verify-data cannot be set, so it cannot happen |
| Control characters in `tag` / `owner` | Only a length limit | Control characters such as newline and tab are rejected | They go into syslog lines, exec environment variables and the SNMP DisplayString, where a newline could forge a line |

## `goipsla validate`

```
goipsla validate <file> [--print] [-o table|json] [--show-secrets]
```

Validates only the file, without connecting to the daemon.

- On success, `OK: <N> operations` is printed on standard output and the exit status is 0.
- On failure, every error is printed on standard error, one per line, in the form `<location>: <message>`, and the exit status is 1.
- Warnings ("Warnings" under "Validation rules") are printed on standard error even on success, one per line, in the form `warning: <location>: <message>` (for example `warning: templates.spare: not used by any operation`). With warnings only, the exit status stays 0.
- The three points above apply to the table format (the default). With `-o json` (without `--print`), the result is printed as a single JSON object on standard output and nothing is printed on standard error. The exit status is the same.
  - Success: `{"ok": true, "operations": <N>, "warnings": [...]}` (`[]` when there are no warnings)
  - Failure: `{"ok": false, "errors": ["<location>: <message>", ...]}` (the same form for YAML syntax errors and for a file that cannot be read)
- With `--print`, the effective configuration, with defaults filled in and templates expanded, is printed. The table format (the default) lists ID, type, target, frequency, timeout, threshold and tag, followed by the `OK:` line. With `-o json`, only the JSON is printed. The JSON uses the same key names and duration syntax as this configuration file (`config.EffectiveConfigJSON`). Secrets are redacted: the SNMP `community` becomes `***`, and everything after the host of a webhook URL (path, userinfo, query, fragment) becomes `***` (for example `https://hooks.example/***`). To print them unredacted, add `--show-secrets`.

```console
$ goipsla validate /etc/goipslad/goipslad.yaml
OK: 4 operations

$ goipsla validate bad.yaml
operations[0].tos: must be between 0 and 255
operations(id=1).timeout: must not exceed frequency (5s)
```

## Example

```yaml
global:
  api-socket: /run/goipslad/goipslad.sock
  metrics-listen: 127.0.0.1:9818
  syslog: { facility: local0 }
  snmp:
    agentx: /var/agentx/master
    traps: [ { host: 192.0.2.10, community: public } ]

templates:
  wan-echo:
    type: icmp-echo
    frequency: 10s
    timeout: 2000ms
    threshold: 300ms
    tos: 0xB8
    tag: wan
    react:
      - { element: rtt, threshold-type: consecutive, count: 3, upper: 300, lower: 200, action: trap }
      - { element: timeout, threshold-type: immediate, action: trap }

operations:
  - id: 1
    type: icmp-jitter
    target: 10.100.2.11
    interval: 20ms
    num-packets: 10
    frequency: 30s

  # define many targets at once with the same template
  - template: wan-echo
    source-interface: eth1
    vrf: blue
    targets:
      101: 10.100.1.11
      102: 10.100.1.12
      103: fd00:100:2::11   # tos 0xB8 is used as traffic-class 0xB8

  # measure for one hour from 01:30 every day
  - id: 200
    type: icmp-echo
    target: 192.0.2.200
    history: { lives-kept: 1, buckets-kept: 60, filter: failures, enhanced: {} }
    schedule: { start-time: "01:30", life: 1h, recurring: true }

tracks:
  - { id: 1, operation: 101, mode: reachability, delay: { up: 10s, down: 5s } }

actions:
  - on: [track-down, track-up]
    track: 1
    webhook: https://example.invalid/hook
  - on: [threshold-exceeded]
    exec: /usr/local/libexec/ipsla-notify.sh
```
