# ICMP engine (`internal/probe`)

`internal/probe` is the engine that sends ICMP Echo and matches replies, ICMP errors, and timeouts to requests.
The types and the `Engine` interface are defined in `probe.go` and `jitter.go` (see "File layout" below); design decisions follow section 3.1 of `docs/implementation-plan.md`.
Linux only (`rawconn_linux.go`).

## File layout

| File | Contents |
|---|---|
| `probe.go` | Contract types, constants, `Engine`, `Options`, `ErrClosed` |
| `engine.go` | `New`, the pending table, matching, timeouts, late-reply detection, cleanup, RTT calculation |
| `jitter.go` | Contract types for icmp-jitter (`JitterRequest`, `JitterPacket`, `JitterReply`, `ErrSkipped`, `ErrUnsupportedFamily`) |
| `jitter_engine.go` | Implementation of `Engine.Jitter` (the send grid, matching of Timestamp Reply, late replies) |
| `outcome.go` | `Outcome.String()` (for logs; an addition to the contract) |
| `icmp.go` | ICMP type numbers, header length, protocol numbers, building Echo / Timestamp Request (`buildICMPEcho`, `buildICMPTimestamp`), checksum (`icmpChecksum`), RFC 792 timestamp (`icmpTimestampMs`), list of types for the receive filter (`acceptedICMPTypes`) |
| `ip.go` | IP header length, skipping the IPv4 header (`stripIPv4Header`) |
| `payload.go` | Format of the Echo data part (custom header, pattern, `payloadOverhead`, `maxPayloadDataSize4/6`) |
| `parse.go` | Parsing received packets (`parseReply`, `parsedMsg`, the embedded data of ICMP errors, `errorDetail` / `codeName`) |
| `conn.go` | Internal socket interfaces (`packetConn`, `connControl`, `connOpener`) |
| `rawconn_linux.go` | Linux implementation of `packetConn` (raw sockets, `sendmsg` / `recvmsg`, control messages, Flow Label). The namespace is `raw` |
| `*_test.go` | Unit tests with a fake socket and `clock.Fake` (engine, jitter, review_p3), table-driven tests (icmp, ip, payload, parse), `FuzzParseReply` |
| `probe_integration_test.go` | `//go:build integration`. Integration tests with real sockets |

The development CLI is `tools/probe-echo` (described below).

**Namespaces (declscope)**: engine.go, jitter_engine.go, the API files (probe.go, jitter.go, outcome.go), and the engine tests are core (the ICMP engine, which is what the name of package probe stands for). icmp / ip / payload / parse / conn / raw are each separate namespaces, and declarations used across layers carry `//declscope:package` with a reason.

## Sockets

- One raw socket is opened per (address family, VRF name) pair and shared by all operations.
  - IPv4: `socket(AF_INET, SOCK_RAW|SOCK_NONBLOCK|SOCK_CLOEXEC, IPPROTO_ICMP)`
  - IPv6: `socket(AF_INET6, SOCK_RAW|SOCK_NONBLOCK|SOCK_CLOEXEC, IPPROTO_ICMPV6)`
- `New` opens the default (no VRF) IPv4 and IPv6 sockets. If IPv4 cannot be opened, `New` returns an error (`EPERM` without `CAP_NET_RAW`). If IPv6 cannot be opened, a warning is logged and IPv6 requests become `OutcomeError` (`Err` is `ipv6 unavailable: ...`). There is no retry.
- A socket with a VRF is opened on the first request and bound to the VRF device with `SO_BINDTODEVICE`. If it cannot be opened, that request is `OutcomeError`, and the next request tries to open it again.
- The fd is wrapped with `os.NewFile`, and `Read` / `Write` go through the `syscall.RawConn` from `SyscallConn()`. Waiting is done by the runtime netpoller (epoll), so there is no busy loop and no thread is occupied. `Close` is `os.File.Close`; it wakes the receive goroutine that is reading, and there is no fd reuse race. `net.FileConn` is not used.
- There is one receive goroutine per socket. A 1 MiB receive buffer is requested (`SO_RCVBUFFORCE`, falling back to `SO_RCVBUF`; the limit is `net.core.rmem_max`).

### Socket options

