# Statistics (`internal/stats`)

`internal/stats` accounts for the attempt results of operations (`op.Result`) in the same form as CISCO-RTTMON-MIB.
The accounting model and the `internal/stats` types are described in this document. The basis is the "Return codes are ..." section and the "Statistics are kept in three layers ..." section of `reports/cisco-ip-sla-icmp-spec-research.md`.

Statistics consist of the following five parts.

| Layer | Type | MIB | CLI |
|---|---|---|---|
| Latest result | `Latest` | `rttMonLatestRttOper*` | `show ip sla statistics` |
| Cumulative over the whole life | `Snapshot.Totals` / `SummaryRow.Totals` (`Counters`) | `rttMonStatsTotals*`, and the sum of `rttMonStatsCapture*` / `rttMonStatsCollect*` over the whole life | Number of successes / failures in `show ip sla statistics` |
| Hourly aggregation and distribution | `HourGroup`, `DistBucket` | `rttMonStatsCaptureTable`, `rttMonStatsCollectTable` | `show ip sla statistics aggregated [details]` |
| Snapshot history | `HistoryBucket` | `rttMonHistoryCollectionTable` | `show ip sla history` |
| Enhanced history | `EnhancedBucket` | — (CLI only) | equivalent of `show ip sla history enhanced` |

## Accounting model

The MIB-style counters are authoritative. A single result increments the following counters according to its return code. This accounting is applied in the same way to `Totals`, to the hour group of the result's time, and to the enhanced history bucket of the result's time.

| `op.Result.Code` | Initiations | Completions | OverThresholds | Timeouts | Busies | Drops | SequenceErrors | VerifyErrors | RTT accumulation | Distribution | Latest | History |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `ok` | +1 | +1 | | | | | | | yes | yes | updated | attempt |
| `overThreshold` | +1 | +1 | +1 | | | | | | yes | yes | updated | attempt |
| `timeout` (including unreachable) | +1 | | | +1 | | | | | | | updated | attempt |
| `verifyError` | +1 | | | | | | | +1 | | | updated | attempt |
| `dropped`, `error` | +1 | | | | | +1 | | | | | updated | attempt |
| `busy` | | | | | +1 | | | | | | not updated | not recorded |
| `sequenceError` (late or duplicate) | | | | | | | +1 | | | | not updated | not recorded |
| Other (`other`, `disconnected`, `notConnected`, `applicationSpecific`) | +1 | | | | | | | | | | updated | attempt |

