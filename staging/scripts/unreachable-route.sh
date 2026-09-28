#!/bin/sh
# For the unreachable test: set up a state where the router returns Destination Unreachable
# for Echo from the source to 192.0.2.0/24 and 2001:db8::/32.
#
#   scripts/unreachable-route.sh add   add the routes (replace them if they already exist)
#   scripts/unreachable-route.sh del   delete the routes (idempotent)
#
#   source: 192.0.2.0/24 via 10.100.1.254, 2001:db8::/32 via fd00:100:1::254
#   router: unreachable 192.0.2.0/24, unreachable 2001:db8::/32
set -eu
cd "$(dirname "$0")/.."

x() { svc="$1"; shift; docker compose exec -T "$svc" "$@"; }

do_del() {
  x source sh -c "ip route del 192.0.2.0/24 2>/dev/null || true; ip -6 route del 2001:db8::/32 2>/dev/null || true"
  x router sh -c "ip route del unreachable 192.0.2.0/24 2>/dev/null || true; ip -6 route del unreachable 2001:db8::/32 2>/dev/null || true"
  echo "unreachable-route: removed"
}

do_add() {
  trap 'echo "unreachable-route: failed; rolling back" >&2; do_del >/dev/null' EXIT
  x router sh -eu -c "ip route replace unreachable 192.0.2.0/24; ip -6 route replace unreachable 2001:db8::/32"
  x source sh -eu -c "ip route replace 192.0.2.0/24 via 10.100.1.254; ip -6 route replace 2001:db8::/32 via fd00:100:1::254"
  trap - EXIT
  echo "unreachable-route: added (192.0.2.0/24, 2001:db8::/32 are unreachable at the router)"
}

case "${1:-}" in
  add) do_add ;;
  del) do_del ;;
  *) echo "usage: $0 add | del" >&2; exit 2 ;;
esac
