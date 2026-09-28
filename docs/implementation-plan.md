# goipslad / goipsla implementation plan

> On 2026-09-27 the names were changed to `goipslad` / `goipsla` (formerly `ipslad` / `ipslactl`; the module path changed from `ipsla` to `goipsla`).

This plan implements ICMP measurement equivalent to Cisco IOS IP SLA as a Go daemon, `goipslad`, and a dedicated CLI, `goipsla`, running on Linux.
The specification is based on [Cisco IP SLA ICMP spec research](../reports/cisco-ip-sla-icmp-spec-research.md) (hereafter "the spec research"), and verification uses the Docker Compose environment in [staging/](../staging/).

## 1. Decided requirements

The following was decided in an interview (2026-09-27).

| Item | Decision |
|---|---|
| Language | Go (the development environment is go 1.26) |
| Operations in the first release | `icmp-echo` (IPv4 / IPv6) and `icmp-jitter` (IPv4 only, because ICMPv6 has no Timestamp; same as Cisco) |
| Later operations | `path-echo`, `path-jitter` |
| Scale | Up to 1,000 concurrent operations, handled by a single process |
| Architecture | A resident daemon `goipslad` and a CLI `goipsla`. The CLI connects only through a local Unix domain socket |
| Configuration | The YAML file is the single source of truth. After editing, the differences are applied with `goipsla reload` or SIGHUP |
| Cisco compatibility | Match the semantics (defaults, return codes, statistics definitions, reaction algorithms, state machine). The CLI output is designed independently for many targets |
| Features in the first release | Threshold reactions, tracking, hourly statistics and history, schedule control (life / start-time / ageout / recurring and the state machine) |
| External integrations | Prometheus, syslog and JSON logs, webhooks and external scripts, SNMP (reading CISCO-RTTMON-MIB and traps; SET is not accepted) |
| Source control | Source IP, source interface, Linux VRF, ToS / Traffic Class |
| Minimum measurement interval | 1 second (in whole seconds) |
| Statistics persistence | None. Statistics are lost on restart (same as Cisco). Long-term storage is done on the Prometheus side |
| Distribution | A statically linked single binary, a systemd unit, and a container image |
| Privileges | root is not required. Only `CAP_NET_RAW` is granted |

## 2. Overall architecture

```
                    ┌─────────────────────────────────── goipslad ───────────────────────────────────┐
 config.yaml ──────▶│ config ──▶ manager (diff apply, state machine)                                 │
 SIGHUP / reload    │               │                                                                │
                    │               ▼                                                                │
                    │           scheduler ──▶ operation(echo / jitter)                               │
                    │                              │  ▲                                              │
                    │                              ▼  │ reply, timeout                               │
                    │                     probe engine (ICMP sockets)                                │
                    │                              │                                                 │
                    │                              ▼                                                 │
                    │     result ──▶ stats (latest, hourly, distribution, history, enhanced history) │
                    │              └▶ reaction / track ──▶ event bus                                 │
                    │                                        │                                       │
                    │   API (Unix socket, HTTP/JSON)     sinks: syslog, JSON                         │
                    │   /metrics (Prometheus)            log, webhook, exec,                         │
                    │   AgentX (SNMP read)               SNMP trap                                   │
                    └────────────────────────────────────────────────────────────────────────────────┘
goipsla ──(Unix socket)──▶ API
```

### Package layout

| Package | Responsibility |
|---|---|
| `cmd/goipslad`, `cmd/goipsla` | Entry points |
| `internal/config` | Loading YAML, filling in defaults, expanding templates, validation |
| `internal/probe` | ICMP sockets, building and parsing packets, matching replies, obtaining timestamps |
| `internal/op` | The execution logic for one run of `icmp-echo` and `icmp-jitter`. Deciding the return code |
| `internal/sched` | Periodic execution, spreading start times, life / ageout / recurring, busy detection |
| `internal/manager` | Creating and destroying operations, applying reload differences, the state machine |
| `internal/stats` | Latest result, hourly aggregation, distribution buckets, history, enhanced history, jitter statistics |
| `internal/react` | Threshold reactions (immediate / consecutive / xofy / average) and tracking |
| `internal/event` | The event bus and each sink |
| `internal/api` | HTTP/JSON API over a Unix socket (for the CLI) |
| `internal/metrics` | Prometheus exporter |
| `internal/snmp` | AgentX subagent (the MIB tree and tables) and sending traps. AgentX encoding and sessions are in the subpackage `internal/snmp/agentx` |
| `internal/clock` | Time abstraction. Replaced with a fake clock in tests |