- **Completions include overThreshold**. The MIB defines `rttMonStatsCaptureOverThresholds` as "This number is a subset of the accumulation of all rttMonStatsCaptureCompletions. The operation time of these completed operations will be accumulated".
- **CLI Number of successes = Completions − OverThresholds** (the number of return code ok), and **Number of failures = Initiations − successes**. In the CLI, overThreshold counts as a failure (consistent with the real device output "Latest operation return code: Over threshold / Number of successes: 0 / Number of failures: 6"). `Counters.Successes()` and `Counters.Failures()` compute these.
- **busy and sequenceError are not attempts**. busy means "the operation did not occur", and sequenceError is a late or duplicate reply to a previous attempt, so they are not counted in Initiations and update neither Latest nor history.
- Return codes that should not come from ICMP operations (the last row of the table) increment only Initiations. As a result, they are counted as failures.
- **RTT resolution**: In statistics and history, RTT is accumulated as an integer truncated to milliseconds (0.9 ms → 0, 1.9 ms → 1). The accumulators are Sum, Sum² (`uint64`; corresponds to the MIB's `SumCompletionTime2Low/High`), Min, and Max. If Completions is 0, Min / Max are 0. `Latest.RTT` is kept in nanoseconds.
- **Average and standard deviation**: `AvgMs = Sum / N`, `StdDevMs = sqrt(Sum²/N − (Sum/N)²)`. If N = 0, both are 0. If rounding error makes the variance negative, it is set to 0.
- **Latest**: `Valid`, `Seq`, `Start`, `End`, `Code`, and `Detail` are replaced on every attempt. `RTT` is set only for ok / overThreshold, and is 0 otherwise. Before the first attempt, `Valid = false` and `Code = other`.

## Time reference

- The hour group and the enhanced history bucket are chosen by **`Result.Start` (attempt start time)**. `Result.End` is used only for `Latest.End`.
- For a result whose `Result.Start` is the zero value (for example, when a late sequenceError is reported without an occurrence time), the current time of the `Store`'s clock is used.
- **Results from a previous life are discarded**: A result whose `Result.Start` is before the start time of the current life belongs to a previous life (an attempt that was in progress across a `Reset` or `Add`, or a late or duplicate reply to one). These are not counted anywhere in the new life (none of Latest, Totals, hour groups, history, enhanced history, or the jitter accumulators change). The number discarded is counted by `Store.Discarded()` (the total over all operations; for diagnostics). A result whose `Start` is exactly the life start time goes into the new life.
- A result whose `Start` is the zero value is not discarded and is handled with the clock's current time. If the current time is before the start of the life, it goes into the first period (Index 1).

## Hourly aggregation (hour group)

- Periods are cut **every 60 minutes** from the life start time (not aligned to the top of the wall clock hour). Period k (1-based) is `[LifeStart + (k−1)h, LifeStart + k·h)`, with `HourGroup.Index = k` and `HourGroup.Start = LifeStart + (k−1)h`.
- Switching groups is decided at `Record` time. There is no background timer.
- If many hours pass without any result, **empty groups are not created**. A group is created for the time of the new result, and the Index advances by the elapsed time (for example, if the first result arrives 2.5 hours after the life starts, Index 3).
- `HoursKept` groups are kept, and the oldest is discarded when exceeded. The Index increases monotonically, and numbers are not compacted when groups are discarded (the same as the MIB's handling of Start Time Index). If `HoursKept = 0`, nothing is collected (MIB: "When this object is set to the value of zero all rttMonStatsCaptureTable data capturing will be shut off"). `Totals` accumulates over the whole life regardless of `HoursKept`.
- A result with a time older than the current group (when results arrive out of order) is added to that group if it is still kept. If its time falls in a discarded group, it is added only to `Totals`.

### Distribution buckets

- `DistBuckets` buckets of width `DistInterval` (ms, integer). Bucket i (0-based) is `[i × DistInterval, (i+1) × DistInterval)`, and the last bucket has no upper bound (`UpperMs = 0`). The bucket is chosen by `i = min(floor(rttMs / DistIntervalMs), DistBuckets − 1)`.
- Example: with 5 × 10 ms, 0–9, 10–19, 20–29, 30–39, 40–∞ ms (the Command Reference example in the spec research).
- If `DistBuckets = 1`, the width is not used and everything goes into a single bucket (0–∞) (MIB: "the value of rttMonStatisticsAdminDistInterval does not apply when rttMonStatisticsAdminNumDistBuckets is one").
- Only Completions (including overThreshold) go into the distribution. Each bucket has Completions, OverThresholds, Sum, Sum², Min, and Max.
- **OverThresholds per bucket**: the number of completions in that bucket that were overThreshold (the MIB's `rttMonStatsCaptureOverThresholds`; a column held per distribution bucket row). It is a subset of Completions, and the sum over all buckets of an hour group equals that hour group's `Counters.OverThresholds`.

## Snapshot history

- The structure is lives × buckets × samples. For echo, 1 attempt = 1 bucket = 1 sample (`Sample` is always 1).
- **If `Lives = 0`, nothing is collected** (stopped before the attempt). **`Filter` is evaluated after the attempt**.
  - `none`: not recorded
  - `all`: all attempts
  - `overThreshold`: `overThreshold` only
  - `failures`: anything other than Completions (timeout / verifyError / dropped / error, and so on)
  - busy and sequenceError are not attempts, so they are not recorded with any filter.
- **The bucket index is the sequence number of the attempt within the life** (1-based). The MIB's `rttMonHistoryCollectionBucketIndex` says "this object increments on each operation attempt", so attempts not recorded because of the filter also use up a number. With the `failures` or `overThreshold` filter, numbers skip.
- In each life, the latest `Buckets` buckets are kept, and the oldest is discarded when exceeded. The index keeps advancing (as in the MIB).
- `RTTMs` is set only for `ok`, and is 0 otherwise (MIB: "If the RTT operation fails (rttMonHistoryCollectionSense is any value other than ok), this has a value of 0"). Therefore overThreshold buckets also have `RTTMs = 0`.
- `Start` is the attempt start time, and `Target` is the configured target (`cfg.Target`).
- A new life starts on each `Add` and `Reset` (restart). The life index increases monotonically from 1, and `Lives` lives (including the current one) are kept. Buckets of previous lives remain as they are.
- `Snapshot.History` lists the buckets of all lives in oldest-first order (by life, then by bucket).

## Enhanced history

- When `Enhanced != nil`, buckets of `Interval` each from the life start time hold the same accumulators as the hour group (`Counters`; no distribution).
- How buckets are created, how the Index is counted (1-based, monotonically increasing, no empty buckets), and how out-of-order results are handled are the same as for hour groups.
- The latest `Buckets` buckets are kept; when exceeded, the oldest is discarded and collection continues.
- When disabled, `Snapshot.Enhanced` is nil even if `SnapshotOptions.Enhanced` is specified.

## Life, reset, and restart

- `Add(cfg, start)` creates a statistics row and starts life 1 at `start`. Calling it again with the same ID behaves the same as `Reset` (the life index advances by 1). In that case `cfg` is taken in again (in case the configuration changed on reload).
- `Reset(id, start)` discards Latest, Totals, hour groups, and enhanced history, and starts a new life at `start`. Results of attempts that started before `start` and arrive afterwards are discarded (the "Time reference" section). The Index of hour groups and enhanced history goes back to 1. The history life index advances by 1, and history of previous lives remains within `Lives`. The configuration is kept.
- `Remove(id)` deletes the statistics row.
- `Record` / `Reset` / `Remove` for an unregistered ID do nothing (and log nothing).

## Reading

- `Snapshot(id, opts)` returns a copy of the values. The slices (`Hours`, each `Dist`, `History`, `Enhanced`) are newly allocated, so modifications by the caller do not affect the `Store`. Parts not requested in `opts` are nil. Parts requested but with no content are empty slices (nil when enhanced history is disabled).
- `Summary()` returns the ID, kind, target, tags, Latest, and Totals of all operations in ID order. An ascending list of IDs is maintained on `Add` / `Remove`, so no sorting is needed.

## icmp-jitter

icmp-jitter sends `NumPackets` ICMP Timestamps in a single attempt (burst). The per-burst and per-packet definitions are given below.

### Per-burst accounting

- `Counters` (Initiations, Completions, OverThresholds, Timeouts, and so on) are counted **per burst** (to match Cisco's "Number of successes", which is per burst). The return code is decided by `internal/op`.
  - If there is at least one reply and the average RTT is at or below the threshold, `ok`; if it exceeds the threshold, `overThreshold`
  - If there is no reply at all, `timeout`
  - If nothing could be sent, `error`
- `Result.RTT` is the average RTT of the burst (Cisco's "Latest RTT value is equal to the average RTT value"). Therefore the RTT accumulation in `Counters` is the accumulation of "the burst's average RTT truncated to ms". Per-packet RTT is in `JitterCounters`.

### Per-packet values (`op.JitterResult`, one burst)

`computeJitter` in `internal/op` computes these from each packet's O (Originate written by the source), R (the target's Receive), T (the target's Transmit), and A (the source's receive time). All units are integer ms.

| Value | Definition |
|---|---|
| `NumRTT`, `RTTSumMs`, `RTTSum2Ms`, `RTTMinMs`, `RTTMaxMs` | RTT of packets that were replied to (A − S, truncated to ms) |
| `NumOverThreshold` | Number of packets with RTT_i > threshold |
| `PosSD` / `NegSD` | SD jitter (R_i − R_{i−1}) − (O_i − O_{i−1}). 0 or more is positive; negative is negative, stored as the absolute value |
| `PosDS` / `NegDS` | DS jitter (A_i − A_{i−1}) − (T_i − T_{i−1}). A is truncated to ms before taking the difference |
| `PktLoss` | Number of packets sent that got no reply by the deadline (including Unreachable) |
| `MinSucPktLoss` / `MaxSucPktLoss` | Minimum and maximum length of consecutive losses (0/0 if there was no loss) |
| `PktLateArrival` | Number of replies that arrived after the deadline and before the end of the burst |
| `PktOutSeqSD` / `PktOutSeqDS` / `PktOutSeqBoth` | Compared with the previous reply in send order, the number of times only R was reversed / only arrival order was reversed / both were reversed |
| `Skipped` | Number of packets not sent (could not be sent). Excluded from the denominator of all other values |
| `OneWay`, `NumOW`, `OWSD`, `OWDS` | One-way delay R − O and A − T. Aggregated only when enabled in the configuration |

- **Jitter samples** are made only when two packets with adjacent numbers were both replied to. Both loss and skipped break the pair. With no loss there are N − 1 samples (matching the real device's "Number of SD Jitter Samples: 9").
- **Consecutive loss**: skipped does not break a run of losses and is not counted ("statistics are measured on sent packets only").
- **RFC 792 timestamps**: Differences of R, T, and O are corrected for the 86,400,000 ms wraparound (the day boundary) (if a difference is below −43,200,000, add 86,400,000; if it exceeds 43,200,000, subtract 86,400,000). The most significant bit (the non-standard value flag) is masked when used for jitter, and replies with it set are excluded from the one-way delay.
- **One-way delay**: A reply with a negative SD or DS (the clocks are not synchronized) is not used as a one-way sample. `OneWay` is true if there is at least one sample. One-way delays are aggregated only when the configuration key `one-way-delay` is `true`; with the default `false`, they are never aggregated.
- **Average jitter**: `AvgJitterMs` is the average of the absolute values of all samples of the four kinds, and `AvgSDJitterMs` / `AvgDSJitterMs` are per-direction averages (the MIB's "average of positive and negative jitter values").

### Accumulation into statistics (`JitterCounters`)

- An icmp-jitter row has `JitterCounters` (corresponding to rttMonIcmpJitterStatsTable) in `Snapshot.TotalsJitter` (the whole life) and in each `HourGroup.Jitter`. For other kinds these are nil.
- Each burst's `JitterResult` is added. Sums and sample counts are added up, and minimum and maximum are taken across all bursts. `MinSucPktLoss` / `MaxSucPktLoss` take the minimum and maximum only over bursts that had loss (so that bursts without loss do not pull the minimum down to 0).
- busy and sequenceError results have no `Jitter`, so `JitterCounters` does not change (only Busies / SequenceErrors in `Counters` increase).
- `Latest.Jitter` is a copy of the latest burst's `JitterResult`.
- **No distribution buckets** (`HourGroup.Dist = nil`). **No history is recorded either** (the MIB's history table is "not applicable to http and jitter probes"). A configuration with `Lives > 0` is ignored.
- Enhanced history holds only the per-burst `Counters` (no jitter accumulators).

## Concurrency

- `Store` protects a `map[int]*row` and the ascending ID list with an RWMutex, and each row has its own mutex.
- `Record` / `Snapshot` take the Store read lock to look up the row, then take only that row's mutex. `Summary` holds the Store read lock and takes each row's mutex only while copying. Only `Add` / `Remove` take the Store write lock.
- This design was chosen based on measurements. If the whole Store is protected by one mutex, every `Record` stops while `Summary` runs (about 150 µs for 1,000 entries). With per-row mutexes, `Record` takes about 240 ns even while running concurrently with `Summary`.

## Intentional deviations from Cisco

| Item | Cisco | goipsla | Reason |
|---|---|---|---|
| Retention of history buckets | The CLI documentation says "History buckets do not wrap" (recording stops when the limit is reached) | The latest `Buckets` buckets are kept, and the index keeps advancing | Follows the MIB definition ("the most recent rttMonHistoryAdminNumBuckets buckets are retained (the index is incremented though)"). For monitoring, the latest state is more useful |
| End of enhanced history | "When the specified number of buckets is reached, statistic gathering for the operation ends" | The latest `Buckets` buckets are kept and collection continues | For a long-running daemon, it is inconvenient for enhanced history to stop after a few hours. Cisco also does not say whether this wording stops the regular statistics too |
| Destination Unreachable | Cisco's handling is undocumented | The attempt ends without waiting for the timeout, and the return code is `timeout` (the unreachable details are kept in `Detail`) | The option adopted in section 7 of the implementation plan. In accounting it is counted as Timeouts |
| Gaps in hour groups | Undocumented | Groups are not created for hours with no results; only the Index advances | Cisco creates a group per hour, but keeping empty groups for hours with no results carries no information |
| History bucket numbers | Undocumented (numbering when a filter is used) | The number advances per attempt, and numbers skip for attempts not recorded because of the filter | Literal interpretation of the MIB's "increments on each operation attempt" |
| icmp-jitter RTT and jitter | The arithmetic for the ICMP version is undocumented | The formulas in the "Per-packet values" section (RTT does not subtract the target's processing time, a difference of 0 is positive) | Adopts the inference from the spec research |
| Classification of icmp-jitter reordering | The CLI shows three counts, SD / DS / both, but their definitions are undocumented | Compared with the previous reply in send order, a reversal of only R is SD, of only arrival order is DS, and of both is both, counted so that they do not overlap | The three counters are made mutually exclusive so that their total is the total number of reversals |
| Return codes that do not occur for ICMP | — | Only Initiations is incremented (counted as failures) | They are not in the accounting table, so they are treated as a kind of failure |
