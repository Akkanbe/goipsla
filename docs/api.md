# Control API reference

`goipslad` serves an HTTP/1.1 and JSON API on a local Unix domain socket. `goipsla` is a client of this API.
The API is not exposed to the network. Access control relies only on the permissions and group of the socket file.

## Connection

| Item | Value |
|---|---|
| Socket | `global.api-socket` in the configuration (default `/run/goipslad/goipslad.sock`) |
| Protocol | HTTP/1.1. The Host header can be anything (`goipslad` in the examples) |
| Format | JSON for both requests and responses (`Content-Type: application/json`) |
| Paths | `/v1/...` |

To try it with `curl`:

```console
$ curl --unix-socket /run/goipslad/goipslad.sock http://goipslad/v1/health
```

### Handling of the socket file

- If the parent directory does not exist, it is created with mode 0750.
- At startup, goipslad first takes an exclusive `flock` on the lock file `<socket path>.lock` (for example `/run/goipslad/goipslad.sock.lock`, mode 0600).
  - If the lock cannot be taken, another goipslad is assumed to be running and startup stops.
  - The lock is held until the daemon stops. The lock file itself stays after the daemon stops (removing it would open a window in which two processes hold two different lock files at the same time).
  - Everything from checking an existing socket to listening happens under this lock, so even if two processes start at the same time, only one gets the socket.
- If a socket file already exists, goipslad tries to connect to it.
  - Only when the connection is refused (`ECONNREFUSED`, nobody is listening) is the file treated as stale, removed and recreated.
  - If the connection succeeds (another goipslad is running), startup stops with an error.
  - If the connection fails for another reason, such as insufficient permission (`EACCES`) or a timeout, the socket may belong to a live daemon, so it is not removed and startup stops with an error.
  - If a regular file (not a socket) is there, it is not removed and startup fails.
- The new socket gets the configured mode (for example 0660) and group. If the group name cannot be resolved, startup fails. The group can be written as a name or as a numeric gid.
- At shutdown, the socket file is removed and the lock is released.
- Group names are resolved from `/etc/group` only (goipslad is built without cgo, so NSS modules such as LDAP or SSSD are not used). Write other groups as a numeric gid.

### Connections and logging

- Request headers must arrive within 5 seconds. The server closes a keep-alive connection that has been idle for 60 seconds.
- At shutdown, requests in progress are given up to 5 seconds. After that, the remaining connections are closed and the warning `api shutdown timed out; closing the remaining connections` is logged.
- The request log `api request` has `method`, `path`, `status` and `duration_ms` (and `op` for restart). POST requests that change state (reload, restart, reset) are logged at info, GET requests at debug.
- A 5xx response is logged as the error `api request failed` (`method`, `path`, `status`, `err`). 501 is an exception and does not produce this log: 501 is a deliberate response from `api.ErrNotImplemented` (for example `GET /v1/reactions` asked of a Provider that does not handle events), not a failure that needs the operator's attention. A 501 still appears in the request log `api request` (at debug, since it is a GET).
- When a handler panics, the error `api handler panicked` (`method`, `path`, `panic`, `stack`) is logged and 500 with `{"error": "internal error"}` is returned. The daemon keeps serving requests.

## JSON conventions

- Field names are in snake case.
- Times are RFC 3339 in UTC, with up to nanoseconds (for example `"2026-09-27T12:00:00.001500123Z"`).
- The RTT of the latest result is given as a fractional number of milliseconds, `rtt_ms`. It is rounded to the microsecond, so it has at most 3 decimals.
- Statistics accumulators (`rtt_sum_ms`, `rtt_min_ms` and so on) are integers truncated to milliseconds (as in Cisco / the MIB).
- Return codes are given as the MIB enumeration names (`ok`, `overThreshold`, `timeout`, `busy`, `dropped`, `sequenceError`, `verifyError`, `error`, and `other` before the first attempt).
- Only inside the effective configuration (`config`), keys are in kebab case, as in the configuration file. Durations are also written as in the configuration file, such as `"5s"` or `"2000ms"`.
- Response JSON is indented.

## Errors

Errors are returned with a body of the following form and the corresponding HTTP status.