## 3. Key design decisions

### 3.1 ICMP engine

- **Sockets**: Open only one raw ICMP socket per combination of address family and VRF, shared by all operations. Even with 1,000 targets, only a few sockets are needed. The ICMP Timestamp of `icmp-jitter` cannot be sent over an unprivileged ping socket, so raw sockets are used throughout.
- **Receive filtering**: Use `ICMP_FILTER` for IPv4 and `ICMP6_FILTER` for IPv6 to receive only Echo Reply, Timestamp Reply, Destination Unreachable, and Time Exceeded. The last two are used by the path operations.
- **Source selection**: While keeping the shared socket, specify the source address and interface per send with `IP_PKTINFO` / `IPV6_PKTINFO`.
- **VRF**: Bind each per-VRF socket to the VRF device with `SO_BINDTODEVICE`.
- **ToS / Traffic Class**: Specify per send with a control message.
- **Time**: Take the receive time from the kernel with `SO_TIMESTAMPNS`. For the send time, use the monotonic time just before `sendmsg`; as an improvement, consider software send timestamps from `SO_TIMESTAMPING` later. The control messages of `golang.org/x/net` cannot handle receive timestamps, so `recvmsg` is implemented in-house with `golang.org/x/sys/unix`.
- **Matching**: Assign an ICMP Identifier per operation and advance the Sequence per attempt. The Echo data contains a custom header (magic, operation ID, sequence, send time) so that ping from other processes and late replies are reliably distinguished.
- **Payload length**: As in Cisco, the default `request-data-size` is 28 bytes. Cisco defines the ICMP data as "an 8-byte internal timestamp + request-data-size", which makes an IPv4 packet 64 bytes by default, so we also make the data `8 + request-data-size` bytes to match the on-wire size (revised on 2026-09-27 following a P1a finding). The 20-byte custom header is placed at its start, and the rest is filled with `data-pattern` (default: repeated `0xABCDABCD`).
- **Pending attempts**: Unanswered attempts are managed in a table keyed by (socket, Identifier, Sequence), and expired entries are swept with a single timer. Replies that arrive after the timeout are counted as `sequenceError` and are not used in the result (as specified in the spec research).

### 3.2 Scheduler

- Manage all operations in a single min-heap and start them in order of next run time.
- To keep 1,000 targets from sending at the same moment, spread the first start time of each operation within the range of `frequency`. The offset is determined by a hash of the operation ID so that it does not change across restarts. This corresponds to Cisco's group schedule.
- If the previous attempt has not finished, do not send and count `busy`. Because of the `frequency > timeout` validation, this does not actually happen for echo.

### 3.3 Statistics and history

Implement the three-layer model from the spec research as is.

- **Latest result**: RTT, return code, completion time.
- **Hourly aggregation**: Keep `hours-of-statistics-kept` (default 2) hour groups, each covering 60 minutes from the start. Each group has Completions, OverThresholds, RTT Sum / Sum² (64 bit) / Min / Max, distribution buckets, per-error counters (Timeouts, Busies, Drops, SequenceErrors, VerifyErrors), and Initiations.
- **History**: lives × buckets × samples. The filter (none / all / overThreshold / failures) is evaluated after the attempt.
- **Enhanced history**: Keep `buckets` (default 100) aggregations, one per `interval` (default 900 seconds).
- **Jitter statistics**: Keep RTT, one-way delay (SD / DS), positive and negative jitter, loss, late arrivals, and out-of-sequence packets, following the object definitions of CISCO-RTTMON-ICMP-MIB.

Even with 1,000 operations running with defaults, memory is expected to stay within a few tens of MB.

### 3.4 CLI and API

- The API is HTTP/JSON over a Unix domain socket (default `/run/goipslad/goipslad.sock`). Access control uses the socket's file permissions and group.
- `goipsla` is implemented with cobra, and every command accepts `-o table|json`.

