# Threshold reaction and tracking rules

These are the evaluation rules for `react` (Cisco's `ip sla reaction-configuration`) and `tracks` (Cisco's `track <n> ip sla <op>`).
See `docs/config.md` for how to write the configuration and `docs/events.md` for notification delivery. The basis is the "Threshold reactions ..." and "track ip sla ..." sections of the spec research, and the options adopted in section 7 of the implementation plan.

## Threshold reactions

### Overall flow

Each reaction row (one item of `react`) has an "occurred" flag (`occurred`, initially false) and an evaluation window.
Every time an attempt of the operation finishes, all rows are evaluated with its result. When `occurred` changes from false to true, `threshold-exceeded` is emitted once; when it goes back from true to false, `threshold-cleared` is emitted once. If violations continue while the reaction is occurring, no further notifications are sent.

```
value  : 350       400       500       250       150       120       350
state  : occurred  (occ.)    (occ.)    (occ.)    cleared   (clr.)    occurred
notify : exceeded                                cleared             exceeded
```

(Example with `immediate`, upper 300, lower 200. Behaves the same as steps 1-4 in the figure of Cisco's configuration guide.)

### Monitored values

| Element | Value | Attempts evaluated |
|---|---|---|
| `rtt` | RTT truncated to an integer in ms. For icmp-jitter, the average RTT of the whole burst | Only completions (ok / overThreshold). Failed attempts are not evaluated and are not put into the window |
| `timeout` | 1 for timeout, 0 otherwise | All attempts |
| `verifyError` | 1 for verifyError, 0 otherwise | All attempts. icmp-echo only (ICMP Timestamp in icmp-jitter has no data part, so it cannot occur. An intentional deviation from Cisco's table) |
| `jitterAvg` / `jitterSDAvg` / `jitterDSAvg` | Average jitter (ms, truncated) | When there is an icmp-jitter result, at least 1 packet was sent, and there is at least one sample of that jitter |
| `maxOfPositiveSD` and the other 3 | Maximum positive / negative jitter (ms) | Same as above (when there are samples in that direction) |
| `packetLoss` | Number of lost packets | When there is an icmp-jitter result and at least 1 packet was sent (see below) |
| `packetLateArrival` | Number of late packets | Same as above |
| `packetOutOfSequence` | Number of order reversals (sum of source→target, target→source, and both directions) | Same as above |
| `successivePacketLoss` | Maximum length of consecutive losses | Same as above |
| `maxOfLatencySD` / `maxOfLatencyDS` | Maximum one-way delay (ms) | With `one-way-delay: true`, when one-way delay could be aggregated |
| `latencySDAvg` / `latencyDSAvg` | Average one-way delay (ms, truncated) | Same as above |

busy and sequenceError are not attempts, so no element is evaluated for them.

When an icmp-jitter attempt is error (a local failure sent no packets; `sent` is 0), the jitter elements (including the packet counts) are not evaluated. Nothing was measured, so this avoids counting 0 loss as "recovery" and clearing an occurring `packetLoss` reaction or similar. The next burst that actually sends packets is evaluated.

### Violation and recovery

| Element type | Violation (rising) | Recovery (falling) | Neither |
|---|---|---|---|
| Numeric | value > `upper` | value < `lower` | `lower` ≤ value ≤ `upper` |
| Boolean (`timeout`, `verifyError`) | value = 1 | value = 0 | — |

A boundary value counts as neither violation nor recovery (section 7 of the implementation plan). value = `upper` is not a violation, and value = `lower` is not a recovery.

### threshold-type

| threshold-type | Occurrence (false → true) | Recovery (true → false) | Window |
|---|---|---|---|
| `never` | Never | Never | None (values are recorded) |
| `immediate` | On 1 violation | On 1 recovery | None |
| `consecutive` | When violations occur `count` times in a row | When recoveries occur `count` times in a row | Two consecutive counts (violation, recovery). A value that is neither resets both to 0. A transition resets both to 0 |
| `xofy` | `x` or more violations in the last `y` | `x` or more recoveries in the last `y` | Decisions of the last `y`. Not cleared on a transition (section 7 of the implementation plan) |
| `average` | Average of the last `count` > `upper` | Average < `lower` | Values of the last `count`. Not evaluated until `count` values are available. Cannot be used for boolean elements |

- The `xofy` window survives a transition. For example, with x=2, y=3, if the reaction occurs after "violation, recovery, violation" and a recovery then arrives, the window becomes "recovery, violation, recovery", which has 2 recoveries, so it recovers immediately.
- `average` is a moving average (sliding window). It behaves as in Cisco's example (6000, 6000, 5000 against upper 5000 → average 5667, a violation). The event's `value` is the average rounded to an integer (5667 in this example).

### Initialization

In the following cases, `occurred` is reset to false, the window and consecutive counts are emptied, and the recorded value (`value` in `show reactions`, `rttMonReactValue`) is reset to 0. No notification is sent at this time.

- restart / reset in `goipsla`
- When a reload changes the reaction configuration (`react`) of that operation
- When a reload changes measurement-related configuration and the operation is recreated

An operation whose reaction configuration and measurement configuration are both unchanged by a reload keeps its reaction state (the same applies when only tag or owner changes; only the tag of subsequent events changes). This is because initializing would reset an occurring reaction to false without a recovery notification, and the next violation would notify "occurred" again.

The recorded value is the element's value itself (even for `average`, the last value, not the moving average). The moving average goes into the event's `value` on a transition.

### Correspondence with Cisco

| Item | Cisco | goipslad |
|---|---|---|
| Number of notifications | Once per rising → falling transition | Same |
| Threshold boundary | Not documented | Violation on "greater than the upper limit", recovery on "less than the lower limit" |
| `average` window | Not documented | Moving average of the last N |
| Whether the `xofy` window is reset on a transition | Not documented | Not reset |
| `action` | `none` / `trapOnly` / `triggerOnly` / `trapAndTrigger` (syslog depends on the logging configuration) | `none` / `syslog` / `trap` / `trap-and-syslog`. Always written to the log. Selects delivery to syslog and SNMP traps (P7) |
| icmp-jitter `rtt` | RTTAvg (evaluated on the average at the end of the operation) | Same (the burst's average RTT) |
| One-way delay elements | Evaluated when NTP is synchronized | Can be written only when `one-way-delay: true` |
| icmp-jitter `verifyError` | Y in the table | Cannot be written (ICMP Timestamp has no data part, so it cannot occur) |

## Tracking

### Evaluation

Up / down is decided from the return code of the operation's latest attempt using the following table (busy and sequenceError are not attempts and are ignored).

| mode | up | down |
|---|---|---|
| `state` | ok | Everything else (including overThreshold) |
| `reachability` | ok, overThreshold | Everything else |

The same overThreshold is down in `state` and up in `reachability` (the same as Cisco).

### State transitions

- The initial state is `unknown`. The evaluation of the first attempt decides up or down without waiting for the delay, and this transition is also notified.
- After that, the state changes when the evaluation has differed from the current state and the same evaluation continues for `delay.up` (when going up) or `delay.down` (when going down). If the evaluation reverts in the meantime, the state does not change (the same as the delay of Cisco's EOT). If the delay is 0, the state changes immediately.
- The delay is counted from the time of the attempt at which the evaluation first changed. It is not extended no matter how many attempts with the same evaluation arrive in the meantime.
- The state does not change when the operation becomes inactive / pending (the last evaluation is kept).
- Each time the state changes, `track-up` / `track-down` is emitted once.

Example: `delay.down: 5s`, attempts every second, state is up

1. At time 1, timeout → a pending transition toward down starts (deadline at time 6).
2. At time 2, ok → the evaluation is back to up, so the pending transition is cancelled. The state stays up; no notification.
3. From time 3, timeouts continue → the pending transition starts at time 3, the state becomes down at time 8, and `track-down` is emitted once.

### reload

- A track with the same ID, operation, and mode keeps its state (up / down), change count, and in-progress delay. Tracks are used for failover, so this avoids flapping up / down on reload. If the delay length changes, it is recounted with the new length from the time the pending transition started.
- A track whose operation or mode changed, and a new track, start from `unknown`.
- A track whose referenced operation has disappeared is reset to `unknown`. No notification is sent at this time.
- A deleted track disappears without notification.

### Correspondence with Cisco

| Item | Cisco | goipslad |
|---|---|---|
| How the evaluation is taken | The tracker polls every 5 seconds by default (`track timer ip sla`) | Evaluated every time an attempt finishes (no delay from a polling interval) |
| `delay up` / `delay down` | Yes | Same meaning (0..180 seconds) |
| Initial state | Often treated as Down right after configuration | `unknown`. Decided on the first attempt, and notified |
