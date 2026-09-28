#!/bin/sh
# Reachability check for the staging environment: checks ICMP Echo / routes / ICMP Timestamp from the source to all targets
set -u
cd "$(dirname "$0")/.."

LOCAL4="10.100.1.11 10.100.1.12 10.100.1.13"
REMOTE4="10.100.2.11 10.100.2.12 10.100.2.13"
LOCAL6="fd00:100:1::11 fd00:100:1::12 fd00:100:1::13"
REMOTE6="fd00:100:2::11 fd00:100:2::12 fd00:100:2::13"
ROUTER4=10.100.1.254
ROUTER6=fd00:100:1::254

fail=0
src() { docker compose exec -T source "$@"; }
ok()  { printf '  OK   %s\n' "$1"; }
ng()  { printf '  NG   %s\n' "$1"; fail=1; }

# Check the expected hop count and the intermediate router with traceroute
check_path() {
  fam="$1" dst="$2" hops="$3" router="$4"
  out=$(src traceroute "$fam" -n -I -q 1 -w 1 -m 5 "$dst" 2>/dev/null | tail -n +2)
  n=$(printf '%s\n' "$out" | grep -c .)
  first=$(printf '%s\n' "$out" | awk 'NR==1{print $2}')
  if [ "$n" -ne "$hops" ]; then ng "$dst: $n hops (expected $hops)"; return; fi
  if [ "$hops" -eq 2 ] && [ "$first" != "$router" ]; then ng "$dst: 1st hop $first (expected $router)"; return; fi
  ok "$dst: $hops hop(s) $( [ "$hops" -eq 2 ] && echo "via $router")"
}

echo "== ICMP Echo (IPv4/IPv6) =="
for d in $LOCAL4 $REMOTE4; do src ping -4 -c 2 -W 1 -q "$d" >/dev/null 2>&1 && ok "$d" || ng "$d"; done
for d in $LOCAL6 $REMOTE6; do src ping -6 -c 2 -W 1 -q "$d" >/dev/null 2>&1 && ok "$d" || ng "$d"; done

echo "== Path (traceroute -I) =="
for d in $LOCAL4;  do check_path -4 "$d" 1 "$ROUTER4"; done
for d in $REMOTE4; do check_path -4 "$d" 2 "$ROUTER4"; done
for d in $LOCAL6;  do check_path -6 "$d" 1 "$ROUTER6"; done
for d in $REMOTE6; do check_path -6 "$d" 2 "$ROUTER6"; done

echo "== ICMP Timestamp Request/Reply (for icmp-jitter, IPv4) =="
for d in $LOCAL4 $REMOTE4; do
  if src hping3 --icmp --icmp-ts -c 1 "$d" 2>&1 | grep -q 'ICMP timestamp'; then ok "$d"; else ng "$d"; fi
done

echo
[ "$fail" -eq 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"