```json
{ "error": "operation 999 not found" }
```

| Status | Meaning | Examples |
|---|---|---|
| 400 | Bad request | ID not an integer, unknown query parameter (any parameter on endpoints that take none), a repeated parameter (except `include`), unknown value or empty element in `include`, invalid filter value |
| 400 | Invalid configuration file | Validation of the configuration failed on reload (below) |
| 404 | Not found | Unknown operation ID, unknown path |
| 405 | Wrong method | `POST /v1/operations` (the `Allow` header gives the right method) |
| 409 | Wrong state | Restart of an operation that is not active (`api.ErrNotActive`; the body contains `"operation is not active"`) |
| 500 | Internal daemon error | |
| 501 | Not implemented | `GET /v1/reactions` / `tracks` / `events` called on a Provider that does not implement `api.EventProvider` |

When validation of the configuration file fails on reload (the Provider returns a `*config.ValidationError`), the response is 400 with the following body. `errors` lists every error found, each in the form `"<path>: <message>"` (`config.FieldError.String()`). An error without a path (a YAML syntax error) is just `"<message>"` (for example `"yaml: line 3: did not find expected key"`). The bodies of other errors have no `errors`.

```json
{
  "error": "invalid configuration",
  "errors": [
    "operations[0].timeout: must be at least 1ms",
    "operations(id=5).frequency: must be greater than timeout"
  ]
}
```

When the Provider returns one of these errors, the server picks the status as follows: `api.ErrNotImplemented` → 501, `api.ErrNotFound` → 404, `api.ErrNotActive` → 409, `*config.ValidationError` (checked with `errors.As`, so it may be wrapped) → 400, anything else → 500.

## Endpoints

`/v1/health`, `/v1/reload`, `/v1/reset` and `/v1/operations/{id}/restart` take no query parameters. Any parameter results in 400.

| Method and path | Description | Response on success |
|---|---|---|
| `GET /v1/health` | Daemon status | 200 `Health` |
| `GET /v1/operations` | List of operations | 200 array of `OperationRow` |
| `GET /v1/operations/{id}` | Detail of one operation | 200 `OperationDetail` |
| `POST /v1/reload` | Reload the configuration file | 200 `ReloadResult` |
| `POST /v1/operations/{id}/restart` | Discard the statistics of one operation and start a new life | 204 (no body) |
| `POST /v1/reset` | Discard the statistics of every operation and start a new life | 204 (no body) |
| `GET /v1/reactions?id=` | Threshold reaction rows and their state (P5). Without `id`, all operations | 200 array of `ReactionJSON` |
| `GET /v1/tracks?id=` | Tracking (P5). Without `id`, all tracks | 200 array of `TrackJSON` |
| `GET /v1/events?limit=100` | Recent events (P5), oldest first. `limit` is 1 to 1000, default 100 | 200 array of `EventJSON` |

### `GET /v1/health`

```json
{
  "version": "0.3.0",
  "started_at": "2026-09-27T10:30:00Z",
  "config_path": "/etc/goipslad/goipslad.yaml",
  "operations": { "total": 4, "active": 3, "pending": 1, "inactive": 0 }
}
```

### `GET /v1/operations`

The list can be filtered with query parameters. All are optional; several are combined with AND. Any other parameter, or the same parameter given twice or more, results in 400.

| Parameter | Value | Condition |
|---|---|---|
| `tag` | String | The tag matches exactly |
| `state` | `pending` / `inactive` / `active` | The state matches |
| `rc` | MIB name of a return code (case-insensitive) | The return code of the latest result matches. Operations not yet attempted have `other` |
| `type` | `icmp-echo` / `icmp-jitter` | The type matches |
| `include` | `detail`, `hours`, `history`, `enhanced`, comma-separated (may be repeated) | With it, the response is an array of details (`OperationDetail`) instead of rows (`OperationRow`) (see "Bulk retrieval" below) |

The response is sorted by ascending ID. If nothing matches, `[]` is returned.

