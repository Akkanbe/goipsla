# Prometheus metrics reference

`goipslad` listens for HTTP on `global.metrics-listen` in the configuration (default `127.0.0.1:9818`) and serves metrics in the Prometheus format on `/metrics`.

- Writing `metrics-listen: ""` disables the exporter.
- The only path served is `/metrics`; any other path returns 404.
- The standard Go runtime (`go_*`) and process (`process_*`) metrics are exported as well.
- Values are rebuilt from the statistics at every scrape. Therefore operations added or removed by a reload appear from the next scrape.
- The `metrics-listen` address does not change on reload. A change takes effect after the daemon restarts.

## Labels

Per-operation metrics share the following labels.

| Label | Content | Example |
|---|---|---|
| `id` | Operation ID | `101` |
| `type` | `icmp-echo` / `icmp-jitter` | `icmp-echo` |
| `target` | Target address | `10.100.1.11`, `fd00:100:2::11` |
| `tag` | `tag` from the configuration (empty if none) | `wan` |
| `vrf` | `vrf` from the configuration (empty if none) | `blue` |

The number of series grows with the number of operations. There are at most 19 per operation (including the 7 of `results_total`); with 1,000 operations, goipsla accounts for about 19,000 series and a body of about 2 MB (Prometheus fetches it with gzip, so less is transferred).

## Metrics

| Name | Type | Extra labels | Content |
|---|---|---|---|
| `goipsla_operation_info` | gauge | `state` | Always 1. For looking up the state (`pending` / `inactive` / `active`) by label |
| `goipsla_operation_state` | gauge | | The state as a number: pending 0, inactive 1, active 2 |
| `goipsla_latest_rtt_seconds` | gauge | | RTT of the latest attempt (seconds, a fraction with nanosecond precision). Only when the latest result is ok / overThreshold |
| `goipsla_latest_return_code` | gauge | `rc` | The latest return code as a number (RttResponseSense: ok 1, overThreshold 3, timeout 4, busy 5, dropped 7, sequenceError 8, verifyError 9, error 16). `rc` is the MIB name. Not exported before the first attempt |
| `goipsla_latest_success` | gauge | | 1 if the latest result is ok, 0 otherwise. Not exported before the first attempt |
| `goipsla_latest_end_timestamp_seconds` | gauge | | Completion time of the latest attempt (Unix seconds). Not exported before the first attempt |
| `goipsla_attempts_total` | counter | | Number of attempts (Initiations) |
| `goipsla_completions_total` | counter | | Number of completions (Completions, including overThreshold) |
| `goipsla_results_total` | counter | `result` | Counts per result. See below |
| `goipsla_rtt_sum_seconds_total` | counter | | Sum of the RTTs of completions (seconds): the MIB's RTTSum, the sum of each RTT truncated to ms, divided by 1000 |
| `goipsla_rtt_min_seconds` | gauge | | Minimum RTT of completions in the current life (seconds, truncated to ms). Not exported when there are no completions |
| `goipsla_rtt_max_seconds` | gauge | | Likewise, the maximum |
| `goipsla_life_start_timestamp_seconds` | gauge | | Start time of the current life (Unix seconds) |
| `goipsla_operations` | gauge | `state` only | Number of operations per state. All 3 states are always exported, even at 0 |
| `goipsla_scrape_duration_seconds` | gauge | None | Time taken to collect the goipsla metrics |
| `goipsla_jitter_avg_seconds` | gauge | `direction` | icmp-jitter: Mean jitter of the latest burst (seconds; the mean of integer ms divided by 1000). `direction` is `sd`, `ds` or `both` (every sample of both directions) |
| `goipsla_jitter_packet_loss_total` | counter | | icmp-jitter: Packets lost in the current life |
| `goipsla_jitter_packets_late_total` | counter | | icmp-jitter: Late packets in the current life (replies that arrived after the timeout but before the end of the burst) |
| `goipsla_jitter_packets_out_of_sequence_total` | counter | `direction` | icmp-jitter: Reorderings in the current life. `sd` (R only), `ds` (arrival order only) and `both` (both) do not overlap |
| `goipsla_jitter_packets_skipped_total` | counter | | icmp-jitter: Packets not sent in the current life |
| `goipsla_jitter_rtt_over_threshold_total` | counter | | icmp-jitter: Packets whose RTT exceeded the threshold in the current life |

