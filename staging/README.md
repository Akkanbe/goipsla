# IP SLA (ICMP) staging environment

Docker Compose brings up one IP SLA source, one router, and six targets.
Three targets sit on the same L2 segment as the source, and three sit on a separate segment behind the router.

```
                    seg-local (10.100.1.0/24, fd00:100:1::/64)
  source .10 ──────┬──────────┬──────────┬──────────┐
                tgt-l1 .11 tgt-l2 .12 tgt-l3 .13  router .254
                                                     │ (IPv4/IPv6 forwarding)
                    seg-remote (10.100.2.0/24, fd00:100:2::/64)
                ┌──────────┬──────────┬──────────────┘ .254
             tgt-r1 .11 tgt-r2 .12 tgt-r3 .13
```

| Container | IPv4 | IPv6 | Role |
|---|---|---|---|
| ipsla-source | 10.100.1.10 | fd00:100:1::10 | IP SLA source. Mounts the repository at `/work` |
| ipsla-router | 10.100.1.254 / 10.100.2.254 | fd00:100:1::254 / fd00:100:2::254 | Router between the segments |
| ipsla-tgt-l1 to l3 | 10.100.1.11 to 13 | fd00:100:1::11 to 13 | Targets on the same L2 segment (1 hop) |
| ipsla-tgt-r1 to r3 | 10.100.2.11 to 13 | fd00:100:2::11 to 13 | Targets behind the router (2 hops) |

- `.1` / `::1` on each segment is the gateway on the Docker host side and is not used on the IP SLA paths.
- The source routes only `10.100.2.0/24` and `fd00:100:2::/64` through the router. The remote targets point their default route at the router.
- ICMP rate limiting is disabled on all nodes, so path-echo Time Exceeded messages and consecutive icmp-jitter replies are not thinned out.
- On the targets, the Linux kernel answers ICMP Echo and ICMP Timestamp (Type 13/14). No responder software is needed.

## Usage

```bash
docker compose up -d --build
```

```bash
./scripts/verify.sh
```

`verify.sh` checks Echo (IPv4/IPv6) from the source to all targets, the traceroute hop count and the intermediate router, and ICMP Timestamp replies.

To work on the source, open a shell as follows.

```bash
docker compose exec source bash
```

To stop and remove the environment:

```bash
docker compose down
```

## Injecting delay, jitter, and loss

All nodes have `NET_ADMIN`, so `tc netem` can change the link quality.
For example, add 30 ms ± 5 ms of delay and 1% loss to the replies from tgt-r1:

```bash
docker compose exec tgt-r1 tc qdisc add dev eth0 root netem delay 30ms 5ms loss 1%
```

To remove it:

```bash
docker compose exec tgt-r1 tc qdisc del dev eth0 root
```

Applying it to one of the router's interfaces affects the whole remote segment. Check the router's interface names with `docker compose exec router ip -br addr`.

## Helper scripts (scale, VRF, and unreachable tests)

These scripts make temporary changes to running containers with `docker compose exec`. They do not change `compose.yaml`, so no container is recreated.
The changes disappear when a container restarts. For all three, the undo operation (`del` / `down`) can be run any number of times. If a script fails partway, it automatically removes what it added.

### Scale test: `scripts/scale-addrs.sh`

```bash
./scripts/scale-addrs.sh add [N]   # add N IPv4 addresses in total (default 1000, max 1524) to the 6 targets
./scripts/scale-addrs.sh list      # print the assigned addresses and a targets snippet to paste into the goipslad configuration
./scripts/scale-addrs.sh del       # remove all of them and restore the routes
```

The free addresses within the segments (about 230 per segment) are not enough for 1000 addresses. The script therefore uses a separate prefix for each target.

| Target | Addresses added (/32 on eth0) | Routes added |
|---|---|---|
| tgt-l1 to l3 | `10.200.1.0/24` to `10.200.3.0/24` | source: `10.200.k.0/24 via 10.100.1.1k` |
| tgt-r1 to r3 | `10.200.4.0/24` to `10.200.6.0/24` | source: `10.200.k.0/24 via 10.100.1.254`, router: `10.200.k.0/24 via 10.100.2.1(k-3)` |

The N addresses are split evenly across the 6 targets, starting from `.1` of each prefix. Replies from the targets to the source return over the existing routes.
The `targets: { 1001: 10.200.1.1, ... }` printed by `list` is numbered consecutively from ID 1001. Combined with a template, it can be pasted into the configuration as is.

```yaml
templates: { scale: { type: icmp-echo, frequency: 10s, timeout: 1000ms } }
operations:
  - template: scale
    targets: { 1001: 10.200.1.1, 1002: 10.200.1.2, ... }
```

### VRF test: `scripts/vrf.sh`

```bash
./scripts/vrf.sh up     # create vrf-blue (table 100) on the source
./scripts/vrf.sh show   # show the VRF state and the routes in table 100
./scripts/vrf.sh down   # undo
```

The source's `eth0` is not put into the VRF. Instead, a macvlan `mv-blue` is created on top of `eth0` and put into `vrf-blue`.

- `mv-blue` gets separate addresses on seg-local: `10.100.1.100/24` and `fd00:100:1::100/64`.
- Table 100 holds the connected routes plus `10.100.2.0/24 via 10.100.1.254` and `fd00:100:2::/64 via fd00:100:1::254`.
- Sockets bound to the VRF (`probe-echo --vrf vrf-blue`, or `vrf: vrf-blue` in goipslad) send and receive through `mv-blue`. Targets see them as source address `10.100.1.100`.

Compared with putting `eth0` into the VRF (enslaving it):

| Approach | Advantages | Disadvantages |
|---|---|---|
| Enslave `eth0` | The source address is the usual one (10.100.1.10) | All traffic in the default VRF is cut. Tests run through `docker compose exec` and a goipslad running in parallel are affected. If you forget to undo it, the environment stays broken |
| Enslave a macvlan (chosen) | Traffic in the default VRF is unaffected. Measurements inside and outside the VRF can run at the same time. `down` only deletes 2 links | The source address is different (10.100.1.100). Peers on seg-local get a neighbor entry for mv-blue (removed by `down`) |

After `vrf-blue` is created, operations configured with `vrf: vrf-blue` in goipslad measure through this VRF.

### Unreachable test: `scripts/unreachable-route.sh`

```bash
./scripts/unreachable-route.sh add   # make 192.0.2.0/24 and 2001:db8::/32 unreachable at the router
./scripts/unreachable-route.sh del   # undo
```

- Source: adds `192.0.2.0/24 via 10.100.1.254` and `2001:db8::/32 via fd00:100:1::254`.
- Router: adds `unreachable 192.0.2.0/24` and `unreachable 2001:db8::/32`.

When the source sends Echo to `192.0.2.1` or `2001:db8::1`, the router (`10.100.1.254` / `fd00:100:1::254`) returns Destination Unreachable.

```console
$ docker compose exec source /work/bin/probe-echo 192.0.2.1 2001:db8::1
target=192.0.2.1 ... outcome=unreachable detail="destination unreachable: host unreachable, from 10.100.1.254"
target=2001:db8::1 ... outcome=unreachable detail="destination unreachable: no route to destination, from fd00:100:1::254"
```