```json
[
  {
    "id": 101,
    "type": "icmp-echo",
    "target": "10.100.1.11",
    "tag": "wan",
    "vrf": "blue",
    "state": "active",
    "latest": {
      "valid": true,
      "seq": 120,
      "start": "2026-09-27T11:57:50Z",
      "end": "2026-09-27T11:57:50.001234Z",
      "rtt_ms": 1.234,
      "code": "ok"
    },
    "totals": {
      "initiations": 120, "completions": 118, "over_thresholds": 3, "timeouts": 2,
      "busies": 0, "drops": 0, "sequence_errors": 0, "verify_errors": 0,
      "successes": 115, "failures": 5,
      "rtt_sum_ms": 236, "rtt_sum2_ms": 600, "rtt_min_ms": 1, "rtt_max_ms": 9,
      "rtt_avg_ms": 2, "rtt_stddev_ms": 1.042
    }
  },
  {
    "id": 103,
    "type": "icmp-echo",
    "target": "10.100.1.13",
    "tag": "lan",
    "state": "pending",
    "latest": { "valid": false, "code": "other" },
    "totals": { "initiations": 0, "...": "..." }
  }
]
```

The fields of `OperationRow`:

| Field | Description |
|---|---|
| `id`, `type`, `target`, `tag`, `vrf` | Identify the operation. `tag` and `vrf` are omitted when empty |
| `state` | One of `pending`, `inactive`, `active` |
| `latest` | The latest attempt (corresponds to `rttMonLatestRttOper*`). See below |
| `totals` | Accumulators for the whole current life (`CountersJSON`). See below |
| `life_left_s` | Seconds left in the life. 0 when not active. Omitted when the life is forever (`docs/scheduling.md`) |
| `next_start` | Next start time of a pending / inactive operation. Given only when known |
| `ageout_left_s` | Seconds until an operation that is not active is deleted by ageout. Omitted when there is no ageout |

The fields of `latest`:

- `valid`: `true` once the operation has been attempted at least once. When `false`, every field except `code` (`other`) is omitted.
- `seq`: Sequence number of the attempt.
- `start` / `end`: Start and completion times of the attempt.
- `rtt_ms`: Given only when `code` is `ok` or `overThreshold`.
- `detail`: Additional information (for example `"destination unreachable: host, from 10.100.1.254"`). Omitted when there is none.

The fields of `CountersJSON`. For how they are accounted, see `docs/statistics.md`.

| Field | Content |
|---|---|
| `initiations` | Number of attempts |
| `completions` | Number of completions (including overThreshold) |
| `over_thresholds`, `timeouts`, `busies`, `drops`, `sequence_errors`, `verify_errors` | Counts per return code |
| `successes` | `completions − over_thresholds` |
| `failures` | `initiations − successes` |
| `rtt_sum_ms`, `rtt_sum2_ms` | Sum and sum of squares of the RTTs of completions (integer ms) |
| `rtt_min_ms`, `rtt_max_ms` | Minimum and maximum RTT of completions (0 when there are no completions) |
| `rtt_avg_ms`, `rtt_stddev_ms` | Mean and standard deviation (0 when there are no completions) |

#### Bulk retrieval (`include`)

With `include`, the details of every operation that matches the filters are returned in one request. The response is an array of `OperationDetail`, the same as `GET /v1/operations/{id}` (sorted by ascending ID). This removes the need for one request per operation (CLI commands that show every operation, such as `goipsla show statistics --details`, use it).

| Value | Meaning |
|---|---|
| `detail` | The detail without additional parts (`config`, `life_start`, `life_index`, and `totals_jitter` for jitter). Valid only on the list; `GET /v1/operations/{id}` returns 400 for it |
| `hours`, `history`, `enhanced` | Same as `include` on `GET /v1/operations/{id}`: the given parts are included. The detail itself is also returned, so `detail` need not be added |

- Can be combined with the filters (`tag`, `state`, `rc`, `type`); only the details of the rows that pass them (AND) are returned.
- Examples: `GET /v1/operations?include=hours,history,enhanced`, `GET /v1/operations?tag=wan&include=detail`
- An unknown value or an empty element (`include=`, `hours,,history`) results in 400 (`must be detail, hours, history or enhanced`).
- Operations deleted between listing and fetching the details are left out of the array.

### `GET /v1/operations/{id}`

`{id}` must be a positive integer (400 otherwise). If the operation does not exist, the result is 404.