The `result` label of `goipsla_results_total` always has the following 7 values (even at 0).

| `result` | What it counts |
|---|---|
| `ok` | Successes (Completions − OverThresholds) |
| `overThreshold` | Completions over the threshold |
| `timeout` | Timeouts (including those that received a Destination Unreachable) |
| `busy` | Attempts skipped because the previous one had not finished |
| `dropped` | Attempts that could not be sent (Drops) |
| `sequenceError` | Replies that arrived late, and duplicate replies |
| `verifyError` | Replies whose data part did not match |

`ok + overThreshold + timeout + dropped + verifyError` roughly equals `goipsla_attempts_total`. Internal errors (`error`) are included in dropped. busy and sequenceError are not attempts, so they are not in attempts (see "Accounting model" in `docs/statistics.md`).

### Counter resets

The counters are totals over the current life of the operation. When a new life starts, they go back to 0.

- A new life starts on a `goipsla` restart / reset, a reload that changed a measurement setting, the restart of a life by the schedule, and a daemon restart.
- Prometheus treats this as a counter reset. `rate()` and `increase()` correct for resets, so they can be used as they are. This is intended.
- On the other hand, expressions that subtract raw values (`x - x offset 1h`) can go negative across a reset, so do not use them.

### icmp-jitter

- `goipsla_jitter_*` is exported only for icmp-jitter operations (not for icmp-echo). For a jitter operation whose life has started but that has not run a burst yet, the counters (such as `goipsla_jitter_packet_loss_total`) are exported at 0, and only `goipsla_jitter_avg_seconds` is missing.
- `goipsla_jitter_avg_seconds` is the value of the latest burst (a gauge); the others are totals over the current life (counters, back to 0 when a new life starts).
- The common metrics (`goipsla_attempts_total`, `goipsla_results_total`, `goipsla_latest_rtt_seconds` and so on) count **per burst** for jitter. `goipsla_latest_rtt_seconds` is the mean RTT of the latest burst.
- Example of a loss rate: `rate(goipsla_jitter_packet_loss_total[5m])`. The total number of packets is not in the metrics, so to get a ratio, divide by `num-packets` × the number of bursts (`goipsla_attempts_total`).
- One-way delays are not made into metrics in P3b even with the `one-way-delay` setting enabled (they can be seen in the API and the CLI).

### Diagnostics (engine and statistics)

No operation labels. For troubleshooting in operation; totals since the daemon started (back to 0 on restart).

