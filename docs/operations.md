# Operations guide

This document explains how to install and operate goipslad (the daemon) and goipsla (the CLI). For details of individual configuration keys and output, see each reference (the "Document list" at the end).

## 1. Overview

- `goipslad` periodically runs the operations written in the configuration file (`icmp-echo`, `icmp-jitter`) and collects statistics with the same definitions as Cisco IP SLA.
- `goipsla` queries goipslad over a local Unix socket and displays state and statistics.
- External outputs are Prometheus (`/metrics`), SNMP (reading CISCO-RTTMON-MIB and traps; AgentX), syslog, webhooks, and external scripts (exec).
- Statistics are not persisted. They are lost on restart (same as Cisco). Store values you want to keep long-term in Prometheus.

## 2. Installation

With every method, the only privilege required is `CAP_NET_RAW` (section 3).

### tar (statically linked binaries)

`goipsla-x.y.z-linux-{amd64,arm64}.tar.gz`, built with `make dist VERSION=x.y.z`, contains `goipslad`, `goipsla`, `goipslad.service`, `config.example.yaml`, `README.md`, and `docs/`.

```sh
tar xzf goipsla-x.y.z-linux-amd64.tar.gz
cd goipsla-x.y.z-linux-amd64
sudo install -m 0755 goipslad goipsla /usr/local/bin/
```

### systemd

`goipslad.service` is a unit that runs as an unprivileged transient user (`DynamicUser=yes`) with only `CAP_NET_RAW`. Installation steps, hardening settings, and adjustments for external integrations are in [packaging/systemd/README.md](../packaging/systemd/README.md).

```sh
sudo install -m 0644 goipslad.service /etc/systemd/system/
sudo install -D -m 0644 config.example.yaml /etc/goipslad/config.yaml   # edit this
sudo goipsla validate /etc/goipslad/config.yaml
sudo systemctl daemon-reload && sudo systemctl enable --now goipslad
```

### Docker

```sh
make image
docker run -d --name goipslad --network host --cap-drop ALL --cap-add NET_RAW \
  -v /etc/goipslad:/etc/goipslad:ro -v /run/goipslad:/run/goipslad goipsla:dev
```

For details, see [packaging/docker/README.md](../packaging/docker/README.md).

## 3. Privileges

- The only privilege goipslad needs is `CAP_NET_RAW`. It is used to open raw ICMP sockets and to bind to VRF devices (`SO_BINDTODEVICE`).
- **Do not run it as root.** With `DynamicUser=yes` + `AmbientCapabilities=CAP_NET_RAW` in the systemd unit, or with `setcap cap_net_raw+ep /usr/local/bin/goipslad`, it can run as a regular user.
- It has been confirmed that IPv4 / IPv6 Echo, ICMP Timestamp, `source-interface`, and `vrf` work for a non-root user that has only `CAP_NET_RAW`.
- `goipsla` itself needs no privileges. However, it must be able to connect to the API socket. The socket permissions are 0660.
  - By default, the socket's group is goipslad's group, so `goipsla` is used as root (or as the same user or group as goipslad).
  - If you set `global.api-socket-group: goipsla` in the configuration, goipslad changes the socket's group to `goipsla` at startup. Users in that group can use `goipsla` without sudo.
  - If goipslad does not belong to that group, it cannot change the group and fails to start (`chgrp ...: operation not permitted`). With systemd, add `SupplementaryGroups=goipsla`. The steps are in [packaging/systemd/README.md](../packaging/systemd/README.md).
  - Like the rest of `global`, a change to `api-socket-group` takes effect at the next start.

## 4. Configuration

The configuration file (default `/etc/goipslad/config.yaml`) is the single source of truth. Keys, defaults, and validation rules are in [config.md](config.md). A commented example is [packaging/config.example.yaml](../packaging/config.example.yaml).

### Minimal example

```yaml
operations:
  - { id: 1, type: icmp-echo, target: 192.0.2.1 }
```