The `include` parameter gives, comma-separated, the additional parts to include.

| Value | Field added | Corresponding Cisco command |
|---|---|---|
| `hours` | `hours` (aggregated statistics per hour and distribution) | `show ip sla statistics aggregated` |
| `history` | `history` (history buckets) | `show ip sla history` |
| `enhanced` | `enhanced` (enhanced history) | `show ip sla enhanced-history` |

`include` may be repeated (`include=hours&include=history` is the same as `include=hours,history`). Every comma-separated element of every value is checked. An unknown value or an empty element (`include=`, `hours,,history`) results in 400. The fields of parts that were not requested are omitted.

The response has every field of `OperationRow` plus the following.

```json
{
  "id": 101,
  "...": "(same as OperationRow)",
  "config": {
    "id": 101, "type": "icmp-echo", "target": "10.100.1.11", "target-name": "10.100.1.11",
    "template": "wan-echo", "vrf": "blue", "tos": 184,
    "frequency": "10s", "timeout": "2000ms", "threshold": "300ms",
    "request-data-size": 28, "data-pattern": "0xABCDABCD", "verify-data": false,
    "tag": "wan", "owner": "",
    "history": { "lives-kept": 1, "buckets-kept": 15, "filter": "all",
                 "hours-of-statistics-kept": 2, "distributions-of-statistics-kept": 2,
                 "statistics-distribution-interval": "5ms",
                 "enhanced": { "interval": "60s", "buckets": 100 } },
    "schedule": null,
    "react": []
  },
  "life_start": "2026-09-27T10:00:00Z",
  "life_index": 1,
  "hours": [
    {
      "index": 1,
      "start": "2026-09-27T10:00:00Z",
      "counters": { "initiations": 60, "completions": 59, "...": "..." },
      "dist": [
        { "index": 1, "lower_ms": 0, "upper_ms": 5, "completions": 58, "over_thresholds": 0, "rtt_sum_ms": 109, "rtt_sum2_ms": 219,
          "rtt_min_ms": 1, "rtt_max_ms": 4, "rtt_avg_ms": 1.879, "percent": 98.305 },
        { "index": 2, "lower_ms": 5, "upper_ms": null, "completions": 1, "over_thresholds": 0, "rtt_sum_ms": 9, "rtt_sum2_ms": 81,
          "rtt_min_ms": 9, "rtt_max_ms": 9, "rtt_avg_ms": 9, "percent": 1.695 }
      ]
    }
  ],
  "history": [
    { "life": 1, "bucket": 16, "sample": 1, "start": "2026-09-27T11:57:50Z", "rtt_ms": 1, "code": "ok", "target": "10.100.1.11" }
  ],
  "enhanced": [
    { "index": 2, "start": "2026-09-27T11:59:00Z", "counters": { "...": "..." } }
  ]
}
```

- `config`: The effective configuration, with defaults filled in and templates expanded. Key names and value syntax are the same as in `docs/config.md`. Keys that do not apply to the type or address family are omitted. `schedule: null` means "start at load time and run forever".
- `life_start` / `life_index`: Start time of the current life and the index of the life.
- `hours[].dist[]`: Distribution buckets.
  - The range is `[lower_ms, upper_ms)`; the last bucket has `upper_ms: null` (no upper bound).
  - `percent` is the share of the completions of that hour group (0 to 100).
  - `rtt_avg_ms` is the mean within the bucket, 0 when there are no completions.
  - `over_thresholds` is the number of completions in the bucket that were overThreshold (a subset of `completions`; `rttMonStatsCaptureOverThresholds` in the MIB). The sum over all buckets equals `counters.over_thresholds` of the hour group.
- `history[].rtt_ms`: 0 for anything other than `ok` (as the MIB defines it).
- Every array is sorted oldest first.

#### icmp-jitter fields (P3)

For icmp-jitter operations, the following fields are added. They are omitted for icmp-echo.

