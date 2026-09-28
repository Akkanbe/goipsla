# Scheduler behavior

`internal/sched` and `internal/manager` decide when each operation runs.
This page describes the scheduler, the state machine with life / ageout / recurring (section 7), and reload (section 8).

## 1. Structure

- All operations are managed in a single min-heap, ordered by next run time. Ties are broken by ascending operation ID.
- Only one timer, created with `clock.Clock.NewTimer`, is used, and it is re-armed to the time at the top of the heap. While waiting, the only running goroutine is the one for the scheduler itself.
- `Add` / `Remove` update the heap in place and wake the scheduler through a channel so that it re-arms the timer. They can be called either before or after `Run`.
- The body of a fired entry (`Entry.Run`) is called in a new goroutine. The icmp-echo body blocks until a reply or a timeout.

## 2. The run-time grid

An entry fires only on the grid `First + k × Frequency` (k = 0, 1, 2, ...). The next time is computed from the grid, not as "previous run time + Frequency", so timer delays and body execution time do not accumulate into drift.

The scheduler passes **the time of the grid point that fired** to the body (`Entry.Run`) and to `Busy`. However, the grid point time is not used for `Start` (the attempt start time) in the result. The manager sets the time at which the attempt actually started (`clk.Now()`) (section 6). Scheduling itself stays on the grid, and even after a late wake-up the next run returns to a grid point.

### When the scheduler wakes up late (skipping grid points)

If the scheduler wakes up one or more periods late because the process was stopped (SIGSTOP, suspend) or overloaded:

- It does **not** catch up by running the missed grid points back to back.
- It runs only once, for the most recent grid point at or before the current time, and the next run is the grid point after that (in the future).
- If any entries were skipped, it logs one warn line per wake-up (`sched: runs missed while the scheduler was late were skipped`, with the count in the `entries` attribute).

The same rule applies if `First` is already in the past at registration (for example, with `First` = 2.5 seconds ago and `Frequency` = 1 second, it runs once right after registration for the grid point 0.5 seconds ago, and then 0.5 seconds later, 1.5 seconds later, ...).

## 3. First start time

| Setting | First run (origin of the grid) |
|---|---|
| No `schedule`, or `start-time: now` | Registration time + `Offset(id, frequency)` |
| `start-time: after <duration>` | Registration time + `after` (the spread offset is not added) |
| `start-time` with a date and time or `HH:MM[:SS]` | `at`. If in the past, the registration time |
| `start-time: pending` | Not registered |
| `restart` | That time (the first run happens immediately) |
| Day 2 and later of `recurring` | Start time + 24 hours × number of days |

### Spread offset

To keep 1,000 targets from sending at the same instant, the first run is shifted within `[0, frequency)`. This corresponds to Cisco's group schedule.

`Offset(id, freq)` hashes the ID as an 8-byte big-endian integer (`uint64(int64(id))`) with FNV-1a (64 bit) and takes the remainder modulo `freq` (in nanoseconds). It depends only on the ID and frequency, so the same operation runs at the same phase across restarts.

The offset is not added to an explicit start time (`after` / `at`). The user's intent in specifying a time takes precedence.

## 4. busy

As defined by Cisco (the "frequency > timeout > threshold ..." section of the spec research), if the next grid point arrives while the previous attempt has not finished, nothing is sent and busy is counted.