| Command | Description | Cisco equivalent |
|---|---|---|
| `goipsla show operations [--tag T] [--state S] [--rc RC]` | Lists all operations, one per line. The main screen for many targets | `show ip sla summary` |
| `goipsla show config <id>` | The effective configuration with defaults filled in | `show ip sla configuration` |
| `goipsla show statistics [<id>] [--details]` | Latest result and cumulative totals | `show ip sla statistics` |
| `goipsla show statistics aggregated [<id>]` | Hourly aggregation and distribution | `show ip sla statistics aggregated` |
| `goipsla show history <id>` | History buckets | `show ip sla history` |
| `goipsla show enhanced-history <id>` | Enhanced history | `show ip sla enhanced-history` |
| `goipsla show reactions [<id>]` / `show track [<n>]` | State of reactions and tracking | `show ip sla reaction-configuration` / `show track` |
| `goipsla watch` | Displays the list with periodic refresh | None |
| `goipsla reload` | Applies the differences in the configuration file | None |
| `goipsla validate <file>` | Validates the configuration without the daemon | None |
| `goipsla restart <id>` / `goipsla reset` | Clears statistics and starts again | `ip sla restart` / `ip sla reset` |

Implementation note: `show history` and `show enhanced-history` take the ID as optional (`[<id>]`); without it they show every operation (docs/cli.md).

### 3.5 Configuration file

Common defaults are grouped into templates, and targets can be written briefly as a mapping from ID to target. IDs are used as CLI and SNMP indexes, so they must be written explicitly. This prevents accidents where reordering the list changes IDs.

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

  # Define many targets at once with the same template
  - template: wan-echo
    source-interface: eth1
    vrf: blue
    targets:
      101: 10.100.1.11
      102: 10.100.1.12
      103: fd00:100:2::11

tracks:
  - { id: 1, operation: 101, mode: reachability, delay: { up: 10s, down: 5s } }

actions:
  - on: [track-down, track-up]
    track: 1
    webhook: https://example.invalid/hook
  - on: [threshold-exceeded]
    exec: /usr/local/libexec/ipsla-notify.sh
