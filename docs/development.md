# Development guide

How to build, test and lint `goipslad` / `goipsla`, and how to run them in the staging environment.
Design decisions are in [implementation-plan.md](implementation-plan.md); the contracts between packages are described in the Go doc comments of each package.

## Requirements

| Tool | Purpose | Notes |
|---|---|---|
| Go 1.26 | Build and test | The module path is `goipsla` |
| C compiler | `go test -race` | The race detector uses cgo. The build itself is `CGO_ENABLED=0` |
| GNU Make | Targets | |
| Docker and Docker Compose v2 or later | Staging environment | The user running it must be in the `docker` group |
| golangci-lint v2 | `make lint` | Optional. Without it, `make lint` prints how to install it and fails |

## Directories

```
cmd/goipslad/           daemon entry point
cmd/goipsla/            CLI entry point (cobra)
internal/api/           control API over the Unix socket (server and Go client)
internal/api/apitest/   fake api.Provider for CLI and API tests
internal/clock/         clock abstraction (Real, and Fake for tests)
internal/config/        configuration types, loading, validation
internal/event/         event bus and sinks (log, syslog, webhook, exec)
internal/manager/       operations of a configuration: schedule states, life, reload
internal/metrics/       Prometheus exporter
internal/op/            a single attempt and return codes
internal/probe/         ICMP engine
internal/react/         threshold reactions and tracking
internal/sched/         periodic runs on a fixed time grid
internal/snmp/          CISCO-RTTMON-MIB over AgentX, and traps
internal/snmp/agentx/   AgentX subagent (RFC 2741)
internal/stats/         statistics, history and enhanced history
tools/probe-echo/       development CLI for the ICMP engine
tools/metrics-get/      fetches /metrics in the staging containers (which have no curl)
mibs/                   MIB files for the staging snmpd (not in the repository; see docs/snmp.md, "MIB files")
packaging/              systemd unit, Dockerfile, example configuration
staging/                Docker Compose staging environment
```

## Build

```bash
make build
```

`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)"` builds statically linked `bin/goipslad`, `bin/goipsla`, `bin/probe-echo` (a development CLI for the ICMP engine) and `bin/metrics-get` (fetches `/metrics` in the staging containers, which have no curl or wget).
`VERSION` defaults to the output of `git describe`, or `0.0.0-dev` outside Git. To set it, use `make build VERSION=1.2.3`.

```console
$ file bin/goipslad
bin/goipslad: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, ...
$ bin/goipslad --version
goipslad 0.0.0-dev (go1.26.7 linux/amd64)
$ bin/goipsla version
goipsla 0.0.0-dev (go1.26.7 linux/amd64)
```

## Tests

```bash
make test    # go test -race ./...
make vet     # go vet ./...
make lint    # golangci-lint run (configured in .golangci.yml)
make all     # vet, test and build together
```

- Always take the time from `internal/clock`. In unit tests, use `clock.NewFake(start)` and move the time with `Advance` / `Set`.
  Timers that are due fire in deadline order. To wait until the goroutine under test has armed its timer, use `BlockUntil(n)` (waits until `n` or more timers are pending).
- Tests that need raw sockets carry `//go:build integration` at the top of the file, so that the usual `go test ./...` does not run them.
  Write them to `t.Skip` outside the staging environment.

### Integration tests (staging)

The staging containers have no Go. So the test binaries are built statically on the host and run in the source container through the `/work` mount.

```bash
make staging-integration-test                      # all packages
make staging-integration-test ITEST_FLAGS=-test.v  # flags for the test binaries
```

For each package with tests, `go test -c -tags=integration` builds `bin/itest/<package>.test`, which is run in the source container with that package's directory (`/work/internal/...`) as the working directory. `testdata/` can be read with relative paths too.

`make integration-test` runs `go test -tags=integration ./...` as is. Use it where Go is available.

## Staging environment

For its layout, see [staging/README.md](../staging/README.md). There is one source, one router and six targets; the source container mounts the repository at `/work`.

```bash
make staging-up        # start; running containers are reused
make staging-verify    # check reachability, routes and ICMP Timestamp from the source to every target
make staging-down      # stop and remove
make staging-rebuild   # after changing staging/node/: rebuild the image and recreate every container
```

`staging-up` builds the image only when it does not exist. `docker compose up -d --build` creates a new image ID every time, even when the layers are cached, so every running container would be recreated, losing the netem settings and a running `goipslad`.

To work in the source container:

```bash
docker compose -f staging/compose.yaml exec source bash
```

## Running goipslad in staging

```bash
make staging-run                                  # with the default configuration
make staging-run CONFIG=staging/goipslad.yaml       # with a given configuration
staging/scripts/run-goipslad.sh /dev/null --log-format json --log-level debug
```

`staging/scripts/run-goipslad.sh [config] [goipslad flags...]` does the following.

1. Builds the binaries on the host with `staging/scripts/build.sh` (= `make build`).
2. Runs them in the source container with `docker compose -f staging/compose.yaml exec -T source /work/bin/goipslad --config <config>`. The log goes to standard error.
3. On Ctrl-C (SIGINT) or SIGTERM, forwards the signal to `goipslad` in the container and waits for its shutdown log. The exit status is that of `goipslad`.

How `config` is handled:

- When omitted, `staging/goipslad.yaml` if it exists, otherwise `/dev/null`.
- A file in the repository, given as a relative or absolute path, is mapped to `/work/...` in the container.
- Any other path is passed as is, as a path in the container.

Stopping the `docker compose exec -T` client does not deliver a signal to the process in the container. If you run it directly with `docker compose exec` instead of the script, stop it with:

```bash
docker compose -f staging/compose.yaml exec -T source pkill -TERM -x goipslad
```

Likewise, to try a reload (SIGHUP), send `pkill -HUP -x goipslad` in the container. When `run-goipslad.sh` itself receives SIGHUP, it assumes the terminal was closed and stops `goipslad`.

## goipslad flags

| Flag | Default | Description |
|---|---|---|
| `--config` | `/etc/goipslad/config.yaml` | Configuration file |
| `--log-format` | `text` | `text` or `json` |
| `--log-level` | `info` | `debug` / `info` / `warn` / `error` |
| `--log-results` | | Log every attempt result at info (by default, results are thinned; see the "Logging settings" item in `docs/operations.md`) |
| `--version` | | Print the version and exit |

The log goes to standard error through `log/slog`. goipslad exits on SIGINT / SIGTERM.

## Common goipsla flags

| Flag | Default | Description |
|---|---|---|
| `-o`, `--output` | `table` | `table` or `json` |
| `--socket` | `/run/goipslad/goipslad.sock` | API socket of `goipslad` |

Subcommands register themselves with `registerCommand` from the `init` of each file (see `cmd/goipsla/root.go`).
