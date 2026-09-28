#!/bin/sh
# For the scale test: assign N IPv4 addresses in total to the 6 targets.
#
#   scripts/scale-addrs.sh add [N]   assign N addresses (default 1000). If already assigned, remove them first and assign again
#   scripts/scale-addrs.sh del       remove all of them and restore the routes (idempotent)
#   scripts/scale-addrs.sh list      print the assigned addresses and a targets snippet to paste into the goipslad configuration
#
# The free addresses within the segments alone are not enough for 1000, so each target k (1..6)
# uses a separate prefix 10.200.<k>.0/24. The addresses are assigned as /32 on the target's eth0, and
# routes are added on the source (and the router).
#   tgt-l1..l3 (k=1..3): on source, 10.200.k.0/24 via 10.100.1.1k
#   tgt-r1..r3 (k=4..6): on source, 10.200.k.0/24 via 10.100.1.254,
#                        on router, 10.200.k.0/24 via 10.100.2.1(k-3)
# Replies from the targets to the source return over the existing routes (same segment, or the default route toward the router).
set -eu
cd "$(dirname "$0")/.."

TARGETS="tgt-l1 tgt-l2 tgt-l3 tgt-r1 tgt-r2 tgt-r3"
MAX_PER_TARGET=254
FIRST_ID=1001

x() { svc="$1"; shift; docker compose exec -T "$svc" "$@"; }

# Service name and primary address of the k-th (1..6) target
svc_of()  { echo "$TARGETS" | cut -d' ' -f"$1"; }
addr_of() { if [ "$1" -le 3 ]; then echo "10.100.1.1$1"; else echo "10.100.2.1$(($1 - 3))"; fi; }

do_del() {
  for k in 1 2 3 4 5 6; do
    svc=$(svc_of "$k")
    # Remove all addresses in 10.200.k.0/24
    x "$svc" sh -c "ip -4 -o addr show dev eth0 | awk '\$4 ~ /^10\\.200\\.$k\\./ {print \"addr del \" \$4 \" dev eth0\"}' | ip -batch - 2>/dev/null || true"
    x source ip route del "10.200.$k.0/24" 2>/dev/null || true
    if [ "$k" -gt 3 ]; then
      x router ip route del "10.200.$k.0/24" 2>/dev/null || true
    fi
  done
  echo "scale-addrs: removed"
}

do_add() {
  n="$1"
  case "$n" in '' | *[!0-9]*) echo "scale-addrs: N must be a positive integer" >&2; exit 2 ;; esac
  if [ "$n" -lt 1 ] || [ "$n" -gt $((MAX_PER_TARGET * 6)) ]; then
    echo "scale-addrs: N must be between 1 and $((MAX_PER_TARGET * 6))" >&2
    exit 2
  fi
  do_del >/dev/null
  # If it fails partway, remove what was assigned
  trap 'echo "scale-addrs: failed; rolling back" >&2; do_del >/dev/null' EXIT
  base=$((n / 6)) extra=$((n % 6))
  for k in 1 2 3 4 5 6; do
    cnt=$base
    [ "$k" -le "$extra" ] && cnt=$((cnt + 1))
    [ "$cnt" -eq 0 ] && continue
    svc=$(svc_of "$k")
    i=1
    while [ "$i" -le "$cnt" ]; do
      echo "addr add 10.200.$k.$i/32 dev eth0"
      i=$((i + 1))
    done | x "$svc" ip -batch -
    if [ "$k" -le 3 ]; then
      x source ip route replace "10.200.$k.0/24" via "$(addr_of "$k")"
    else
      x source ip route replace "10.200.$k.0/24" via 10.100.1.254
      x router ip route replace "10.200.$k.0/24" via "$(addr_of "$k")"
    fi
  done
  trap - EXIT
  echo "scale-addrs: added $n addresses"
}

do_list() {
  all=""
  for k in 1 2 3 4 5 6; do
    svc=$(svc_of "$k")
    addrs=$(x "$svc" sh -c "ip -4 -o addr show dev eth0 | awk '\$4 ~ /^10\\.200\\.$k\\./ {sub(\"/.*\", \"\", \$4); print \$4}'" | sort -t. -k4,4n)
    printf '# %s (%s): %s addresses\n' "$svc" "$(addr_of "$k")" "$(printf '%s' "$addrs" | grep -c . || true)"
    all="$all $addrs"
  done
  id=$FIRST_ID
  printf 'targets: {'
  sep=""
  for a in $all; do
    printf '%s %s: %s' "$sep" "$id" "$a"
    sep=","
    id=$((id + 1))
  done
  printf ' }\n'
}

case "${1:-}" in
  add) do_add "${2:-1000}" ;;
  del) do_del ;;
  list) do_list ;;
  *) echo "usage: $0 add [N] | del | list" >&2; exit 2 ;;
esac
