# Events reference

Every time the state of a threshold reaction (`react`) or of tracking (`tracks`) changes, `goipslad` creates one event and sends it to the registered delivery targets (sinks).
Events are produced only when the state changes; a state that persists is not notified again (as in Cisco).
For the evaluation rules, see `docs/reactions.md`.

## Kinds of events

| `kind` | When it is produced |
|---|---|
| `threshold-exceeded` | A reaction changes from "not occurred" to "occurred" (rising) |
| `threshold-cleared` | A reaction returns from "occurred" to "not occurred" (falling) |
| `track-up` | A track becomes up (including the first transition from unknown to up) |
| `track-down` | A track becomes down (including the first transition from unknown to down) |

Reactions for `timeout` and `verifyError` are also produced as `threshold-exceeded` / `threshold-cleared` (with `element` set to `timeout` and so on).
When reset, restart or reload initializes the state of a reaction, no event is produced (the same as "resolution notifications will not occur" in the MIB).

## JSON

This is the format of the webhook body and of what exec receives on standard input. Field names are in snake case, and empty optional fields are omitted.

Example of a threshold reaction:

```json
{
  "time": "2026-09-27T12:00:05.123456789Z",
  "kind": "threshold-exceeded",
  "op_id": 101,
  "type": "icmp-echo",
  "target": "10.100.1.11",
  "tag": "wan",
  "vrf": "blue",
  "element": "rtt",
  "threshold_type": "consecutive",
  "value": 350,
  "upper": 300,
  "lower": 200,
  "action": "trap",
  "reaction_index": 1,
  "message": "%RTT-3-IPSLATHRESHOLD: IP SLAs(101): Threshold exceeded for rtt (value 350, rising 300, falling 200)"
}
```

Example of tracking:

```json
{
  "time": "2026-09-27T12:01:00Z",
  "kind": "track-down",
  "op_id": 101,
  "type": "icmp-echo",
  "target": "10.100.1.11",
  "tag": "wan",
  "track_id": 1,
  "track_mode": "reachability",
  "track_state": "down",
  "latest_rc": "timeout",
  "message": "%TRACK-6-STATE: 1 ip sla 101 reachability Up -> Down"
}
```