- `latest.jitter` (also on the rows of the list `GET /v1/operations`): The result of the latest burst (`op.JitterResult`). `latest.rtt_ms` is the mean RTT of the burst, and `latest.detail` holds `"loss 1/10"` when packets were lost.
- `totals_jitter`: Accumulation over every burst of the current life (`stats.JitterCounters`). Always present, regardless of `include`.
- `hours[].jitter`: Accumulation over the bursts of that hour group. jitter has no distribution buckets, so `hours[].dist` is an empty array.
- History (`history`) is not recorded for jitter (the MIB history table does not apply to jitter).

All values are integer ms (means such as `*_avg_ms` are fractional). For the definitions of jitter and loss, see the "icmp-jitter" section of `docs/statistics.md`. A "side" such as `pos_sd` has the form `{num, sum_ms, sum2_ms, min_ms, max_ms, avg_ms}`; negative jitter is stored as its absolute value.

```json
{
  "id": 1,
  "type": "icmp-jitter",
  "target": "10.100.2.11",
  "state": "active",
  "latest": {
    "valid": true, "seq": 40, "rtt_ms": 0.412, "code": "ok", "detail": "loss 1/10",
    "jitter": {
      "num_packets": 10, "sent": 10, "skipped": 0,
      "num_rtt": 9, "rtt_sum_ms": 4, "rtt_sum2_ms": 4, "rtt_min_ms": 0, "rtt_max_ms": 1, "rtt_avg_ms": 0.444,
      "num_over_threshold": 0,
      "pos_sd": { "num": 6, "sum_ms": 1, "sum2_ms": 1, "min_ms": 0, "max_ms": 1, "avg_ms": 0.167 },
      "neg_sd": { "num": 1, "sum_ms": 1, "sum2_ms": 1, "min_ms": 1, "max_ms": 1, "avg_ms": 1 },
      "pos_ds": { "num": 7, "sum_ms": 0, "sum2_ms": 0, "min_ms": 0, "max_ms": 0, "avg_ms": 0 },
      "neg_ds": { "num": 0, "sum_ms": 0, "sum2_ms": 0, "min_ms": 0, "max_ms": 0, "avg_ms": 0 },
      "avg_jitter_ms": 0.143, "avg_sd_jitter_ms": 0.286, "avg_ds_jitter_ms": 0,
      "pkt_loss": 1, "pkt_late_arrival": 0,
      "pkt_out_seq_sd": 0, "pkt_out_seq_ds": 0, "pkt_out_seq_both": 0,
      "min_suc_pkt_loss": 1, "max_suc_pkt_loss": 1,
      "one_way": false, "num_ow": 0,
      "ow_sd": { "num": 0, "sum_ms": 0, "sum2_ms": 0, "min_ms": 0, "max_ms": 0, "avg_ms": 0 },
      "ow_ds": { "num": 0, "sum_ms": 0, "sum2_ms": 0, "min_ms": 0, "max_ms": 0, "avg_ms": 0 }
    }
  },
  "totals": { "initiations": 40, "completions": 40, "...": "(counts per burst)" },
  "totals_jitter": {
    "num_rtt": 396, "rtt_sum_ms": 160, "rtt_sum2_ms": 170, "rtt_min_ms": 0, "rtt_max_ms": 3, "rtt_avg_ms": 0.404,
    "num_over_threshold": 0,
    "pos_sd": { "...": "..." }, "neg_sd": { "...": "..." }, "pos_ds": { "...": "..." }, "neg_ds": { "...": "..." },
    "avg_jitter_ms": 0.171, "avg_sd_jitter_ms": 0.286, "avg_ds_jitter_ms": 0.057,
    "pkt_loss": 4, "pkt_late_arrival": 1,
    "pkt_out_seq_sd": 0, "pkt_out_seq_ds": 2, "pkt_out_seq_both": 0,
    "min_suc_pkt_loss": 1, "max_suc_pkt_loss": 2,
    "skipped": 0, "num_ow": 0,
    "ow_sd": { "...": "..." }, "ow_ds": { "...": "..." }
  }
}
```

