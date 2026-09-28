#!/bin/sh
# P6 integration check: checks the goipslad Prometheus exporter (/metrics) in the staging environment.
#
#   staging/scripts/p6-verify.sh [--no-scale]
#
# 1. Start goipslad with staging/goipslad.yaml (metrics-listen 0.0.0.0:9818, 12 icmp-echo and 2 icmp-jitter operations)
#    and fetch /metrics from inside the source container (there is no curl / wget, so
#    tools/metrics-get is used).
# 2. goipsla_latest_rtt_seconds exists for the 12 icmp-echo operations, and results_total{result="ok"} increases.
# 3. With tgt-l3 stopped, results_total{result="timeout"} for id 13 / 113 increases, and
#    goipsla_latest_success becomes 0.
# 4. Create a configuration with 1,000 targets using scale-addrs.sh add 1000, and measure the /metrics
#    fetch time with 1,000 operations (skipped with --no-scale).
#
# Takes about 2 minutes. It checks the state before the run (whether goipslad is running, whether tgt-l3 is running, whether scale-test addresses exist)
# and on exit (including interruption) restores only what it changed itself (staging/scripts/verify-lib.sh).
# If goipslad is already running or scale-test addresses are already assigned, it aborts without changing anything.
# It does not recreate containers.
set -u
repo=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$repo"

SCALE=1
[ "${1:-}" = "--no-scale" ] && SCALE=0

RUN=/work/staging/run
LOG=$RUN/goipslad-p6.log
CONFIG=/work/staging/goipslad.yaml
SCALE_CONFIG=$RUN/p6-scale.yaml
URL=http://127.0.0.1:9818/metrics
SCRAPE_LIMIT_MS=1000 # allowance for 1,000 operations in the staging environment (the unit benchmark target is 100 ms)

fail=0
. "$repo/staging/scripts/verify-lib.sh"

# metrics: print the /metrics body to standard output
metrics() { src /work/bin/metrics-get -url "$URL" 2>/dev/null; }
# value <body> <series prefix (metric_name{labels...)>: value of the first matching series
value() { printf '%s\n' "$1" | grep -F "$2" | head -n 1 | awk '{print $NF}'; }
# sum_ok <body>: sum of results_total{result="ok"} over all operations
sum_ok() { printf '%s\n' "$1" | grep '^goipsla_results_total{' | grep 'result="ok"' | awk '{s += $NF} END {print s + 0}'; }
# count <body> <metric name> [additional grep condition]: number of series
count() { printf '%s\n' "$1" | grep "^$2{" | grep -c -- "${3:-}" || true; }
gt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 > b + 0) }'; }

# Whether scale-test addresses (10.200.k.0/24) are assigned
scale_added=0

# serve <config>: start goipslad and wait until /metrics can be fetched
serve() {
  start_daemon "$1" "$LOG"
  i=0
  until metrics >/dev/null 2>&1 || [ $i -ge 30 ]; do sleep 0.5; i=$((i + 1)); done
  if metrics >/dev/null 2>&1; then
    ok "goipslad (pid $own_pid) serves $URL ($1)"
  else
    ng "goipslad serves $URL ($1)"
    src tail -n 20 "$LOG" | indent
    exit 1
  fi
}