The defaults are the same as Cisco (frequency 60s, timeout 5000ms, threshold 5000ms, request-data-size 28). However, an operation that omits `schedule` starts when loaded and runs indefinitely (the Cisco defaults are `start-time pending`, `life 3600`; see the deviations table in [config.md](config.md)).

### Many targets

Group common settings into a template and list "ID: target" entries under `targets`. IDs become goipsla and SNMP indexes, so write them explicitly and do not reuse them for a different target.

```yaml
templates:
  wan:
    type: icmp-echo
    frequency: 10s
    timeout: 2000ms
    threshold: 300ms
    tag: wan
operations:
  - template: wan
    targets:
      101: 192.0.2.1
      102: 192.0.2.2
      103: "2001:db8::1"
```

It is designed for up to 1,000 concurrent operations in a single process.

### Validation

```sh
goipsla validate /etc/goipslad/config.yaml          # OK: 3 operations
goipsla validate --print /etc/goipslad/config.yaml  # effective configuration with defaults filled in
```

## 5. Starting and reload

| Action | systemd | Docker | Direct |
|---|---|---|---|
| Start | `systemctl start goipslad` | `docker run ...` | `goipslad --config FILE` |
| Reload the configuration | `systemctl reload goipslad` | `docker kill --signal HUP goipslad` | `goipsla reload` or SIGHUP |
| Logs | `journalctl -u goipslad` | `docker logs goipslad` | Standard error |

- **Effect of reload**: An operation whose measurement-related settings (target, frequency, timeout, and so on) changed is recreated, and its statistics are cleared. One where only tag / owner / react changed is updated while keeping its statistics. `goipsla reload` shows the IDs that were added, removed, recreated, or updated.
- **If the configuration has errors**: The reload is not applied, and the running configuration continues (`goipsla reload` prints the errors one per line and exits with exit code 1).
- **Settings that require a restart**: The `global` settings (`api-socket`, `api-socket-group`, `metrics-listen`, `log`, `syslog`, `snmp`) and `actions` (webhook / exec destinations) are read only at startup. A reload does not apply changes to them, and `goipsla reload` prints `warning: ... requires a restart of goipslad`. They take effect at the next start.
- **Logging settings**: `--log-format json` and `--log-level debug` change the log format and level (they take precedence over `global.log` in the configuration).
  - By default, logs of attempt results are thinned as follows. With `--log-results`, the results of all attempts are logged at info.
    - The result of the first attempt of each life is logged at info as `result first` (with a `life` attribute), regardless of the return code.
    - After that, when the return code changes, `result changed` is logged at info (with the previous return code in `from`). While a non-ok return code continues, it is summarized at info as `result repeated` (with `count`) once every 60 seconds.
    - Repeated ok results are not summarized (they appear only at debug, so that 1,000 operations do not produce 1,000 lines per minute). Use `--log-results` when you want to see them.
    - When a new life begins (restart, reset, a reload that changed measurement-related settings, a schedule restart), the previous state is discarded and logging starts again from `result first`. An operation that a reload removed and recreated goes back to life 1, so in addition to a change of life, a Seq that does not increase is also treated as a new life.
    - busy and sequenceError are logged at info once every 60 seconds per operation and return code (with the number of thinned entries in `suppressed`).
  - debug is for development and investigation. Do not use it routinely in production. The raw socket receives all ICMP replies on the host, so at debug a line is also emitted for each ping reply of other processes. The number of discarded packets can be seen in the metric `goipsla_probe_packets_dropped_total`.

## 6. Main goipsla commands

For details, see [cli.md](cli.md). RTT in the tables is in ms with 3 decimal places, and times are local time.