- `totals` (`CountersJSON`) counts **per burst** (one attempt = one burst). Per-packet values are in `jitter` / `totals_jitter`.
- `pkt_out_seq_sd` / `pkt_out_seq_ds` / `pkt_out_seq_both` do not overlap (reordering in R only, in arrival order only, and in both).
- `min_suc_pkt_loss` / `max_suc_pkt_loss`: In `totals_jitter` and `hours[].jitter`, the minimum and maximum among the bursts that lost packets.
- `totals_jitter` has `skipped` as an integer (the total), and `latest.jitter` has the burst's `num_packets` / `sent` / `skipped`.
- `one_way` / `num_ow` / `ow_sd` / `ow_ds` have values only when the setting `one-way-delay: true` is given.

### `POST /v1/reload`

```json
{ "added": [5], "removed": [], "restarted": [101], "updated": [102], "warnings": ["templates.spare: not used by any operation"] }
```

- `restarted`: Operations rebuilt because a measurement setting changed.
- `updated`: Operations whose reactions alone changed.
- `warnings`: Settings accepted but without effect (`config.Config.Warnings`; "Warnings" in `docs/config.md`), and global changes that take effect only after a restart. Omitted when there are none.
- If validation of the configuration file fails, the response is 400 with a body carrying `errors` (see "Errors"). The daemon keeps using the running configuration.

### `POST /v1/operations/{id}/restart`, `POST /v1/reset`

On success, 204 with no body is returned. For restart, an unknown ID results in 404 and an operation that is not active in 409.

### `GET /v1/reactions`, `GET /v1/tracks`, `GET /v1/events` (P5)

The only query parameters accepted are `id`, `id` and `limit` respectively (others result in 400). `id` is an integer of at least 1; if it does not exist, the result is 404. If the operation exists but has no reactions, the result is 200 with an empty array. If the daemon does not support P5 (the Provider does not implement `api.EventProvider`), the result is 501.

`ReactionJSON` (one row = one `ip sla reaction-configuration`):

```json
{"op_id": 13, "element": "timeout", "threshold_type": "consecutive", "upper": 0, "lower": 0, "count": 2,
 "action": "syslog", "occurred": true, "value": 1, "last_change": "2026-09-27T11:59:13Z", "changes": 1}
```

`count` is given only for consecutive / average, `x` / `y` only for xofy. `value` is the last evaluated value (the moving average for average). `last_change` is when `occurred` last changed; it is omitted if it has never changed.

`TrackJSON`:

```json
{"id": 1, "operation": 13, "mode": "reachability", "state": "up", "changes": 2,
 "last_change": "2026-09-27T11:59:13Z", "latest_rc": "ok", "latest_rtt_ms": 0.123, "delay_up": "5s", "delay_down": "0s"}
```

`state` is `up` / `down` / `unknown`. While a delay is running, the state being moved to is in `pending`. `latest_rtt_ms` is given only when `latest_rc` is ok / overThreshold.

`EventJSON` is `event.Event` itself (the same JSON that webhook and exec receive). For the format, see [events.md](events.md).

## Go client

`internal/api.Client` is the Go client of this API.

- The timeout is 5 seconds.
- At most 64 MiB of a response body is read. Beyond that, it returns the error `response from <socket> exceeds 64 MiB; ask for fewer operations or without include` (it does not try to parse truncated JSON). With the settings at their maximum, one detail is about 0.2 to 0.3 MB, so a bulk retrieval with `include=hours,history,enhanced` is good for a few hundred operations.
- When a non-2xx response body is not JSON (something other than goipslad answered), the message is `"<status>: <first 512 bytes of the body>"` on one line.
- When it cannot connect, it returns a one-line error that includes the socket path (for example `cannot connect to /run/goipslad/goipslad.sock: connection refused (is goipslad running?)`).
- HTTP errors are returned as `*api.Error` (with the status and the message). 404 can be checked with `errors.Is` against `api.ErrNotFound`, 409 against `api.ErrNotActive`, and 501 against `api.ErrNotImplemented`.
- `Operations` returns the list of rows; `OperationDetails(ctx, filter, opts)` is the bulk retrieval with `include` (`include=detail` when opts is empty).
- For 400 "invalid configuration", the `Errors` of `*api.Error` hold the individual errors (`"<path>: <message>"`). `Error()` joins them into one line in the same format as `config.ValidationError.Error()` (`invalid configuration: 2 errors: a: x; b: y`). When printing to a terminal, print `Errors` one per line.