| Option | Purpose |
|---|---|
| `ICMP_FILTER` (`SOL_RAW`, value 1) | On IPv4, pass only Echo Reply(0), Destination Unreachable(3), Time Exceeded(11), Timestamp Reply(14) (types whose bit is set are dropped) |
| `ICMPV6_FILTER` (`SOL_ICMPV6`, value 1) | On IPv6, pass only Echo Reply(129), Destination Unreachable(1), Time Exceeded(3) |
| `SO_TIMESTAMPNS_NEW` (or `SO_TIMESTAMPNS` if unavailable) | Receive the receive timestamp (`CLOCK_REALTIME`) as a control message |
| `SO_BINDTODEVICE` | Binding to the VRF device |
| `IPV6_FLOWINFO_SEND` / `IPV6_FLOWLABEL_MGR` | Flow Label (described below). Set only when a non-zero label is first sent |

`golang.org/x/sys/unix` has no constants for `ICMP_FILTER`, `ICMPV6_FILTER`, `IPV6_FLOWLABEL_MGR`, and `IPV6_FLOWINFO_SEND`, so they are defined in `rawconn_linux.go`, inside the functions that use them.

### Per-send control messages

| Control message | Condition | Contents |
|---|---|---|
| `IP_PKTINFO` (`struct in_pktinfo`) | When `Source` or `Interface` is specified | Source in `ipi_spec_dst`, interface in `ipi_ifindex` |
| `IP_TOS` (int) | `TOS != 0` | ToS byte |
| `IPV6_PKTINFO` (`struct in6_pktinfo`) | When `Source` or `Interface` is specified | Source and interface |
| `IPV6_TCLASS` (int) | `TOS != 0` | Traffic Class |

- `Request.Interface` is resolved to an index with `net.InterfaceByName` and cached for 30 seconds (failures are cached too).
- For an IPv6 link-local target, the zone (`fe80::1%eth0` or a number) is used as `sin6_scope_id`.
- `sendmsg` calls `SYS_SENDMSG` directly instead of `unix.Sendmsg`, in order to set `sin6_flowinfo` in `sockaddr_in6`. `recvmsg` uses `unix.Recvmsg`.
- The IPv4 ICMP checksum is computed by the engine itself. The ICMPv6 checksum is computed by the kernel (always enabled on ICMPv6 raw sockets).
- Received IPv4 packets carry the IP header, so it is skipped by reading the IHL. For IPv4 the kernel does not verify the ICMP checksum for raw sockets, so the engine verifies it and drops mismatches as "unparsable packets".

### Flow Label

Implemented. Linux cannot send with a non-zero label (`EINVAL`) unless the label is allocated with `IPV6_FLOWLABEL_MGR`.
When a request with a non-zero `FlowLabel` arrives, the following is done once on that socket.

1. `IPV6_FLOWINFO_SEND = 1` (make it use the label in `sin6_flowinfo`)
2. Pass `struct in6_flowlabel_req{flr_dst=target, flr_label=label, flr_action=IPV6_FL_A_GET, flr_share=IPV6_FL_S_PROCESS, flr_flags=IPV6_FL_F_CREATE}` to `IPV6_FLOWLABEL_MGR`

