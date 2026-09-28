#!/bin/sh
# For the VRF test: create VRF vrf-blue (table 100) on the source.
#
#   scripts/vrf.sh up     create it (recreate it if it already exists)
#   scripts/vrf.sh down   delete it (idempotent)
#   scripts/vrf.sh show   show the state
#
# eth0 is not put into the VRF (doing so would cut all traffic in the default VRF and break tests running in parallel).
# Instead, a macvlan mv-blue is created on top of eth0 and put into vrf-blue, with separate addresses on seg-local
# 10.100.1.100/24 and fd00:100:1::100/64. Traffic of sockets bound to the VRF
# goes through mv-blue and reaches the targets via the routes in table 100.
#   table 100: 10.100.1.0/24 dev mv-blue (connected route), 10.100.2.0/24 via 10.100.1.254
#              fd00:100:1::/64 dev mv-blue (connected route), fd00:100:2::/64 via fd00:100:1::254
set -eu
cd "$(dirname "$0")/.."

VRF=vrf-blue
TABLE=100
DEV=mv-blue
ADDR4=10.100.1.100/24
ADDR6=fd00:100:1::100/64

src() { docker compose exec -T source "$@"; }

do_down() {
  src sh -c "ip link del $DEV 2>/dev/null || true; ip link del $VRF 2>/dev/null || true"
  # Also delete the mv-blue neighbor entries left on peers on seg-local (they would expire anyway if left alone)
  for svc in router tgt-l1 tgt-l2 tgt-l3; do
    docker compose exec -T "$svc" sh -c "ip neigh del ${ADDR4%/*} dev eth0 2>/dev/null; ip -6 neigh del ${ADDR6%/*} dev eth0 2>/dev/null; true"
  done
  echo "vrf: removed"
}

do_up() {
  do_down >/dev/null
  trap 'echo "vrf: failed; rolling back" >&2; do_down >/dev/null' EXIT
  src sh -eu -c "
    ip link add $VRF type vrf table $TABLE
    ip link set $VRF up
    ip link add $DEV link eth0 type macvlan mode bridge
    ip link set $DEV master $VRF
    ip addr add $ADDR4 dev $DEV
    ip addr add $ADDR6 dev $DEV nodad
    ip link set $DEV up
    ip route replace 10.100.2.0/24 via 10.100.1.254 dev $DEV table $TABLE
    ip -6 route replace fd00:100:2::/64 via fd00:100:1::254 dev $DEV table $TABLE
  "
  trap - EXIT
  echo "vrf: $VRF (table $TABLE) is up with $DEV $ADDR4 $ADDR6"
}

do_show() {
  src sh -c "ip -br link show master $VRF 2>/dev/null; ip -br addr show dev $DEV 2>/dev/null; echo '-- table $TABLE'; ip route show table $TABLE 2>/dev/null; ip -6 route show table $TABLE 2>/dev/null" || true
}

case "${1:-}" in
  up) do_up ;;
  down) do_down ;;
  show) do_show ;;
  *) echo "usage: $0 up | down | show" >&2; exit 2 ;;
esac