| Command | Description | Cisco equivalent |
|---|---|---|
| `goipsla show operations [--tag T] [--state S] [--rc RC]` | List with one operation per line | `show ip sla summary` |
| `goipsla watch` | Refreshes the list on screen periodically | — |
| `goipsla show statistics [<id>] [--details]` | Latest result and totals for the life | `show ip sla statistics` |
| `goipsla show statistics aggregated [<id>]` | Hourly aggregation and distribution | `show ip sla statistics aggregated` |
| `goipsla show history [<id>]`, `show enhanced-history [<id>]` | History, enhanced history | `show ip sla history` |
| `goipsla show config <id>` | Effective configuration | `show ip sla configuration` |
| `goipsla show reactions`, `show track`, `show events` | Threshold reactions, tracking, recent events | `show ip sla reaction-configuration`, `show track` |
| `goipsla restart <id>`, `goipsla reset` | Clears statistics and restarts measurement | `ip sla restart`, `ip sla reset` |
| `goipsla reload` | Reloads the configuration | — |
| `goipsla health` | Version, start time, number of operations | — |

Every command can print the API response as is with `-o json`. Use JSON from scripts.

## 7. Prometheus

`/metrics` is served on `global.metrics-listen` (default `127.0.0.1:9818`). The list of metrics is in [metrics.md](metrics.md). There is no authentication, so protect it with a firewall when scraping from outside.

```yaml
scrape_configs:
  - job_name: goipsla
    scrape_interval: 15s
    static_configs:
      - targets: ["probe-host:9818"]
```

PromQL examples:

```promql
# Latest RTT per target (ms)
goipsla_latest_rtt_seconds{tag="wan"} * 1000

# Success rate over the last 5 minutes (%)
100 * increase(goipsla_results_total{result="ok"}[5m])
  / ignoring(result) increase(goipsla_attempts_total[5m])

# Failing targets
goipsla_latest_success == 0

# Average jitter of icmp-jitter (ms)
goipsla_jitter_avg_seconds{direction="both"} * 1000
```

Counters are cumulative per life. They go back to 0 on restart / reset / a recreating reload, so handle them with `rate()` / `increase()`.

## 8. SNMP

If `global.snmp` is set, goipslad registers CISCO-RTTMON-MIB (read-only) with snmpd as an AgentX subagent and sends threshold reaction traps (`rttMonNotification`, SNMPv2c). For details, see [snmp.md](snmp.md).

```yaml
# goipslad
global:
  snmp:
    agentx: tcp:127.0.0.1:705
    traps: [ { host: 192.0.2.10, community: public } ]
```

```
# /etc/snmp/snmpd.conf
master agentx
agentXSocket tcp:127.0.0.1:705
rocommunity public default
```

```sh
# Latest results (rttMonLatestRttOperTable)
snmpbulkwalk -v2c -c public localhost 1.3.6.1.4.1.9.9.42.1.2.10
# State (rttMonCtrlOperTable)
snmpbulkwalk -v2c -c public localhost 1.3.6.1.4.1.9.9.42.1.2.9
```

- goipslad starts even if it cannot connect to snmpd (it logs a warn and reconnects every 10 seconds).
- With many operations, fetch the specific tables you need rather than the whole MIB.

## 9. Threshold reactions and external integrations