- If the entry's previous body has not returned yet, the scheduler calls `Busy(grid point time)` instead of `Run`. `Busy` is called after releasing the lock, so it may call `Add` / `Remove`.
- The manager passes `Result{Code: busy, Seq: 0, Start = End = the time busy was detected (clk.Now())}` to the sink and increments the operation's busy count by 1.
- busy is not an attempt, so it does not consume a Seq (the attempt sequence number).
- The next attempt is made at the next grid point (the MIB's "the next attempt will occur at the next rttMonCtrlAdminFrequency expiration").
- For icmp-echo, configuration validation enforces `frequency > timeout`, so busy does not actually occur. It occurs only with the P3 icmp-jitter or when the engine is abnormally congested.

## 5. Replacement, removal, and stopping

- Registering the same ID with `Add` replaces the old entry. The ctx of the old entry's running body is canceled. The new entry does not inherit the old entry's busy state (it runs normally at the next grid point).
- `Remove` removes the entry and cancels the ctx of its running body.
- `Run(ctx)` returns nil immediately when ctx is done. It does not wait for running bodies to complete (a body's ctx is derived from the ctx of `Run`, so it is canceled together).
- The manager discards, without passing them to the sink, results of attempts that return after the ctx is canceled. An attempt cut short by daemon shutdown does not represent the state of the target.

**Shutdown policy**: `Scheduler.Run` returns without waiting for running bodies (attempt goroutines) to return. This is so that shutdown is not delayed, and cleanup of the bodies is ensured by the following two points. (1) A body's ctx is derived from the ctx of `Run`, so by the time `Run` returns, the ctx of every body is canceled. (2) The manager discards results after the ctx is canceled, and `probe.Engine.Close` releases every waiting `Echo`. Therefore, with goipslad's shutdown order (API → manager → Engine), no attempt goroutine writes to the sink after the Engine is closed. If a guarantee to "wait for all attempts to complete before moving on" is needed in the future (for example, to prevent results of an old operation from mixing into a new statistics row during reload), count and wait for attempts with `sync.WaitGroup` on the manager side, not in the scheduler.

## 6. Result delivery by the manager

- For each attempt, Seq starts at 1 and increases by 1, and the result of `op.Runner.Run(ctx, seq, time the attempt actually started)` is passed to the sink. The start time is `clk.Now()` at the moment the body goroutine starts running, which lags the grid point by the timer delay and the goroutine startup wait. This is used instead of the grid point time so that `Start` is close to the time the Echo Request was actually sent (changed on 2026-09-27).
- When the engine finds a reply after the timeout or a duplicate reply, `Manager.LateReply(opID, seq, sentAt)` is called through `probe.Options.OnLateReply`, and `Result{Code: sequenceError, Seq: seq, Detail: "late or duplicate reply"}` is passed to the sink. `sentAt` is the send time of the request the reply answers. It is delivered only when the operation is active (checked under `sinkMu`; late replies after the life has ended and the operation became inactive are discarded so that the frozen statistics do not change), `seq` is in the recent attempt table, and `sentAt` is at or after the start time of that attempt and at or after the start of the current life; otherwise it is discarded (debug log only). Seq restarts from 1 for each life, so if a restart happens between the probe finding the late reply and the callback, the same `seq` can refer to an attempt of the new life. The send time distinguishes these cases. The check and the delivery are done under `sinkMu` and do not interleave with life changes (`lockLife`).
- The manager sets `Result.Life` (the life number, counted the same way as `LifeIndex` in the statistics: starting at 1, +1 for each new life, back to 1 when recreated after removal) on every result it passes to the sink (attempts, busy, late replies).
- The manager serializes the sink so that it is not called concurrently from multiple operations. Blocking for long inside the sink stalls result delivery for all operations, so do not do heavy work there.

## 7. State machine (P4)

Following `rttMonCtrlOperState`, there are three states: `pending` / `inactive` / `active`. The transitions are collected in `internal/manager/lifecycle.go`.

| Event | Transition and side effects |
|---|---|
| At startup, `schedule` omitted or `start-time: now` | → `active`. The life starts at the scheduled first time (registration time + spread offset) (`Store.Add`) |
| `start-time: after` / date and time / `HH:MM` | `pending`. The statistics row is created in advance with the start time as the life start. At the start time → `active` (using the same life). The spread offset is not added. If the start time is already in the past (and not `recurring`), it becomes `active` immediately |
| `start-time: pending` | Stays `pending`. The statistics row exists, but there are no attempts. It cannot be started with `restart`; it runs if `start-time` is changed by `reload` |
| life reaches 0 (`life` is finite) | `active` → `inactive`. Attempts stop, and statistics (hour groups and so on) are frozen and kept in `Store`. Late and duplicate replies arriving after the end are not counted either (§6). The attempt at a grid point exactly at the end of the life is not made |
| `recurring: true` | Every day at `start-time` (`HH:MM[:SS]`) → `active`, and when the life expires → `inactive`. From day 2 on, each start begins a new life (`Store.Add`; the history life index advances). Seq starts from 1 for each life |
| ageout | Decreases only while not `active` (`pending` and `inactive`). It stops when the operation becomes `active`, and counts again from the beginning the next time it stops being `active`. When it reaches 0, the operation is removed from the manager and `Store` |
| `restart <id>` | Possible only when `active`. `Store.Reset(id, now)` discards the statistics and starts a new life from now, resets life to the configured value, resets Seq to 1, and makes the first attempt immediately (after that, the grid starts from that time). Rejected when not `active` |
| `reset` | Stops the schedules of all operations and redoes the same initial transitions as at startup from the current configuration. All statistics begin a new life (operations removed by ageout are also recreated) |

Time-driven transitions (start, end of life, ageout) are registered in a `sched.Scheduler` separate from the one for attempts (keyed by `ID × 3 + kind`). Instead of a 1-second timer per operation, a single timer waits for the deadline times. "Operation time to live" is computed by subtracting the current time from the deadline (`life_left_s` in the API; seconds are rounded up).

The API list (`OperationRow`) includes the following.

| Field | Content |
|---|---|
| `life_left_s` | Remaining seconds of the life. 0 when `inactive` / `pending`. Omitted for `life: forever` |
| `next_start` | When `pending` / `inactive` and the next start time is known (`after`, a date and time, the next day of `recurring`) |
| `ageout_left_s` | Remaining seconds while ageout is counting |

icmp-echo runs with the Runner from `op.NewEcho`, and icmp-jitter with the Runner from `op.NewJitter` (P3); the state machine is the same regardless of type. If there is a type that the configuration accepts but that has no Runner, it stays `inactive` and makes no attempts (warn log `<type> is not implemented yet; skipped`).

### Deviations from Cisco

| Item | Cisco | goipslad | Reason |
|---|---|---|---|
| Operations removed by ageout | Removed along with their configuration | Removed from the manager and statistics, but they remain in the configuration file, so they are recreated by the next `reload` (or `reset`) | The configuration file is the source of truth. They cannot be kept "removed" without editing the file |
| `reset` | `ip sla reset` also deletes the configuration of all operations and does not reread the startup-config | Resets statistics and schedules but does not delete the configuration (it keeps the last loaded configuration file) | Same as above |
| Starting from `start-time pending` | Started by a trigger (the reaction's `trigger`) or by redoing `ip sla schedule` | Triggers are not implemented. `restart` cannot start it either. Change `start-time` with `reload` | Because triggers are not implemented |
| `recurring` period | The same time every day | Every 24 hours from the start time (elapsed time). In regions with daylight saving time changes, the start after a change shifts by 1 hour | To keep the implementation simple. No effect in Japan Standard Time |
| State after the life expires | The MIB says `inactive`; the CLI description of `lives-kept` says `pending` (the "Schedules ..." section of the spec research) | `inactive` | Matches the MIB transition table |

## 8. Reload (P4)

The entry points are SIGHUP and `POST /v1/reload` (`goipsla reload`), and both go through the same function (serialized with a mutex). It rereads and validates the configuration file; if that fails, nothing is applied and the daemon keeps running with the old configuration (it returns all errors and also logs them).

The comparison is made per ID between the currently running operations and the new configuration. An operation removed by ageout is "not running", so if it remains in the file it becomes `added`.

| Class | Condition | Action |
|---|---|---|
| `added` | New ID | Created with the same initial transitions as at startup |
| `removed` | ID removed from the configuration | Stopped, and its statistics are also removed (`Store.Remove`) |
| `restarted` | A measurement-related item changed: `type`, `target`, `source-ip`, `source-interface`, `vrf`, `tos`, `traffic-class`, `flow-label`, `frequency`, `timeout`, `threshold`, `request-data-size`, `data-pattern`, `verify-data`, `interval`, `num-packets`, statistics, history, and enhanced history settings, `schedule` | Destroyed and recreated; statistics start from a new life (in Cisco, a scheduled operation cannot be changed unless it is deleted and recreated) |
| `updated` | Only `tag`, `owner`, `react` changed | Only the configuration is replaced, without stopping measurement or statistics. If `react` changed, the reaction evaluation state of that operation is reset; if only `tag` or `owner` changed, it is kept (docs/reactions.md, "Initialization") |
| (none) | No change | Nothing is done |

- Which items changed is written to a debug log (`reload: measurement settings changed`, attribute `fields`).
- `start-time: HH:MM` is resolved at every load to "the next time that time of day occurs". Therefore, if you reload after the start time has passed, the resolved result falls on the next day or later even if the file is unchanged. So if the old configuration's start time has passed, the new configuration's start time has not yet come, and both have the same hours, minutes, and seconds in local time, it is treated as "no change". `after` is compared as durations.
- Changes to `global` (`api-socket`, `api-socket-group`, `metrics-listen`, `log`, `syslog`, `snmp`) and to `actions` are not applied. A warn log and `ReloadResult.warnings` report "requires a restart".