```

An operation that omits `schedule` starts immediately when loaded and runs indefinitely. This differs from the Cisco defaults (`start-time pending`, `life 3600`), but for a daemon whose configuration file is the source of truth, "it runs once you write it" is more natural, so this is documented as an intentional deviation. If `schedule` is written, `life`, `start-time`, `ageout`, and `recurring` can be used with the same meaning as in Cisco.

### 3.6 Reload semantics

- Compare the old and new configurations keyed by ID.
- An operation whose measurement-related items changed is destroyed and recreated, and its statistics are reset. This matches Cisco's statement that "a scheduled operation cannot be changed unless it is deleted and recreated".
- If only reactions, tracking, or actions changed, only the evaluation state is reset, without stopping measurement or statistics.
  Implementation note: the reaction state is reset only for an operation whose `react` changed (a change of `tag` or `owner` alone keeps it), a track keeps its state while its ID, operation and mode are unchanged, and a change to `actions` is not applied until goipslad restarts (reload reports it as a warning); see docs/scheduling.md, "Reload (P4)", and docs/reactions.md, "Initialization".
- A configuration that fails validation is not applied at all. The daemon keeps running with the old configuration and returns the errors to the CLI and the log.

### 3.7 Events and external integrations

- State transitions of reactions and tracking are published to the event bus, and each sink subscribes to it. As in Cisco, there is one notification per transition.
- **syslog / JSON logs**: Use wording close to Cisco's `%RTT-3-IPSLATHRESHOLD` and similar messages. Cisco does not publish the exact format, so we define our own.
- **Webhook / exec**: Pass events as JSON. On failure, retry a limited number of times without stopping measurement.
- **Prometheus**: Expose the latest RTT, jitter, loss, return code, cumulative counters, and tracking state at `/metrics`. The labels are `id`, `type`, `target`, `tag`, and `vrf`.
- **SNMP**: Connect to net-snmp's snmpd as an AgentX subagent and expose the ICMP-related tables of CISCO-RTTMON-MIB and CISCO-RTTMON-ICMP-MIB read-only. Traps send `rttMonNotification`.

## 4. Phase plan

The completion criteria of each phase include verifying the behavior in the staging environment.

| Phase | Content | Completion criteria |
|---|---|---|
| P0 Foundation | Go module, directory layout, Makefile, CI for lint and tests, a mechanism to run `goipslad` in the source container | An empty daemon and CLI start in staging |
| P1 Minimal echo path | `probe` engine (IPv4 / IPv6, source selection, ToS, VRF, receive timestamps, matching, timeouts), `icmp-echo`, scheduler, loading and validating the configuration | Measure 6 targets in parallel over IPv4 / IPv6, and the latest RTT and return code appear in the log |
| P2 Statistics and CLI | All return codes (ok, overThreshold, timeout, sequenceError, verifyError, busy, dropped), `verify-data`, hourly statistics, distribution, history, enhanced history, API, the `goipsla show` commands | Add delay and loss with netem, and statistics and history match the expected values |
| P3 icmp-jitter | RTT, one-way delay, positive and negative jitter, loss, late arrivals, and out-of-sequence packets using ICMP Timestamp. Statistics conforming to ICMP-MIB | netem delay, jitter, and reordering are reflected in the statistics |
| P4 Schedule and reload | life, start-time, ageout, recurring, state machine, `restart` and `reset`, applying reload differences | Unit tests of state transitions, and confirming that reload does not clear unrelated statistics |
| P5 Reactions and tracking | The 4 threshold-types, monitored elements, tracking (state / reachability, delay up/down), event bus, syslog, JSON logs, webhooks, exec | Stop and restore targets, and exactly one notification is emitted per transition |
| P6 Prometheus | `/metrics` | Scraping with 1,000 operations completes in a practical time |
| P7 SNMP | AgentX subagent, reading MIB tables, traps | Add snmpd to staging and verify with `snmpwalk` and trap reception |
| P8 Distribution and scale testing | Static binary, systemd unit (`AmbientCapabilities=CAP_NET_RAW`), container image, load test with 1,000 operations, user documentation | Run 1,000 targets at 1-second intervals and measure missed runs, CPU, and memory |
| Next stage | `path-echo`, `path-jitter` | — |

P1 through P3 are the core of measurement and are solidified first. P6 and P7 can proceed in parallel with P4 and P5 once the P2 data model exists.

## 5. Test strategy

- **Unit tests**: Replace the clock and sockets through interfaces and deterministically verify the scheduler, statistics, reactions, and state machine. Use the Cisco examples in the spec research (such as the example where an average of 5667 ms violates an average threshold, and the boundary examples of distribution buckets) as test cases.
- **Fuzz tests**: Parsing ICMP packets and loading the configuration file.
- **Integration tests**: Run `go test -tags=integration` from inside the source container in the staging environment. Add delay, jitter, loss, and reordering with netem, and create unreachability by stopping target containers.
- **Scale testing**: Attach many secondary addresses to the target containers in staging to create 1,000 targets and measure them.

## 6. Additions to the staging environment

- For scale testing, add a script that attaches secondary addresses to the target containers in bulk.
- For VRF testing, add a procedure that creates a VRF device in the source container.
- In P7, add an snmpd container (AgentX master) and a trap receiver container.
- Run a prebuilt Go binary in the source container. The build is done on the host with static linking and passed through the `/work` mount.

## 7. Items Cisco does not document, decided here

Of the "undocumented items" in the spec research, those relevant to the first release proceed with the following proposals. If there are objections, they are changed before implementation.

| Item | Adopted proposal |
|---|---|
| Echo payload format | The data is `8 + request-data-size` bytes (the same on-wire size as Cisco's "internal timestamps 8 B + request size"). A custom header (magic 4 B, operation ID 4 B, sequence 4 B, send time 8 B) is placed at the start, and the rest is filled with the pattern. A request-data-size below 28 bytes is not accepted |
| RTT resolution | Kept internally in nanoseconds, truncated to milliseconds for Cisco-compatible display and SNMP. Exported to Prometheus as floating-point seconds |
| Success or failure of overThreshold | Following the MIB definition, RTT is aggregated as part of completions. The CLI shows "success", "over threshold", and "failure" in separate columns |
| When Destination Unreachable is received | End the attempt without waiting for the timeout, and set the return code to `timeout`. Record unreachable in the detail field |
| Threshold boundaries | Exceeding is "greater than the upper limit", and recovery is "less than the lower limit" |
| average window | A moving average of the last N attempts |
| xofy window | The window is not cleared on a transition; the decision is always based on the last y attempts |
| One-way delay of icmp-jitter | There is no guarantee that the target's clock is synchronized with the source, so one-way delay is not aggregated by default. It is aggregated only when enabled in the configuration |
| syslog wording | Define and document a custom format resembling Cisco's `%RTT-` format |