An allocated label is kept until the socket is closed. The result (success or failure) is remembered per label.
Because `IPV6_FL_S_PROCESS` is used, `CAP_NET_ADMIN` is not required (`IPV6_FL_S_ANY` would require it). If the same label is used for a different target, it is reused on the same socket as already allocated.
If allocation fails (because of `net.ipv6.flowlabel_state_ranges` restrictions, a conflict with another process, and so on), the packet is sent without a label and a warning is logged only once.
Even on a socket with `IPV6_FLOWINFO_SEND` enabled, a request with label 0 is sent with `sin6_flowinfo = 0`, so it behaves as before (the kernel's automatic flow label).
`FlowLabel` on an IPv4 request is ignored. A value wider than 20 bits is `OutcomeError`.

## Echo data part

As specified by the contract (format of the Echo data part, revised 2026-09-27), the data part is **`8 + DataSize` bytes**.
Cisco defines "total packet size = IP header(20) + ICMP header(8) + 8 (internal timestamps) + request size" (CISCO-RTTMON-MIB `rttMonEchoAdminPktDataRequestSize`), counting 8 bytes outside `DataSize` (request-data-size). This implementation matches that.
The first 20 bytes are a custom header (including the part that corresponds to Cisco's 8 bytes of internal timestamps), and the rest is `Pattern` repeated (big-endian, truncated at the end).

| Offset | Length | Contents |
|---|---|---|
| 0 | 4 | Magic `0x49534C41` ("ISLA") |
| 4 | 4 | OpID (BE) |
| 8 | 4 | Seq (BE) |
| 12 | 8 | Send time. Unix nanoseconds (BE). For display and debugging; not used for RTT |
| 20 | 8 + DataSize − 20 | Pattern repeated |

- `DataSize` has the same meaning as Cisco's request-data-size: at least `MinDataSize` (28), and at most 65499 for IPv4 and 65519 for IPv6 (in both cases the limit at which the IP datagram fits in 65535 bytes). Out-of-range values are `OutcomeError`.
- The on-wire size is `36 + DataSize` bytes for IPv4 and `56 + DataSize` bytes for IPv6.

  | DataSize | ICMP data part | ICMP message | IPv4 packet | IPv6 packet |
  |---|---|---|---|---|
  | 28 (default) | 36 | 44 | 64 | 84 |
  | 1400 | 1408 | 1416 | 1436 | 1456 |
  | 65499 / 65519 (limit) | | | 65535 | 65575 |

- The implementation is `payloadOverhead` (8) and `payloadLen(dataSize) = 8 + dataSize` in `payload.go`. `buildPayload` takes `DataSize` and returns a data part of `payloadLen` bytes. Verify compares against the whole data part that was sent (`8 + DataSize` bytes).
- The Identifier in the ICMP header is allocated per socket from the OpID. It starts at `OpID & 0xFFFF`, and if that is in use, the next value is searched. The allocation is fixed per OpID and is released when the operation is deleted (the manager calls `Engine.Forget(opID)`; on reload removal and on ageout). Up to 65,536 operations can exist at the same time per socket. Sequence is the lower 16 bits of `Seq`.

## Matching

- The pending table is a per-socket `map[(kind, Identifier, Sequence)]*pending` (protected by a mutex). The kinds are Echo and Timestamp, so they do not collide even if they use the same (Identifier, Sequence). The value holds the OpID, Seq, target, the data part sent, Verify, the send time, and the reply channel (buffer 1). A Timestamp entry also holds a reference to its burst, the packet number, and the Originate sent.
- An entry has four states: "not sent (reserved only; not matched)", "waiting", "replied", and "expired".
- **Echo Reply**: A reply is treated as ours only when the magic, OpID, and Seq (32 bits) in the data part match an entry in the table. Anything that does not match (another process's ping, a different operation, an old attempt whose 16-bit Sequence has wrapped) is silently dropped and counted.
  - If `Verify` is true, the length and every byte of the data part are compared with what was sent, and a mismatch is `OutcomeVerifyError` (`Detail` holds the first mismatch position, or the length difference). If false, a header match alone gives `OutcomeReply`.
- **Destination Unreachable / Time Exceeded**: The embedded original datagram is parsed; if the ICMP type is Echo Request (or Timestamp Request; see below), the original destination matches the request's target, and (Identifier, Sequence) matches a waiting entry, the entry is completed with `OutcomeUnreachable`. If the embedded data contains the 20-byte custom header (Linux routers usually include it), the magic, OpID, and Seq are also checked. If only 8 bytes are embedded, as in RFC 792, the decision uses only ID / Sequence and the destination. For IPv6, extension headers in the embedded packet (Hop-by-Hop, Routing, Destination Options, the first fragment of Fragment) are skipped.
  - `Detail` is `"destination unreachable: <code description>, from <sender>"` or `"time exceeded: <code description>, from <sender>"`. The code description is the RFC 792 / 1812 / RFC 4443 name in lowercase (for example `host unreachable`, `address unreachable`, `ttl exceeded in transit`). A code without a name is `code N`.
  - `RTT` is 0, and `ReceivedAt` is the time the reception was read.
- **Timestamp Reply** is used for icmp-jitter matching (the "ICMP Timestamp (icmp-jitter)" section).
- Dropped packets are counted by internal counters (`foreign` / `malformed` / `late` / `other`), and per-packet logs are emitted only at debug level. The counters can be read with `Engine.Stats()` (including socket read failures, `RecvErrors`), and goipslad exposes them as the `goipsla_probe_*` metrics (`docs/metrics.md`).
- **Taking over a request of the same operation**: When an Echo is registered and a request with the same OpID is still waiting (or not yet sent) under the same key, the old request is finished with `ErrSuperseded` (`OutcomeError`) and replaced. The first attempt of a new life (restart, reset) reuses Seq from 1, so it would collide with an attempt of the cancelled old life still left in the table. The old attempt's ctx has been cancelled, so the manager discards its result. A reply to the old request has a different send time in its data part, so it is dropped as someone else's packet.

## Waiting, timeouts, and late replies

Flow of `Echo`:

1. Validate the request and choose the socket (opening it if needed for a VRF).
2. Reserve an entry in the pending table. If an entry with the same key is waiting, return `OutcomeError` (duplicate send of the same attempt). Replied or expired entries are replaced.
3. Take `Clock.Now()` as the send time `SentAt` and **start the timeout timer (`Clock.NewTimer(Timeout)`) at that moment**. Write the send time into the custom header, compute the checksum, make the entry matchable, then call `sendmsg`. If sending fails, remove the entry and return `OutcomeError`.
4. `select` on the reply channel, the timer, `ctx.Done()`, and `Close`.

**Send deadline**: The timeout is counted from `SentAt` (just before `sendmsg`). `sendmsg` is given the deadline `SentAt + Timeout`. If the send buffer is full and `EAGAIN` continues, `rawConn` waits with `poll(POLLOUT)` until the deadline (this is rare, so a thread is occupied during that time). If the packet cannot be sent by the deadline, sending is abandoned and the result is `OutcomeTimeout` (`Detail = "send timed out"`). If the send completes after the deadline, the timer started earlier has already fired, so the result is also `OutcomeTimeout`.

- On timeout, the entry becomes "expired" and stays in the table for `min(2 × Timeout, 60s)`. If a matching reply arrives during that time, `OnLateReply(opID, seq, sentAt)` is called and the reply is dropped. `sentAt` is the send time of the original request; the upper layer uses it to distinguish an attempt of a new life that reuses Seq from a late reply of the previous life.
- Replied entries (Reply / VerifyError / Unreachable) also stay for the same period, and a second Echo Reply (duplicate) calls `OnLateReply`. A second ICMP error is not counted.
- If `ctx` ends first, the entry is marked expired and the result is `OutcomeError` (`Err = ctx.Err()`). Later replies are treated as late.
- Expired and replied entries are cleaned up by a single periodic timer (1 second) for the whole engine.
- If the timer and a reply arrive at the same time, the decision is made under the socket's mutex, so if the reply was reflected in the table first, the reply is returned.
- The receive goroutine only parses, matches, and hands off to the channel. The channel has buffer 1, and if there is no receiver the value is dropped (by design this does not happen). `OnLateReply` is called from the receive goroutine outside the lock, so the callee must not do heavy work.

`Close` closes all sockets and waits for the receive goroutines and the cleanup goroutine to finish. Waiting and subsequent `Echo` calls return `OutcomeError` (`Err = ErrClosed`). A second `Close` does nothing.

## RTT measurement

- The send time `SentAt` is `Clock.Now()` just before `sendmsg` (with `clock.Real()`, it includes the monotonic reading).
- On the receive side, with `recvNow` being `Clock.Now()` right after reading the reception and `kernelRxWall` being the kernel timestamp,

  `RTT = (recvNow − SentAt) − (wall(recvNow) − kernelRxWall)`

  The first term is a monotonic difference, so it is not affected by wall clock steps. The second term corrects for the delay between the kernel receiving the packet and user space reading it, and is a wall clock difference over a short interval.
- **Handling wall clock steps**: If the correction (`wall(recvNow) − kernelRxWall`) is negative or exceeds 1 second (`maxRxLag`), the wall clock is considered to have stepped between the kernel timestamp and the read. In that case the correction is discarded, the uncorrected `recvNow − SentAt` (a monotonic difference) is used, and a debug log is emitted. Without this, RTT would stick at 0 when the clock moves forward and be too large when it moves back.
- If there is no timestamp, no correction is made. If RTT becomes negative, it is rounded to 0 with a debug log.
- `ReceivedAt = SentAt + RTT`.

## ICMP Timestamp (icmp-jitter)

`Engine.Jitter` runs one icmp-jitter burst. The contract types are in `jitter.go`. IPv4 only; an IPv6 target gives `JitterReply.Err = ErrUnsupportedFamily`.

- **Packet**: ICMP Timestamp Request (Type 13, Code 0, fixed 20 bytes, checksum computed by the engine). The Identifier uses the same per-OpID allocation as Echo. Sequence is a 16-bit counter per OpID and per socket, and keeps increasing across bursts. Originate is "ms since midnight UT" (`UnixMilli mod 86,400,000`) computed from the wall clock just before sending. Receive / Transmit are 0.
- **Send grid**: The scheduled send time is `S_i = S_0 + i × Interval`. A single `Clock` timer waits until the next scheduled time or the deadline of a waiting packet, whichever is earlier. Because scheduled times are kept on a grid, delays do not accumulate onto the next packet. A packet that is 1 Interval or more behind its scheduled time is not sent and becomes `OutcomeError` (`Err = ErrSkipped`; Cisco's Packet Skipped). If `Interval = 0`, all packets are sent back to back and none is skipped. A packet whose send fails is also `OutcomeError` (skipped).
- **Timeout**: Per packet, `Timeout` from its `SentAt`. As with Echo, the send is given the deadline `SentAt + Timeout`.
- **Reply (Type 14)**: The receive goroutine looks up the entry by (Timestamp, Identifier, Sequence) and checks that the reply's Originate matches the value sent. If it does not match, the reply is dropped as someone else's packet. RTT uses the same formula as Echo (corrected with the kernel timestamp, discarding the correction on a step), and is closed on the source's clock alone. Receive / Transmit are returned as raw 32-bit values; handling of the non-standard flag and wraparound is done by `internal/op`.
- **Destination Unreachable / Time Exceeded**: Matched on the (Identifier, Sequence) of the embedded Timestamp Request, and on Originate if the embedded data includes it, and that packet becomes `OutcomeUnreachable` (`Detail` has the same format as Echo).
- **Arrival order**: `ArrivalPos` is the arrival order (0-based) of replies that arrived within the deadline in the burst.
- **Late replies**: A reply that arrives after the packet's deadline but before the end of the burst gets `Late = true` (`Outcome` stays `OutcomeTimeout`). Second and later replies to the same packet are ignored. A reply that arrives after the end of the burst calls `OnLateReply(opID, the burst's Seq, that packet's send time)` (sequenceError in the upper layer).
- **Completion**: The burst ends when every packet has become a reply, Unreachable, timeout, or skipped. At the end, the entries' grace period `min(2 × Timeout, 60s)` is set and they are left to the cleanup timer (until then they are not removed even if expired).
- **Early termination**: On `ctx` cancellation or `Close`, packets not yet sent and packets waiting for a reply are returned as `OutcomeError` (`Err = ctx.Err()` / `ErrClosed`).
- `JitterReply.Err` is set when nothing could be sent (validation error, socket error, send failure of all packets).

## tools/probe-echo

```
probe-echo [flags] target [target...]
  --source ADDR  --interface NAME  --vrf NAME  --tos 0xB8  --flow-label 0x12345
  --size 28  --pattern 0xABCDABCD  --verify  --timeout 5s
  --count 1  --interval 1s  --op 1  --debug
```

It starts a goroutine per target and sends Echo concurrently through a shared engine. The OpID starts at `--op` and increases by 1 per target.
It prints the result of one attempt per line (`size` (request-data-size), `ip_len` (the on-wire IP packet length: 36 + size for IPv4, 56 + size for IPv6), `outcome`, `rtt_ms`, `detail`, `err`), and prints `late or duplicate reply` for late and duplicate replies. The exit code is 0 if every attempt is a reply, 1 otherwise.

With `--jitter`, it runs icmp-jitter bursts through `op.NewJitter` and prints `op.JitterResult` one item per line.

```
probe-echo --jitter [--interval 20ms] [--num-packets 10] [--timeout 2s] [--threshold 5s]
           [--count 1] [--gap 1s] [--packets] [--source ADDR] [--interface NAME] [--vrf NAME] [--tos 0xB8] target...
```

With `--jitter`, `--interval` is the packet interval (default 20ms), `--gap` is the interval between bursts, and `--packets` also prints per-packet results (RTT, O / R / T, arrival order, late). The exit code is 0 if every burst is ok.

Link it statically and run it in the source container of the staging environment:

```sh
make build   # builds bin/probe-echo together with bin/goipslad, bin/goipsla
docker compose -f staging/compose.yaml exec -T source /work/bin/probe-echo 10.100.1.11 fd00:100:2::11
```

## Tests

- `TestOnWireSize` / `TestBuildPayload` / `TestMaxDataSizeAccepted`: pin data part = 8 + DataSize, IPv4 = 36 + DataSize (64 bytes by default), IPv6 = 56 + DataSize, and the limits 65499 / 65519.
- `TestRTTCorrection` / `TestRTTWallClockStepThroughEngine`: the correction is discarded on wall clock steps (forward, backward, and the 1-second boundary). `TestTimeoutCountsFromSendTime` / `TestSendDeadlineExceeded`: the timeout is counted from the send time.
- `jitter_test.go`: sending on the grid and Originate, reversed arrival order, expiry and Late, `OnLateReply` after the burst, skipped and returning to the grid, Unreachable for Timestamp, mismatched Originate and a stray Echo Reply, coexistence of Echo and Timestamp with the same (Identifier, Sequence), ctx cancellation, Close, input validation. `burstParked` is used to synchronize the fake clock with the loop running concurrently.
- `go test -race ./internal/probe/...`: with a fake socket (a fake implementation of `packetConn`) and `clock.Fake`, verifies reply, timeout, late reply, duplicate, Unreachable (IPv4 / IPv6, 8 bytes embedded / whole packet embedded), someone else's reply, Verify failure, ctx cancellation, Close, IPv6 unavailable, lazy opening of VRF sockets, Identifier allocation, Sequence wraparound, and 1,000 concurrent requests.
- `FuzzParseReply`: `go test -run '^$' -fuzz FuzzParseReply ./internal/probe`
- Integration tests: run `go test -tags=integration ./internal/probe` in the source container (`make staging-integration-test`). They send Echo with 28 / 1400 bytes to `IPSLA_TARGET4` (default `10.100.1.11`) and `IPSLA_TARGET6` (default `fd00:100:1::11`) and check `OutcomeReply` and `0 < RTT < 1s`. `TestIntegrationJitter` sends a burst of 10 × 20ms to `IPSLA_TARGET4` and checks replies for all packets and the send intervals. Skipped without `CAP_NET_RAW`.

## Known limitations

- The send time is taken in user space just before `sendmsg`. The sender-side in-kernel delay is included (software send timestamps via `SO_TIMESTAMPING` are not implemented).
- If more than 65,536 OpIDs are used at the same time, Identifier allocation fails. The Identifier of a deleted operation is released by `Forget`.
- Once opened, a VRF socket is not closed until `Close`. For a VRF that could not be opened, the first failure is logged at warn, and for 30 seconds the same error is returned without reopening (negative cache). After that, it is reopened on request. When it opens, this is logged at info.
- Receive (`recvmsg`) failures are logged at warn the first time and then once per minute (`vrf`, `ipv6`, `err`, `count`), and recovery is logged at info.
- The icmp-jitter SD / DS jitter and the SD ordering check are computed with the non-standard flag (high bit) of Receive / Transmit cleared. RFC 792 non-standard values have arbitrary units and origin, so jitter is meaningless for targets that return non-standard times (only the one-way delay excludes non-standard values). Linux targets return standard values.
- DS jitter uses the difference of receive times, `ReceivedAt.UnixMilli()`. ReceivedAt is "the wall clock at send time + the monotonic RTT", so if the wall clock steps in the middle of a burst, that one sample is distorted.
- For an IPv4 ICMP error whose embedded data is only 8 bytes, the custom header cannot be checked. The decision uses only the match of the destination and (Identifier, Sequence).
- The icmp-jitter send time depends on the precision of the `Clock` timer. Even at 20ms intervals it drifts by tens to hundreds of µs, and because Originate / Receive are in ms, SD jitter shows quantization noise of about ±1ms even on the same host.
- Linux only.
