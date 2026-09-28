# goipsla command reference

`goipsla` is the CLI of `goipslad`. Every command except `validate`, `version` and `completion` connects to the daemon's Unix socket (`docs/api.md`) to get its information. Only the configuration file and `reload` change the daemon's settings; the CLI never rewrites the configuration of an operation.

## Contents

| Command | Description |
|---|---|
| [`goipsla show operations`](#goipsla-show-operations) | One-line list of every operation |
| [`goipsla show statistics`](#goipsla-show-statistics) | Latest result and totals of the current life (vertical layout) |
| [`goipsla show statistics aggregated`](#goipsla-show-statistics-aggregated) | Aggregation per hour (hour groups) and distribution buckets |
| [`goipsla show history`](#goipsla-show-history) | History buckets |
| [`goipsla show enhanced-history`](#goipsla-show-enhanced-history) | Enhanced history |
| [`goipsla show config`](#goipsla-show-config) | Effective configuration of one operation |
| [`goipsla show reactions`](#goipsla-show-reactions) | Threshold reaction rows and their state |
| [`goipsla show track`](#goipsla-show-track) | Tracking state |
| [`goipsla show events`](#goipsla-show-events) | Recent events |
| [`goipsla watch`](#goipsla-watch) | Redraw the list periodically |
| [`goipsla reload`](#goipsla-reload) | Reload the configuration file and apply the differences |
| [`goipsla restart`](#goipsla-restart) | Discard the statistics of one operation and start a new life |
| [`goipsla reset`](#goipsla-reset) | Reset the statistics and schedules of every operation |
| [`goipsla health`](#goipsla-health) | Daemon version, start time and operation counts |
| [`goipsla validate`](#goipsla-validate) | Validate a configuration file (without connecting to the daemon) |
| [`goipsla version`](#goipsla-version) | Version of goipsla itself |
| [`goipsla completion`](#goipsla-completion) | Shell completion script |

## Mapping to Cisco commands

| Cisco IOS | goipsla | Notes |
|---|---|---|
| `show ip sla summary` | `show operations` | Columns are `ID TYPE TARGET TAG STATE RC RTT(ms) LAST ...`. Can be filtered by tag, state, return code and type |
| `show ip sla statistics [<id>]` | `show statistics [<id>]` | The headings of the vertical layout follow Cisco. icmp-jitter follows Cisco's icmp-jitter layout |
| `show ip sla statistics [<id>] details` | `show statistics [<id>] --details` | |
| `show ip sla statistics aggregated [<id>]` | `show statistics aggregated [<id>]` | Distribution buckets with `--details` |
| `show ip sla history [<id>] tabular` | `show history [<id>]` | |
| `show ip sla enhanced-history [<id>]` | `show enhanced-history [<id>]` | |
| `show ip sla configuration [<id>]` | `show config <id>` | Printed with the key names of the configuration file |
| `show ip sla reaction-configuration [<id>]` | `show reactions [<id>]` | Also shows the state (occurred) |
| `show track [<n>]` | `show track [<n>]` | |
| `ip sla restart <id>` | `restart <id>` | |
| `ip sla reset` | `reset` | Does not delete the configuration (below) |
| (Reloading the configuration; IOS has no equivalent) | `reload` | Same as SIGHUP |

## Common flags

| Flag | Default | Description |
|---|---|---|
| `-o`, `--output` | `table` | `table` (tables for people) or `json` (the API response, indented). Any other value is a usage error (exit status 2) |
| `--socket` | `/run/goipslad/goipslad.sock` | Socket to connect to |
| `-h`, `--help` | | Print the help of the command (exit status 0) |
| `-v`, `--version` | | Root only. Prints one line, `goipsla <version>` (`goipsla version` also prints the Go version and platform) |

## Exit status

| Status | Meaning |
|---|---|
| 0 | Success. Also 0 when a list is empty; `no operations` or similar is printed on standard error |
| 1 | API error (cannot connect, 404, 409 and so on). The message is printed on standard error in the form `goipsla: ...`. When it cannot connect: `goipsla: cannot connect to <socket>: <reason> (is goipslad running?)` |
| 2 | Usage error (unknown flag, unknown subcommand, bad value for `-o`, bad ID, bad filter value, too many or too few arguments). The message and `Run '<command> --help' for usage.` are printed on standard error. A misspelled subcommand, such as `goipsla show operatoins`, is also 2, without printing the help |

Exit statuses specific to commands:

- Validation errors of `validate`: 1 (every error is printed on standard error).
- An invalid configuration on `reload`: 1 (the daemon keeps the running configuration).
- Answering anything but `y` to the confirmation of `reset`: 1 (`reset canceled`).

## Display conventions

- RTTs are printed in milliseconds with 3 decimals (for example `1.234`). Means (`AVG(ms)`, the middle of `MIN/AVG/MAX`) are also computed from the statistics and printed with 3 decimals. Minimums, maximums and sums are printed as integer milliseconds (truncated, as in Cisco).
- Times in tables are local times, `15:04:05`. Vertical layouts use the form `2006-01-02 15:04:05 MST`.
- The `LAST` column is the time elapsed since the latest attempt finished, truncated to seconds (`3s ago`, `2m10s ago`). `-` before the first attempt.
- A field without a value is shown as `-`.
- The `RC` and `SENSE` columns show the MIB name of the return code (`ok`, `overThreshold`, `timeout` ...). Only `Latest operation return code` in vertical layouts uses the Cisco CLI display name (`OK`, `Over Threshold`, `Timeout` ...).
- The content and key names of `-o json` are the same as the responses in `docs/api.md`.

## `goipsla show operations`

Lists every operation, one per line. The main screen for overseeing many targets. The corresponding Cisco command is `show ip sla summary`.

```
goipsla show operations [--tag T] [--state S] [--rc RC] [--type TYPE]
```

Filters are combined with AND. A bad value is a usage error (exit status 2).

| Flag | Value |
|---|---|
| `--tag` | Tag (exact match) |
| `--state` | `pending` / `inactive` / `active` |
| `--rc` | MIB name of the latest return code (case-insensitive; `other` before the first attempt) |
| `--type` | `icmp-echo` / `icmp-jitter` |

```console
$ goipsla show operations
ID   TYPE         TARGET          TAG  STATE    RC       RTT(ms)  LAST      SUCC  FAIL  AVG(ms)  JIT(ms)  LOSS
1    icmp-jitter  10.100.2.11     -    active   ok       0.412    2s ago    40    0     1.000    0.143    1/10
101  icmp-echo    10.100.1.11     wan  active   ok       1.234    2m9s ago  115   5     2.000    -        -
102  icmp-echo    fd00:100:2::11  wan  active   timeout  -        6s ago    0     12    -        -        -
103  icmp-echo    10.100.1.13     lan  pending  -        -        -         0     0     -        -        -
```

| Column | Meaning |
|---|---|
| `ID`, `TYPE`, `TARGET`, `TAG` | Identify the operation |
| `STATE` | State (pending / inactive / active) |
| `RC` | Return code of the latest attempt |
| `RTT(ms)` | RTT of the latest attempt. Shown only for ok / overThreshold |
| `LAST` | Time elapsed since the latest attempt finished |
| `SUCC` | Successes in the current life (completions − overThreshold) |
| `FAIL` | Failures in the current life (attempts − successes) |
| `AVG(ms)` | Mean RTT of the completions in the current life |
| `JIT(ms)` | Mean jitter of the latest icmp-jitter burst (the mean of the absolute values of every SD and DS sample, positive and negative; 3 decimals). `-` for icmp-echo and before the first attempt |
| `LOSS` | Packets lost in the latest icmp-jitter burst / packets sent (for example `1/10`). Packets not sent (skipped) are in neither count (counted the same way as `loss 1/10` in the result detail). `-` for icmp-echo and before the first attempt |

- For icmp-jitter, `RTT(ms)` is the mean RTT of the latest burst, and `SUCC` / `FAIL` / `AVG(ms)` count per burst.
- If no operation matches, `no operations` is printed on standard error (exit status 0; `[]` with `-o json`).

## `goipsla show statistics`

Prints the latest result and the totals of the current life. The corresponding Cisco command is `show ip sla statistics [<id>] [details]`.

```
goipsla show statistics [<id>] [--details]
```

| Flag | Description |
|---|---|
| `--details` | Adds every counter of the current life (attempts, completions, counts per return code, minimum, mean and maximum RTT, standard deviation, sum, sum of squares) |

- Without an id, prints the same list as `show operations`. With `--details`, prints every operation one by one in the vertical layout, separated by blank lines. The details are fetched in one request, `GET /v1/operations?include=detail`.
- With an id, prints a vertical layout similar to Cisco's `show ip sla statistics`.

```console
$ goipsla show statistics 101
IPSLA operation id:            101
Type of operation:             icmp-echo
Target address:                10.100.1.11 (vrf blue)
Tag:                           wan
Latest RTT:                    1.234 milliseconds
Latest operation start time:   2026-09-27 11:57:50 UTC
Latest operation return code:  OK
Number of successes:           115
Number of failures:            5
Operational state:             active
Life start:                    2026-09-27 10:00:00 UTC (life 1)
```

With `--details`, the following lines are added.

```console
$ goipsla show statistics 101 --details
...
Life start:                    2026-09-27 10:00:00 UTC (life 1)
Initiations:                   120
Completions:                   118
Over thresholds:               3
Timeouts:                      2
Busies:                        0
Drops:                         0
Sequence errors:               0
Verify errors:                 0
RTT Min/Avg/Max:               1/2.000/9 milliseconds
RTT standard deviation:        1.042 milliseconds
RTT sum / sum of squares:      236 / 600
```

`Latest RTT` is shown as follows, depending on the situation.

- For a failed attempt, `NoConnection/Busy/Timeout` (the same wording as Cisco).
- Before the first attempt, `Unknown`. Then `Latest operation start time` is `-` and `Latest operation return code` is `Unknown`.
- When there is additional information, a `Latest operation detail` line is added (for example, the sender of an unreachable: `destination unreachable: host, from fd00:100:1::254`).

### Vertical layout for icmp-jitter

For icmp-jitter operations, the values of the latest burst are printed in a layout modeled on Cisco's `show ip sla statistics` for icmp-jitter. The headings follow Cisco. `Number of successes` / `Number of failures` count per burst.

```console
$ goipsla show statistics 1
IPSLA operation id: 1
Type of operation: icmp-jitter
Target address: 10.100.2.11
        Latest RTT: 0.412 milliseconds
Latest operation start time: 2026-09-27 11:59:57 UTC
Latest operation return code: OK
Latest operation detail: loss 1/10
RTT Values:
        Number Of RTT: 9               RTT Min/Avg/Max: 0/0.444/1 milliseconds
Latency one-way time:
        Number of Latency one-way Samples: 0
        Source to Destination Latency one way Min/Avg/Max: 0/0.000/0 milliseconds
        Destination to Source Latency one way Min/Avg/Max: 0/0.000/0 milliseconds
Jitter Time:
        Number of SD Jitter Samples: 7
        Number of DS Jitter Samples: 7
        Source to Destination Jitter Min/Avg/Max: 0/0.286/1 milliseconds
        Destination to Source Jitter Min/Avg/Max: 0/0.000/0 milliseconds
Over Threshold:
        Number Of RTT Over Threshold: 0 (0%)
Packet Late Arrival: 0
Out Of Sequence: 0
        Source to Destination: 0        Destination to Source 0
        In both Directions: 0
Packet Skipped: 0
Packet Loss: 1
        Loss Period Length Min/Max: 1/1
Number of successes: 40
Number of failures: 0
Operational state: active
Life start: 2026-09-27 10:00:00 UTC (life 1)
```

- `Number Of RTT` / `RTT Min/Avg/Max`: The number of packets answered and their RTT (truncated to ms; the mean has 3 decimals).
- `Latency one-way time`: Has numbers only when the `one-way-delay` setting is enabled.
- `Jitter Time`: The number of SD / DS jitter samples, and the minimum, mean and maximum of their absolute values, positive and negative together.
- `Out Of Sequence`: The total and breakdown of SD (reordered in R only), DS (reordered in arrival order only) and both.
- `Loss Period Length Min/Max`: The shortest and longest run of successive losses.
- Cisco's `Packet Unprocessed`, `Loss Periods Number` and `Inter Loss Period Length` are not recorded and not printed.

With `--details`, the per-burst counts of the current life and the per-packet accumulation are added after `Life start`. The per-packet accumulation uses the same headings as above, indented by 8 columns.

```console
$ goipsla show statistics 1 --details
...
Life start: 2026-09-27 10:00:00 UTC (life 1)
Bursts of the current life:
        Initiations: 40  Completions: 40  Over thresholds: 0
        Timeouts: 0  Busies: 0  Drops: 0  Sequence errors: 0
Packets of the current life:
        RTT Values:
                Number Of RTT: 396               RTT Min/Avg/Max: 0/0.404/3 milliseconds
        ...
        Packet Loss: 4
                Loss Period Length Min/Max: 1/2
```

## `goipsla show statistics aggregated`

Prints the aggregation per hour (hour groups), one per line. The corresponding Cisco command is `show ip sla statistics aggregated [<id>]`.

```
goipsla show statistics aggregated [<id>] [--details]
```

| Flag | Description |
|---|---|
| `--details` | Adds a table of distribution buckets for each hour group (icmp-echo only) |

Without an id, every operation is printed in turn under the heading `Operation <id> (<type> <target>)`. The hour groups are fetched in one request, `GET /v1/operations?include=hours`.

```console
$ goipsla show statistics aggregated 101 --details
Operation 101 (icmp-echo 10.100.1.11)
INDEX  START     RTTs  MIN/AVG/MAX  SUCC  FAIL  OVERTH  TIMEOUT  BUSY  DROP  SEQERR  VERERR
1      10:00:00  59    1/2.000/9    58    2     1       1        0     0     0       0
2      11:00:00  59    1/2.000/4    57    3     2       1        1     0     0       0

Distribution of hour group 1:
RANGE   COMPLETIONS  OVERTH  %     AVG(ms)
0-<5ms  58           0       98.3  1.879
>=5ms   1            0       1.7   9.000

Distribution of hour group 2:
RANGE   COMPLETIONS  OVERTH  %      AVG(ms)
0-<5ms  59           0       100.0  2.000
>=5ms   0            0       0.0    -
```

| Column | Meaning |
|---|---|
| `INDEX` | Start Time Index (starts at 1 and keeps increasing) |
| `START` | Start time of the hour group |
| `RTTs` | Number of completions (Cisco's Number Of RTT) |
| `MIN/AVG/MAX` | RTT of the completions (ms) |
| `SUCC`, `FAIL` | Successes and failures |
| `OVERTH`, `TIMEOUT`, `BUSY`, `DROP`, `SEQERR`, `VERERR` | Counts per return code |

The table of distribution buckets (`--details`):

- `RANGE` has the form `lower-<upperms`; the last bucket is `>=lowerms`.
- `COMPLETIONS` is the number of completions in the bucket, and `OVERTH` is how many of them exceeded the threshold (rttMonStatsCaptureOverThresholds; `over_thresholds` in the API).
- `%` is the share of the completions of that hour group (1 decimal). `AVG(ms)` of a bucket with no completions is `-`.

When there are no hour groups (`hours-of-statistics-kept: 0`, or before the first attempt):

- With one id, `no hour groups` is printed on standard error.
- Without an id, `no hour groups` is printed under that operation's heading.

The table for icmp-jitter operations:

- `RTTs` and `MIN/AVG/MAX` use per-packet RTTs (the number of packets answered and their RTT).
- The columns `SDJ` (mean SD jitter), `DSJ` (mean DS jitter), `LOSS` (packets lost), `LATE` (late arrivals) and `OOS` (total reorderings) are added.
- `SUCC` through `VERERR` count per burst.
- jitter has no distribution buckets, so `--details` prints no distribution table. In that case, the line `distribution is not kept for icmp-jitter` is printed on standard error (one line even when no id is given and there are several jitter operations; the exit status stays 0; not printed with `-o json`).

```console
$ goipsla show statistics aggregated 1
Operation 1 (icmp-jitter 10.100.2.11)
INDEX  START     RTTs  MIN/AVG/MAX  SUCC  FAIL  OVERTH  TIMEOUT  BUSY  DROP  SEQERR  VERERR  SDJ    DSJ    LOSS  LATE  OOS
2      11:00:00  396   0/0.404/3    40    0     0       0        0     0     0       0       0.286  0.057  4     1     2
```

## `goipsla show history`

Prints the history buckets, oldest first. The corresponding Cisco command is `show ip sla history [<id>] tabular`.

```
goipsla show history [<id>]
```

- When `history.lives-kept` is 0, there is no history. With an id, `no history buckets` is printed on standard error (icmp-jitter has no history).
- Without an id, the history of every operation is fetched in one request (`GET /v1/operations?include=history`) and printed per operation under the heading `Operation <id> (<type> <target>)`. An operation without history gets `no history buckets` under its heading.
- With `-o json`, the detail object is printed when an id is given, and the array of details otherwise.

```console
$ goipsla show history
Operation 1 (icmp-jitter 10.100.2.11)
no history buckets

Operation 101 (icmp-echo 10.100.1.11)
LIFE  BUCKET  SAMPLE  START     RTT(ms)  SENSE    TARGET
1     14      1       11:57:30  2        ok       10.100.1.11
1     15      1       11:57:40  0        timeout  10.100.1.11
1     16      1       11:57:50  1        ok       10.100.1.11

Operation 102 (icmp-echo fd00:100:2::11)
no history buckets

Operation 103 (icmp-echo 10.100.1.13)
no history buckets
```

Example with an id:

```console
$ goipsla show history 101
LIFE  BUCKET  SAMPLE  START     RTT(ms)  SENSE    TARGET
1     14      1       11:57:30  2        ok       10.100.1.11
1     15      1       11:57:40  0        timeout  10.100.1.11
1     16      1       11:57:50  1        ok       10.100.1.11
```

`RTT(ms)` is integer ms, and 0 for anything but ok (as the MIB defines it). `BUCKET` keeps increasing, and only the latest `buckets-kept` buckets are kept.

## `goipsla show enhanced-history`

Prints the enhanced history (`history.enhanced`) buckets, oldest first. The corresponding Cisco command is `show ip sla enhanced-history [<id>]`.

```
goipsla show enhanced-history [<id>]
```

With an id and no enhanced history, `no enhanced history buckets` is printed on standard error. Without an id, as with `show history`, the enhanced history of every operation is fetched in one request (`include=enhanced`) and printed per operation under a heading.

```console
$ goipsla show enhanced-history 101
INDEX  START     COMPS  OVERTH  SUM  SUM2  MIN  MAX  TIMEOUT  BUSY  DROP  SEQERR  VERERR
1      11:58:00  6      1       12   30    1    4    0        0     0     0       0
2      11:59:00  5      0       10   20    2    2    1        0     0     0       0
```

`SUM`, `SUM2`, `MIN` and `MAX` are the sum, sum of squares, minimum and maximum of the RTTs of completions (integer ms).

## `goipsla show config`

Prints the effective configuration of one operation, with defaults filled in and templates expanded. The corresponding Cisco command is `show ip sla configuration [<id>]`.

```
goipsla show config <id>
```

Printed vertically as `key: value`, with the same key names as the configuration file. With `-o json`, only the configuration object is printed as JSON. Only the operation's configuration is printed; `global` (such as SNMP communities) and `actions` (webhook URLs) are not included. To see the whole configuration, use `goipsla validate --print`.

```console
$ goipsla show config 101
id: 101
type: icmp-echo
target: 10.100.1.11
target-name: 10.100.1.11
template: wan-echo
source-interface: eth1
vrf: blue
tos: 184
frequency: 10s
timeout: 2000ms
threshold: 300ms
request-data-size: 28
data-pattern: 0xABCDABCD
verify-data: false
tag: wan
owner: ""
history:
  lives-kept: 1
  buckets-kept: 15
  filter: all
  hours-of-statistics-kept: 2
  distributions-of-statistics-kept: 2
  statistics-distribution-interval: 5ms
  enhanced:
    interval: 60s
    buckets: 100
schedule:
  life: forever
  start-time: now
  ageout: 0s
  recurring: false
react:
  - element: rtt
    threshold-type: consecutive
    count: 3
    x: 5
    y: 5
    upper: 300
    lower: 200
    action: trap
```

- `schedule: null` means the operation starts immediately at load time and runs forever. This is what omitting `schedule`, or writing `schedule: {}`, gives.
- `enhanced: null` means the enhanced history is disabled, and `react: []` that there are no reactions.
- An unknown id gives `goipsla: operation <id> not found` (exit status 1).

## `goipsla show reactions`

Prints the threshold reaction (`ip sla reaction-configuration`) rows and their state, one per line. The corresponding Cisco command is `show ip sla reaction-configuration [<id>]`.

```
goipsla show reactions [<id>]
```

Without `id`, all operations. If there are no reactions, `no reactions` is printed on standard error.

```console
$ goipsla show reactions
OP  ELEMENT  TYPE         RISING  FALLING  COUNT  X/Y  ACTION           OCCURRED  VALUE  LAST-CHANGE
13  rtt      immediate    100     50       -      -    syslog           false     1      -
13  timeout  consecutive  -       -        2      -    syslog           true      1      47s ago
21  rtt      xofy         100     50       -      3/5  trap-and-syslog  true      150    47s ago
```

- `RISING` / `FALLING` are the thresholds (`upper` / `lower`). `-` for boolean elements (timeout, verifyError).
- `COUNT` only for consecutive / average, `X/Y` only for xofy.
- `OCCURRED` is true while in violation, `VALUE` is the last evaluated value, and `LAST-CHANGE` is the time since occurred last changed.

## `goipsla show track`

Prints the state of tracking (`tracks`). The corresponding Cisco command is `show track [<n>]`.

```
goipsla show track [<n>]
```

- `<n>` is the track number, 1..2147483647 (out of range is a usage error).
- An unknown number gives `goipsla: track <n> not found` (exit status 1).
- If there are no tracks, `no tracks` is printed on standard error.

With a number, a vertical layout modeled on Cisco's `show track` is printed.

```console
$ goipsla show track 1
Track 1
  IP SLA 13 reachability
  Reachability is Up
    2 changes, last change 00:00:47
  Delay up 5 secs, down 0 secs
  Latest operation return code: OK
  Latest RTT (millisecs) 4.000
```

- In `state` mode, the third line is `State is Up`.
- While a delay is running, an `Up pending (delay running)` line is added.
- If both delays are 0, the `Delay` line is not printed.

Without a number, a list is printed.

```console
$ goipsla show track
TRACK  OP  MODE          STATE     CHANGES  LAST-CHANGE  RC             RTT(ms)
1      13  reachability  up        2        47s ago      ok             4.000
2      21  state         down->up  1        1h2m0s ago   overThreshold  150.000
3      99  state         unknown   0        -            -              -
```

`down->up` in `STATE` means the track is due to change to Up when the running delay ends.

## `goipsla show events`

Prints the recent events (threshold reaction and tracking notifications), oldest first. Cisco has no direct equivalent (they correspond to the `%RTT-…` and `%TRACK-…` syslog lines).

```
goipsla show events [--limit N]
```

| Flag | Default | Description |
|---|---|---|
| `--limit` | `100` | Number of events to print. 1..1000 (out of range is a usage error). The daemon keeps the latest 1000 |

```console
$ goipsla show events
TIME                     KIND                OP  TARGET       DETAIL
2026-09-27 11:59:00 UTC  track-down          13  10.100.1.13  %TRACK-6-STATE: 1 ip sla 13 reachability Up -> Down
2026-09-27 11:59:05 UTC  threshold-exceeded  13  10.100.1.13  %RTT-4-OPER_TIMEOUT: IP SLAs(13): Threshold exceeded for timeout
```

- `DETAIL` is the one-line text also used for syslog.
- If there are no events, `no events` is printed on standard error.
- The result of each attempt (timeout and so on) is not an event. See the results in the daemon's log (the "Logging settings" item in `docs/operations.md`) and with `show history`.

## `goipsla watch`

Redraws the `show operations` list periodically, clearing the screen. Cisco has no equivalent.

```
goipsla watch [--interval 2s] [--tag T] [--state S] [--rc RC] [--type TYPE]
```

| Flag | Default | Description |
|---|---|---|
| `--interval` | `2s` | Redraw interval. A positive value (`0s` or less is a usage error) |
| `--tag`, `--state`, `--rc`, `--type` | | The same filters as `show operations` |

```console
$ goipsla watch --tag wan
Every 2s: goipsla show operations --tag wan    2026-09-27 12:00:00

ID   TYPE       TARGET          TAG  STATE   RC       RTT(ms)  LAST      SUCC  FAIL  AVG(ms)  JIT(ms)  LOSS
101  icmp-echo  10.100.1.11     wan  active  ok       1.234    2m9s ago  115   5     2.000    -        -
102  icmp-echo  fd00:100:2::11  wan  active  timeout  -        6s ago    0     12    -        -        -
```

- The first line shows the interval, the filters and the current time.
- Exits normally on Ctrl-C (SIGINT) or SIGTERM (exit status 0).
- When it cannot connect to the daemon, it does not exit; it shows `error: ...` and keeps retrying (so it keeps watching across a daemon restart).
- Cannot be combined with `-o json` (a usage error, exit status 2).

## `goipsla reload`

Makes goipslad reload its configuration file and apply the differences. It goes through the same processing as SIGHUP (`systemctl reload goipslad`) (the "Reload (P4)" section of `docs/scheduling.md`). IOS has no equivalent (IOS changes the configuration directly from the CLI).

```
goipsla reload
```

Only the common flags (`-o`, `--socket`).

- Operations whose measurement settings (target, frequency, timeout and so on) changed are rebuilt (`restarted`), and their statistics are discarded.
- Operations where only tag / owner / react changed are updated, keeping their statistics (`updated`).
- If the configuration file is invalid, nothing is applied and the daemon keeps the running configuration.

```console
$ goipsla reload
added:      [5]
removed:    [4]
restarted:  [2 3]
updated:    []
warning: global.api-socket changed; it requires a restart of goipslad to take effect
```

- `added` / `removed` / `restarted` / `updated` are the lists of the corresponding operation IDs.
- A `warning:` line is a change that reload does not apply and that needs a daemon restart (changes to `global` and `actions`).
- With `-o json`, `{"added": [...], "removed": [...], "restarted": [...], "updated": [...], "warnings": [...]}` is printed (`warnings` is omitted when there are none).

When the configuration is invalid (exit status 1):

```console
$ goipsla reload
goipsla: reload failed; goipslad keeps the running configuration: invalid configuration:
  operations[0].timeout: must be at least 1ms
  operations[1].frequency: must be greater than timeout
```

Exit status: 0 (applied), 1 (invalid configuration, cannot connect, other API errors), 2 (usage errors such as giving arguments).

## `goipsla restart`

Discards the statistics of an active operation and starts a new life at once. The corresponding Cisco command is `ip sla restart <id>`.

```
goipsla restart <id>
```

Only the common flags.

- The life is reloaded from the configuration, and Seq counts again from 1. The numbers of hour groups and of the enhanced history also go back to 1, and the life number of the history advances by one.
- Only an active operation can be restarted (as in Cisco). To start a pending operation, change `start-time` in the configuration and `reload`.

```console
$ goipsla restart 101
operation 101 restarted
```

With `-o json`, `{"restarted": 101}` is printed.

On failure:

```console
$ goipsla restart 103
goipsla: operation 103 is pending: only an active operation can be restarted
$ goipsla restart 999
goipsla: operation 999 not found
```

Exit status: 0 (restarted), 1 (not active (409 from the API), unknown (404), cannot connect), 2 (missing or bad id).

## `goipsla reset`

Discards the statistics of every operation and puts the schedules back into the initial state they have when goipslad starts. The corresponding Cisco command is `ip sla reset`.

```
goipsla reset [--yes]
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip the confirmation. Required when standard input is not a terminal (scripts, pipes) |

- **Difference from Cisco**: `ip sla reset` also deletes the configuration of every operation, but goipsla does not delete the configuration. goipslad keeps running the configuration file it last loaded ("Deviations from Cisco" in `docs/scheduling.md`).
- Operations deleted by ageout are recreated too.
- From a terminal, it asks for confirmation. Any answer other than `y` / `yes` (case-insensitive) cancels.

```console
$ goipsla reset
Discard the statistics of every operation and restart all schedules? [y/N] y
all operations reset
$ goipsla reset --yes
all operations reset
```

- The confirmation question is printed on standard error.
- With `-o json`, `{"reset": true}` is printed.
- When canceled, `reset canceled` is printed on standard error.
- When standard input is not a terminal and `--yes` is not given, it ends with `goipsla: reset needs --yes when standard input is not a terminal` without doing anything.

Exit status: 0 (reset), 1 (canceled at the confirmation, cannot connect), 2 (not a terminal and no `--yes`, other usage errors).

## `goipsla health`

Prints the daemon version, start time, configuration file and operation counts per state. IOS has no direct equivalent.

```
goipsla health
```

```console
$ goipsla health
Version:     0.3.0
Started:     2026-09-27 10:30:00 UTC (up 1h30m0s)
Config:      /etc/goipslad/goipslad.yaml
Operations:  4 total, 3 active, 1 pending, 0 inactive
```

With `-o json`, `{"version", "started_at", "config_path", "operations": {"total", "active", "pending", "inactive"}}` is printed. Exit status 1 if it cannot connect. It can also be used as a liveness check from monitoring.

## `goipsla validate`

Validates a configuration file without connecting to the daemon. For details, see the "`goipsla validate`" section of `docs/config.md`.

```
goipsla validate <file> [--print] [--show-secrets]
```

| Flag | Description |
|---|---|
| `--print` | Print the effective configuration. The table format lists ID, type, target, frequency, timeout, threshold and tag, followed by the `OK:` line. With `-o json`, only the JSON of the whole configuration is printed (warnings go to standard error) |
| `--show-secrets` | Do not redact secrets in `--print -o json` |

- On success, `OK: <N> operations` is printed on standard output (exit status 0).
- On failure, every error is printed on standard error, one per line, in the form `<location>: <message>` (exit status 1).
- Settings that are accepted but have no effect (`config.Config.Warnings`: unused templates, actions with no receiver and so on) are printed on standard error in the form `warning: <location>: <message>`. With warnings, the exit status is still 0.
- `--print -o json` redacts secrets. With `--show-secrets`, they are not redacted.
  - The SNMP `community` becomes `***`.
  - Everything after the host of a webhook URL (path, userinfo, query, fragment) becomes `***` (for example `https://hooks.example/***`), because the path itself can be a token.
  - The table format (`--print` alone) does not print these in the first place.
- A missing file name, or two or more, is a usage error (exit status 2).

```console
$ goipsla validate /etc/goipslad/goipslad.yaml
warning: templates.spare: not used by any operation
OK: 4 operations
```

With `-o json` and without `--print`, the result is printed as a JSON object on standard output (for scripts and CI). Warnings and errors then go into this JSON, and nothing is printed on standard error.

```console
$ goipsla -o json validate /etc/goipslad/goipslad.yaml
{
  "ok": true,
  "operations": 4,
  "warnings": [
    "templates.spare: not used by any operation"
  ]
}
$ goipsla -o json validate bad.yaml; echo $?
{
  "ok": false,
  "errors": [
    "operations[0].tos: must be between 0 and 255",
    "operations(id=1).timeout: must not exceed frequency (5s)"
  ]
}
1
```

- Success: `{"ok": true, "operations": <N>, "warnings": [...]}` (`[]` when there are no warnings). Exit status 0.
- Failure: `{"ok": false, "errors": ["<location>: <message>", ...]}`. Exit status 1. Besides validation errors, YAML syntax errors and a file that cannot be read take the same form.

## `goipsla version`

Prints the version of `goipsla` itself. Does not connect to the daemon (the daemon's version is in `goipsla health`).

```
goipsla version
```

```console
$ goipsla version
goipsla 0.3.0 (go1.26.7 linux/amd64)
$ goipsla -o json version
{
  "go": "go1.26.7",
  "platform": "linux/amd64",
  "version": "0.3.0"
}
```

`goipsla --version` (`-v`) prints only the one line `goipsla 0.3.0`.

## `goipsla completion`

Prints a completion script for a shell (bash, zsh, fish, powershell) on standard output. A standard cobra command; it does not connect to the daemon. For usage, see `goipsla completion <shell> --help`.

```
goipsla completion bash|zsh|fish|powershell
```

```console
$ goipsla completion bash | sudo tee /etc/bash_completion.d/goipsla >/dev/null
```