cleanup() {
  restore_env
  if [ "$scale_added" -eq 1 ]; then
    staging/scripts/scale-addrs.sh del >/dev/null 2>&1
  fi
  src rm -f "$SCALE_CONFIG" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

echo "== 1. build and start goipslad with the exporter =="
preflight_daemon
if [ "$SCALE" -eq 1 ] && scale_present; then
  echo "p6-verify.sh: scale test addresses (10.200.k.0/24) are already configured; run 'staging/scripts/scale-addrs.sh del' first" >&2
  exit 2
fi
if make build >/dev/null; then
  ok "make build, bin/metrics-get"
else
  ng "build"; exit 1
fi
if grep -q '^  metrics-listen: 0.0.0.0:9818' staging/goipslad.yaml; then
  ok "staging/goipslad.yaml has metrics-listen 0.0.0.0:9818"
else
  ng "metrics-listen 0.0.0.0:9818 missing in staging/goipslad.yaml"
fi
src mkdir -p "$RUN"
serve "$CONFIG"

echo "== 2. 14 operations: latest RTT and results_total{result=\"ok\"} grows =="
sleep 15
m1=$(metrics)
n=$(count "$m1" goipsla_operation_info)
[ "$n" -eq 14 ] && ok "14 operations exported (goipsla_operation_info)" || ng "operations exported: $n (want 14)"
n=$(count "$m1" goipsla_latest_rtt_seconds 'type="icmp-echo"')
[ "$n" -eq 12 ] && ok "12 icmp-echo goipsla_latest_rtt_seconds series" || ng "icmp-echo goipsla_latest_rtt_seconds series: $n (want 12)"
n=$(count "$m1" goipsla_latest_rtt_seconds 'type="icmp-jitter"')
[ "$n" -eq 2 ] && ok "2 icmp-jitter goipsla_latest_rtt_seconds series" || ng "icmp-jitter goipsla_latest_rtt_seconds series: $n (want 2)"
printf '%s\n' "$m1" | grep '^goipsla_latest_rtt_seconds{' | indent
ok1=$(sum_ok "$m1")
sleep 10
m2=$(metrics)
ok2=$(sum_ok "$m2")
gt "$ok2" "$ok1" && ok "results_total{result=\"ok\"} grew: $ok1 -> $ok2" || ng "results_total{result=\"ok\"} did not grow: $ok1 -> $ok2"
if printf '%s\n' "$m2" | grep -q '^go_goroutines '; then ok "go_* runtime metrics present"; else ng "go_* runtime metrics missing"; fi
printf '%s\n' "$m2" | grep -E '^goipsla_(operations|scrape_duration_seconds)' | indent

echo "== 3. stop tgt-l3: id 13 / 113 time out =="
t13=$(value "$m2" 'goipsla_results_total{id="13",result="timeout"')
t113=$(value "$m2" 'goipsla_results_total{id="113",result="timeout"')
service_stop tgt-l3
sleep 20
m3=$(metrics)
for id in 13 113; do
  before=$t13; [ "$id" = 113 ] && before=$t113
  after=$(value "$m3" "goipsla_results_total{id=\"$id\",result=\"timeout\"")
  gt "$after" "$before" && ok "id $id: results_total{result=\"timeout\"} grew: $before -> $after" || ng "id $id: timeout did not grow: $before -> $after"
  s=$(value "$m3" "goipsla_latest_success{id=\"$id\",")
  [ "$s" = 0 ] && ok "id $id: goipsla_latest_success 0" || ng "id $id: goipsla_latest_success $s (want 0)"
  rc=$(printf '%s\n' "$m3" | grep "^goipsla_latest_return_code{id=\"$id\"," | head -n 1)
  printf '%s\n' "$rc" | grep -q 'rc="timeout"' && ok "id $id: latest_return_code rc=timeout" || ng "id $id: $rc"
  printf '%s\n' "$m3" | grep -q "^goipsla_latest_rtt_seconds{id=\"$id\"," && ng "id $id: latest_rtt_seconds still exported" || ok "id $id: no latest_rtt_seconds"
done
s11=$(value "$m3" 'goipsla_latest_success{id="11",')
[ "$s11" = 1 ] && ok "id 11 (tgt-l1) still latest_success 1" || ng "id 11: latest_success $s11"
service_restore tgt-l3

if [ "$SCALE" -eq 1 ]; then
  echo "== 4. 1,000 operations: scrape time =="
  stop_own_daemon
  if staging/scripts/scale-addrs.sh add 1000 >/dev/null; then
    scale_added=1
    ok "scale-addrs.sh add 1000"
  else
    ng "scale-addrs.sh add 1000"
  fi
  targets=$(staging/scripts/scale-addrs.sh list | grep '^targets:')
  src sh -c 'cat >"$0"' "$SCALE_CONFIG" <<EOF
# generated by staging/scripts/p6-verify.sh
global:
  api-socket: /work/staging/run/goipslad.sock
  metrics-listen: 0.0.0.0:9818
  log: { format: text, level: warn }
templates:
  scale:
    type: icmp-echo
    frequency: 10s
    timeout: 1000ms
    threshold: 100ms
    tag: scale
operations:
  - template: scale
    $targets
EOF
  serve "$SCALE_CONFIG"
  sleep 25 # every operation with a 10 s frequency makes at least one attempt
  m4=$(metrics)
  n=$(count "$m4" goipsla_operation_info)
  [ "$n" -eq 1000 ] && ok "1000 operations exported" || ng "operations exported: $n (want 1000)"
  n=$(count "$m4" goipsla_latest_rtt_seconds)
  [ "$n" -ge 990 ] && ok "$n / 1000 with latest RTT" || ng "only $n / 1000 with latest RTT"
  printf '       /metrics body: %s bytes, %s lines\n' "$(printf '%s\n' "$m4" | wc -c)" "$(printf '%s\n' "$m4" | wc -l)"
  line=$(src /work/bin/metrics-get -url "$URL" -n 10 -q 2>&1 >/dev/null)
  echo "       $line"
  ms=$(printf '%s\n' "$line" | sed -n 's/.* avg_ms=\([0-9.]*\) .*/\1/p')
  if [ -n "$ms" ] && ! gt "$ms" "$SCRAPE_LIMIT_MS"; then ok "1000 operations: average scrape ${ms} ms (< ${SCRAPE_LIMIT_MS} ms)"; else ng "1000 operations: average scrape ${ms:-?} ms"; fi
  printf '%s\n' "$m4" | grep '^goipsla_scrape_duration_seconds' | indent
  pid=$own_pid
  stop_own_daemon
  own_daemon_gone "$pid" && ok "goipslad (pid $pid) exited" || ng "goipslad (pid $pid) still running"
  staging/scripts/scale-addrs.sh del >/dev/null && scale_added=0
  if scale_present; then ng "scale-addrs.sh del: addresses remain"; else ok "scale-addrs.sh del: scale addresses removed"; fi
fi

echo "== 5. environment restored =="
pid=$own_pid
stop_own_daemon
[ -z "$pid" ] || { own_daemon_gone "$pid" && ok "goipslad (pid $pid) exited" || ng "goipslad (pid $pid) still running"; }
[ -n "$(dc ps --status running -q tgt-l3)" ] && ok "tgt-l3 running" || ng "tgt-l3 not running"
src ping -4 -c 1 -W 2 -q 10.100.1.13 >/dev/null 2>&1 && ok "tgt-l3 reachable" || ng "tgt-l3 unreachable"

echo
[ "$fail" -eq 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"