- `react` (per operation) detects threshold violations of RTT, timeout, jitter, and so on.
- `tracks` tracks the Up / Down state of operations (Cisco's `track ip sla`).
- Events are sent to the log, syslog, webhooks, exec, and SNMP traps. The rules are in [reactions.md](reactions.md), and the event JSON and delivery mechanism are in [events.md](events.md).

```yaml
tracks:
  - { id: 1, operation: 101, mode: reachability, delay: { up: 10s, down: 5s } }
actions:
  - on: [track-down, track-up]
    track: 1
    webhook: https://example.invalid/hooks/goipsla   # POSTs the event JSON (retries 3 times on failure)
  - on: [threshold-exceeded, threshold-cleared]
    exec: /usr/local/libexec/ipsla-notify.sh          # JSON on standard input, a summary in environment variables
```

```sh
#!/bin/sh
# /usr/local/libexec/ipsla-notify.sh
logger -t ipsla "$GOIPSLA_EVENT_KIND op=$GOIPSLA_OP_ID target=$GOIPSLA_TARGET value=$GOIPSLA_VALUE"
```

When running under systemd, exec scripts also run as the same transient user with the same hardening settings. If they write to files, add `ReadWritePaths=` (packaging/systemd/README.md).

## 10. Troubleshooting

| Symptom | Cause and remedy |
|---|---|
| At startup: `goipslad: cannot start the ICMP engine (need CAP_NET_RAW): ... operation not permitted` | `CAP_NET_RAW` is missing. With systemd, use `AmbientCapabilities=CAP_NET_RAW`; with Docker, `--cap-add NET_RAW` (do not add `--user`); when run directly, `setcap cap_net_raw+ep`. |
| `goipsla` reports `cannot connect to /run/goipslad/goipslad.sock: no such file or directory (is goipslad running?)` | goipslad is not running, or `global.api-socket` is different. You can specify the location with `--socket`. |
| `goipsla` reports `... permission denied` | You have no permission on the socket (0660). Run as root, or add the user to the group set in `global.api-socket-group` (adding a user to a group takes effect after the user logs in again). |
| At startup: `chgrp /run/goipslad/goipslad.sock: operation not permitted` | goipslad does not belong to the `api-socket-group` group. With systemd, add that group to `SupplementaryGroups=`. |
| All targets, or specific targets, keep reporting `timeout` | Check with `ping` that ICMP is not dropped by a firewall on the path and that the target replies. Rate limiting on the target (such as Linux `net.ipv4.icmp_ratelimit`) may also thin out replies at short frequencies. Also check that `timeout` is not too short compared with the RTT. Attempts that received Destination Unreachable also become `timeout`, and the sender appears in `Latest operation detail` of `show statistics <id>`. |
| Only icmp-jitter reports `timeout` | The target does not reply to ICMP Timestamp (Type 13) (IOS-XR and some operating systems do not reply). Some firewalls do not pass Timestamp. icmp-jitter is IPv4 only. |
| At startup: `probe: cannot open the ipv6 raw socket; ipv6 operations will fail` | IPv6 is disabled in the kernel. IPv6 operations become `error`. IPv4 works. |
| `sequence errors` (late arrivals) increase | Replies arrive after `timeout` has passed. Late arrivals are not counted toward the next attempt; they are counted as sequenceError (same as Cisco). Look at the RTT distribution and increase `timeout` (within `timeout < frequency`). |
| A reload fails with `invalid configuration` | Fix the displayed `<path>: <message>` and reload again. goipslad keeps running with the original configuration. |
| `busy` increases | The next frequency interval arrives before the previous attempt finishes. The configuration prevents this for icmp-echo (validation enforces `timeout < frequency`). For icmp-jitter, check `timeout + interval × num-packets ≤ frequency`. |

## 11. Differences from Cisco

The semantics (defaults, return codes, statistics, reactions, state machine) match Cisco. Intentional changes are summarized in the deviations table of each document.

| Area | Document |
|---|---|
| Configuration (schedule defaults, validation rules) | "Intentional deviations from Cisco" in [config.md](config.md) |
| Scheduling and the state machine | "Deviations from Cisco" in [scheduling.md](scheduling.md) |
| Statistics, history, enhanced history | "Intentional deviations from Cisco" in [statistics.md](statistics.md) |
| Threshold reactions and tracking | "Correspondence with Cisco" in [reactions.md](reactions.md) |
| SNMP | "Differences from Cisco (summary)" in [snmp.md](snmp.md) |
| ICMP engine (packets, RTT measurement) | "Known limitations" in [probe-engine.md](probe-engine.md) |

## Document list

| Document | Content |
|---|---|
| [config.md](config.md) | Configuration file reference |
| [cli.md](cli.md) | goipsla command reference |
| [api.md](api.md) | Control API (HTTP / JSON over a Unix socket) |
| [metrics.md](metrics.md) | Prometheus metrics |
| [snmp.md](snmp.md) | SNMP (AgentX and traps) |
| [events.md](events.md) | Events and delivery destinations |
| [reactions.md](reactions.md) | Rules for threshold reactions and tracking |
| [statistics.md](statistics.md) | Statistics accounting model |
| [scheduling.md](scheduling.md) | Periodic execution, schedules, reload |
| [probe-engine.md](probe-engine.md) | ICMP engine implementation |
