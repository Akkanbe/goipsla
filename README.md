# goipslad / goipsla

A Go daemon, `goipslad`, and CLI, `goipsla`, that perform the same ICMP measurements as Cisco IOS IP SLA (`icmp-echo`, `icmp-jitter`) on Linux.

Semantics such as return codes, defaults, statistics definitions, threshold reactions, and the schedule state machine follow Cisco. The CLI output has its own design, so that many targets can be viewed at a glance.

```console
$ goipsla show operations
ID   TYPE         TARGET          TAG  STATE    RC       RTT(ms)  LAST      SUCC  FAIL  AVG(ms)  JIT(ms)  LOSS
1    icmp-jitter  10.100.2.11     -    active   ok       0.412    2s ago    40    0     1.000    0.143    1/10
101  icmp-echo    10.100.1.11     wan  active   ok       1.234    2m9s ago  115   5     2.000    -        -
102  icmp-echo    fd00:100:2::11  wan  active   timeout  -        6s ago    0     12    -        -        -
103  icmp-echo    10.100.1.13     lan  pending  -        -        -         0     0     -        -        -
```

## Features

- **Operations**
  - `icmp-echo` (IPv4 / IPv6): request-data-size, data-pattern, verify-data, ToS / Traffic Class, Flow Label
  - `icmp-jitter` (IPv4, ICMP Timestamp): per-direction jitter, loss, consecutive loss, late arrivals, out-of-sequence, one-way delay (optional)
- **Source control**: source IP, source interface, Linux VRF
- **Scale**: up to 1,000 concurrent operations in one process. Only one raw socket is used per address family and VRF
- **Statistics** (same accounting as CISCO-RTTMON-MIB): latest result, hourly aggregation and distribution, history (lives × buckets), enhanced history
- **Scheduling**: life, start-time (now / pending / after / time of day), ageout, recurring. States are pending / inactive / active
- **Threshold reactions** (never / immediate / consecutive / xofy / average) and tracking (state / reachability, with delay)
- **Integrations**
  - Prometheus `/metrics`
  - SNMP: an AgentX subagent exposes CISCO-RTTMON-MIB read-only and sends traps
  - syslog, JSON logs, webhooks, external scripts (exec)
- **Configuration**: the YAML file is the single source of truth. Templates and "ID: target" tables keep many targets short. `goipsla reload` / SIGHUP applies only the differences (statistics are kept for changes that do not affect measurement)
- **Distribution**: a single statically linked binary (linux amd64 / arm64), a systemd unit, and a container image. The only privilege required is `CAP_NET_RAW`

## Quick start

```sh
make build                              # bin/goipslad, bin/goipsla (statically linked)
cat > /tmp/goipslad.yaml <<'EOF'
global:
  api-socket: /tmp/goipslad.sock
operations:
  - { id: 1, type: icmp-echo, target: 192.0.2.1, frequency: 5s, timeout: 1000ms, threshold: 200ms }
EOF
bin/goipsla validate /tmp/goipslad.yaml
sudo setcap cap_net_raw+ep bin/goipslad  # run with only CAP_NET_RAW instead of root
bin/goipslad --config /tmp/goipslad.yaml &
bin/goipsla --socket /tmp/goipslad.sock show operations
bin/goipsla --socket /tmp/goipslad.sock show statistics 1
```

In production, use the systemd unit ([packaging/systemd/](packaging/systemd/)) or the container image ([packaging/docker/](packaging/docker/)). The procedures are in [docs/operations.md](docs/operations.md).

| Artifact | How to build |
|---|---|
| Release tarball | `make dist VERSION=x.y.z` → `dist/goipsla-x.y.z-linux-{amd64,arm64}.tar.gz` |
| Container image | `make image` → `goipsla:dev` |

## Documentation

### For users

| Document | Contents |
|---|---|
| [docs/operations.md](docs/operations.md) | Operations guide (installation, privileges, configuration, startup, reload, Prometheus, SNMP, integrations, troubleshooting) |
| [docs/config.md](docs/config.md) | Configuration file reference |
| [docs/cli.md](docs/cli.md) | goipsla command reference |
| [docs/metrics.md](docs/metrics.md) | Prometheus metrics |
| [docs/snmp.md](docs/snmp.md) | SNMP (AgentX and traps) |
| [docs/events.md](docs/events.md), [docs/reactions.md](docs/reactions.md) | Events and destinations, threshold reactions and tracking |
| [docs/statistics.md](docs/statistics.md), [docs/scheduling.md](docs/scheduling.md) | Statistics accounting model, scheduling and reload semantics |
| [docs/api.md](docs/api.md) | Control API (HTTP / JSON over a Unix socket) |
| [packaging/config.example.yaml](packaging/config.example.yaml) | Commented configuration example |

### For developers

| Document | Contents |
|---|---|
| [docs/development.md](docs/development.md) | Build, test, lint, running in staging |
| [docs/implementation-plan.md](docs/implementation-plan.md) | Implementation plan and decision records |
| [docs/probe-engine.md](docs/probe-engine.md) | ICMP engine implementation |
| [reports/cisco-ip-sla-icmp-spec-research.md](reports/cisco-ip-sla-icmp-spec-research.md) | Spec research on Cisco IP SLA (ICMP) (the basis for the semantics) |
| [staging/README.md](staging/README.md) | Docker Compose staging environment |

Main development commands:

```sh
make test                          # unit tests (-race)
make lint                          # golangci-lint
make staging-up                    # start the staging environment
staging/scripts/verify-all.sh      # run all staging integration checks
```

## License

To be decided.