| Name | Type | Labels | Content |
|---|---|---|---|
| `goipsla_probe_packets_dropped_total` | counter | `reason` | Packets the ICMP engine dropped. `foreign` (not a reply to our requests; a raw socket receives every Echo Reply on the host, so replies to other processes' pings are counted here too), `malformed` (unparsable, bad checksum), `other` (other ICMP types that passed the filter) |
| `goipsla_probe_late_replies_total` | counter | None | Replies that arrived after the timeout, and duplicate replies (what the operations' sequenceErrors come from, including those of earlier lives) |
| `goipsla_probe_receive_errors_total` | counter | None | Failed reads from the raw sockets. If it keeps growing, replies are being lost (a warning also appears in the log) |
| `goipsla_results_discarded_total` | counter | None | Results discarded as belonging to an earlier life (attempts that spanned a restart / reset / reload). See `docs/statistics.md` |

### Tracking, reactions and events (P5)

These have no operation labels (`id`, `type`, `target`, `tag`, `vrf`), only the following labels.

| Name | Type | Labels | Content |
|---|---|---|---|
| `goipsla_track_state` | gauge | `track`, `op`, `mode` | Track state: up 1, down 0, unknown −1 (before the first attempt, or when the referenced operation does not exist) |
| `goipsla_reaction_occurred` | gauge | `id`, `element` | 1 while the reaction row is in the threshold violation state (occurred), 0 otherwise |
| `goipsla_events_total` | counter | `kind`, `sink`, `result` | Delivery results per sink. `result` is `delivered` (the sink accepted it, including events the sink's conditions did not send), `failed`, or `dropped` (the sink's queue overflowed). The Bus does not tell kinds apart for `dropped`, so `kind` is empty. Overflows of the Bus's own queue are `sink="bus"`. `sink` is `log`, `syslog`, `snmp-trap`, `webhook#<N> <scheme>://<host>` or `exec#<N> <path>` (`N` is the position in `actions`, 1-based). A webhook shows only the host (and port); path, userinfo, query and fragment are all redacted, because some services use the path itself as the token, as in `/services/T000/B000/XXXX` (for example `webhook#3 https://hooks.example:8443`). Webhooks on the same host are told apart by `N` |

Example:

```
goipsla_track_state{mode="reachability",op="13",track="1"} 0
goipsla_reaction_occurred{element="timeout",id="13"} 1
goipsla_events_total{kind="track-down",result="delivered",sink="exec#1 /work/staging/scripts/hook.sh"} 1
```

## Example output

```
# HELP goipsla_latest_rtt_seconds Round-trip time of the latest attempt, when it completed (ok or overThreshold).
# TYPE goipsla_latest_rtt_seconds gauge
goipsla_latest_rtt_seconds{id="101",tag="wan",target="10.100.1.11",type="icmp-echo",vrf="blue"} 0.001234567
# HELP goipsla_latest_return_code Return code of the latest attempt (RttResponseSense number); the rc label is its MIB name.
# TYPE goipsla_latest_return_code gauge
goipsla_latest_return_code{id="101",rc="ok",tag="wan",target="10.100.1.11",type="icmp-echo",vrf="blue"} 1
goipsla_latest_return_code{id="102",rc="timeout",tag="wan",target="fd00:100:2::11",type="icmp-echo",vrf=""} 4
# HELP goipsla_results_total Attempts by result in the current life; ok counts successes (completions without overThreshold).
# TYPE goipsla_results_total counter
goipsla_results_total{id="101",result="ok",tag="wan",target="10.100.1.11",type="icmp-echo",vrf="blue"} 115
goipsla_results_total{id="101",result="overThreshold",tag="wan",target="10.100.1.11",type="icmp-echo",vrf="blue"} 3
goipsla_results_total{id="101",result="timeout",tag="wan",target="10.100.1.11",type="icmp-echo",vrf="blue"} 2
...
# HELP goipsla_operations Number of operations by state.
# TYPE goipsla_operations gauge
goipsla_operations{state="active"} 2
goipsla_operations{state="inactive"} 0
goipsla_operations{state="pending"} 1
```

## Example Prometheus configuration

```yaml
scrape_configs:
  - job_name: goipsla
    scrape_interval: 15s
    static_configs:
      - targets: ["probe-host:9818"]
```

## PromQL examples (for Grafana)

**1. Latest RTT per target (ms).** An example narrowed by tag.

```promql
goipsla_latest_rtt_seconds{tag="wan"} * 1000
```

`{{target}} ({{id}})` works well as the legend. The series of a target in timeout breaks off, so a gap shows that it is unreachable.

**2. Success rate over the last 5 minutes (%).** Per target.

```promql
100 * increase(goipsla_results_total{result="ok"}[5m])
  / ignoring(result) increase(goipsla_attempts_total[5m])
```

To count overThreshold as success, use `increase(goipsla_completions_total[5m])` as the numerator.

**3. Mean RTT over the last 5 minutes (ms), and the list of failing targets.**

```promql
1000 * increase(goipsla_rtt_sum_seconds_total[5m]) / increase(goipsla_completions_total[5m])
```

```promql
goipsla_latest_success == 0
```

The latter suits alerts and table panels. To show the name of the latest return code too, attach the `rc` label of `goipsla_latest_return_code` with `group_left`.

```promql
(goipsla_latest_success == 0)
  * on(id) group_left(rc) (goipsla_latest_return_code * 0 + 1)
```

## Performance

Every scrape reads the statistics of every operation. The unit benchmark (`go test -bench Scrape1000 ./internal/metrics`) measures it; the target for 1,000 operations is under 100 ms. Measurements in the staging environment are taken with `staging/scripts/p6-verify.sh`.