| Field | Description |
|---|---|
| `time` | When the state changed (RFC 3339) |
| `kind` | One of the 4 kinds above |
| `op_id`, `type`, `target`, `tag`, `vrf` | The operation concerned |
| `element`, `threshold_type` | The reaction's monitored element and `threshold-type` (threshold-* only) |
| `value` | The value that decided the transition (`rttMonReactValue`). For `average`, the moving average rounded to the nearest integer. For boolean elements, 1 (occurred) or 0 (recovered) |
| `upper`, `lower` | The reaction's thresholds (omitted for boolean elements) |
| `action` | The reaction's `action` (`none` / `syslog` / `trap` / `trap-and-syslog`) |
| `reaction_index` | Number of the reaction row (which line of the operation's `react`, 1-based). Fixed when the event is created, and the same number as SNMP `rttMonReactConfigIndex`. It does not change if a reload reorders the rows before delivery (threshold-* only) |
| `track_id`, `track_mode`, `track_state` | Track ID, `state` / `reachability`, and the new state (track-* only) |
| `latest_rc`, `latest_rtt_ms` | The latest return code (MIB name) and RTT used for the decision. The RTT is given only for ok / overThreshold |
| `message` | A human-readable line. This text is also sent to syslog |

`value`, `upper` and `lower` are omitted from the JSON when their value is 0 (including `value: 0` and `lower: 0` of numeric elements). Receivers should read a missing key as 0. Whether an element is boolean is told by `element` (`timeout`, `verifyError`).

## Message format

This is our own format, modeled on Cisco's `%RTT-` / `%TRACK-` (Cisco does not publish the exact format).

```
%RTT-3-IPSLATHRESHOLD: IP SLAs(<id>): Threshold exceeded for <element> (value <v>, rising <upper>, falling <lower>)
%RTT-3-IPSLATHRESHOLD: IP SLAs(<id>): Threshold below for <element> (value <v>, rising <upper>, falling <lower>)
%RTT-4-OPER_TIMEOUT: IP SLAs(<id>): Threshold exceeded for timeout
%RTT-4-OPER_TIMEOUT: IP SLAs(<id>): Threshold below for timeout
%RTT-3-IPSLATHRESHOLD: IP SLAs(<id>): Threshold exceeded for verifyError
%TRACK-6-STATE: <track-id> ip sla <op-id> <mode> <Old> -> <New>
```

- Boolean elements (`timeout`, `verifyError`) get no value or thresholds. Only `timeout` uses `%RTT-4-OPER_TIMEOUT`, following Cisco.
- Track states are capitalized: `Unknown`, `Up`, `Down`.

## Delivery targets (sinks)

| Sink | Registered when | Events sent |
|---|---|---|
| Log | Always | All |
| syslog | `global.syslog` is written | All track-* events. threshold-* events only for reactions whose `action` is `syslog` or `trap-and-syslog` |
| Webhook | One per `webhook` in `actions[]` | Those that match the item's `on` and `track` |
| exec | One per `exec` in `actions[]` | Those that match the item's `on` and `track` |
| SNMP trap | `global.snmp.traps` is written (`docs/snmp.md`) | threshold-* events whose `action` is `trap` / `trap-and-syslog` |

The sink names (the `sink` attribute of the log, and `goipsla_events_total{sink=...}` in the metrics) are `log`, `syslog`, `snmp-trap`, `webhook#<N> <scheme>://<host>` and `exec#<N> <path>` (for example `webhook#2 https://hooks.example`). `N` is the position in `actions` (1-based), so writing the same URL or program in two items does not produce the same name. Everything after the host of a webhook URL (path, userinfo, query, fragment) appears neither in the name nor in error messages nor in the log, because the path itself can be a token, as in `https://hooks.example/services/TOKEN`.

`actions[].track` narrows only track-* events. threshold-* events do not belong to a track, so writing `track` does not narrow them (they are selected by `on` alone).

### Log

Logged at info as the message `event`. The attribute names are the same as the JSON field names, except for the following two. With `global.log.format: json`, each is one line of JSON.

- `op_id` becomes `op` (the logging convention: the operation ID is `op`).
- `time` becomes `event_time` (so as not to clash with the log line's own `time`).

### syslog

- Sent to the local syslog (`/dev/log`) with the facility `global.syslog.facility`, severity info and tag `goipslad`. The body is `message`.
- If it cannot connect at startup, startup is not stopped (the warning `syslog not reachable; will retry on the next event`, with `facility` and `err`). It reconnects on the next event. If sending fails, it reconnects on the next event.

### Webhook

- `POST <webhook>`, `Content-Type: application/json`, `User-Agent: goipslad`; the body is the event JSON.
- Each request times out after 10 seconds. A non-2xx response or a communication error is a failure, retried up to 3 times at intervals of 1, 4 and 16 seconds (4 tries in total). If every try fails, it is counted as a failure and a warning is logged.
- https certificates are verified as usual.
- Redirects (3xx) are not followed and are treated as failures (following one would turn the POST into a GET without a body, and it would be counted as a success although nothing was delivered).
- While retrying, the next event for the same webhook waits (order is kept within one sink). Other sinks are not affected.

### exec

- The program written in `exec` is run directly, without a shell. The working directory is `/`.
- The event JSON is passed on standard input as one line.
- Environment variables (in addition to the daemon's environment):

| Variable | Value |
|---|---|
| `GOIPSLA_EVENT_KIND` | `threshold-exceeded` and so on |
| `GOIPSLA_OP_ID` | Operation ID |
| `GOIPSLA_TARGET` | Target |
| `GOIPSLA_TAG` | Tag (empty if none) |
| `GOIPSLA_ELEMENT` | Reaction element (empty for track-*) |
| `GOIPSLA_VALUE` | `value` (0 if none) |
| `GOIPSLA_TRACK_ID` | Track ID (0 for threshold-*) |
| `GOIPSLA_TRACK_STATE` | `up` / `down` (empty for threshold-*) |

- The program runs in its own process group. If it does not finish within 30 seconds, the whole group is killed, including the child processes the program started. Child processes left behind after the program exits (such as ones started with `&`) are killed too. Move work that should keep running to another group, with `setsid` or similar.
- A non-zero exit status is counted as a failure (no retry). The `err` of the failure warning carries the first 256 bytes of standard error on one line (for example `exec#1 /usr/local/libexec/ipsla-notify.sh: exit status 3: stderr: cannot reach pager`).
- Only the first 4 KB of standard output and standard error go to the debug log.
- At most 4 programs run at the same time, across all exec sinks. The rest wait for a free slot.

Example script:

```sh
#!/bin/sh
# /usr/local/libexec/ipsla-notify.sh
logger -t ipsla "$GOIPSLA_EVENT_KIND op=$GOIPSLA_OP_ID target=$GOIPSLA_TARGET track=$GOIPSLA_TRACK_ID $GOIPSLA_TRACK_STATE"
```

## How delivery works, and lost events

- An event first enters a shared queue (10,000 events) and is dispatched from there to a queue per sink (1,000 events). One goroutine per sink delivers the events one at a time, in order.
- A slow sink (such as an unresponsive webhook) only clogs its own queue and does not affect the other sinks.
- An event that overflows a queue is dropped, and a warning is logged and counted. Measurement does not stop.
- Warnings for delivery failures and queue overflows are throttled to at most one per minute per sink, so that a dead syslog or webhook receiver does not produce one warning per event. The number throttled appears in the `suppressed` attribute of the next warning. The counts are always accurate in `Stats()` (failed / dropped per sink).
- The delivery failure warning `event delivery failed` has `sink`, `kind`, `op`, `target`, `element` (threshold-*) or `track_id` (track-*), `err` and `suppressed`.
- When a sink that kept failing delivers again, the info message `event delivery recovered` (`sink`, the number of failures so far `failed`, and the number throttled `suppressed`) is logged once. The next failure is logged at once, without throttling.
- At daemon shutdown, the info message `event sink totals` (`delivered`, `failed`, `dropped`) is logged for each sink that had failures or drops.
- Failures per SNMP trap destination are kept at the debug message `snmp trap failed`; the warning is consolidated into `event delivery failed` above (with the errors of each destination joined).
- The latest 1,000 events are kept in memory whether or not they were delivered, and can be seen with `GET /v1/events` and `goipsla show events` (P5b).
- Events still in the queues when the daemon stops are not delivered.
