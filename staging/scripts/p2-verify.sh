#!/bin/sh
# P2 integration check: run goipslad in the source container and check statistics, history, and enhanced history with goipsla show.
#
#   staging/scripts/p2-verify.sh
#
# Takes about 2 to 3 minutes. Along the way it applies netem to tgt-r1 / tgt-r2 and stops tgt-l3.
# It checks and saves the state before the run (qdisc, whether tgt-l3 is running, whether goipslad is running), and on exit (including interruption)
# restores only what it changed itself (staging/scripts/verify-lib.sh). If goipslad is already running
# or netem is already applied, it aborts without changing anything. It does not recreate containers.
# JSON is checked with jq on the host.
set -u
repo=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$repo"

SOCK=/work/staging/run/goipslad.sock
LOG=/work/staging/run/goipslad.log
CONFIG=/work/staging/goipslad.yaml

fail=0
. "$repo/staging/scripts/verify-lib.sh"
g()   { src /work/bin/goipsla --socket "$SOCK" "$@"; }
gj()  { g "$@" -o json; }
# Evaluate a boolean with jq: jqtrue '<JSON>' '<expression>'

trap restore_env EXIT
trap 'exit 130' INT TERM

command -v jq >/dev/null 2>&1 || { echo "p2-verify: jq is required on the host" >&2; exit 2; }

echo "== 1. build and start goipslad =="
preflight_daemon
preflight_qdisc tgt-r1 tgt-r2
if make build >/dev/null; then ok "make build"; else ng "make build"; exit 1; fi
src mkdir -p /work/staging/run
start_daemon "$CONFIG" "$LOG"
i=0
until g health >/dev/null 2>&1 || [ $i -ge 20 ]; do sleep 0.5; i=$((i + 1)); done
check "goipslad answers on $SOCK" g health
g health | indent

echo "== 2. 30 s later: 14 operations (12 icmp-echo, 2 icmp-jitter) active and ok =="
sleep 30
ops=$(gj show operations)
g show operations | indent
check "14 operations (12 icmp-echo, 2 icmp-jitter)" jqtrue "$ops" 'length == 14 and ([.[] | select(.type == "icmp-jitter") | .id] == [31, 32])'
check "all active" jqtrue "$ops" 'all(.[]; .state == "active")'
check "all latest rc=ok" jqtrue "$ops" 'all(.[]; .latest.code == "ok" and .latest.rtt_ms != null)'

echo "== 3. tgt-r1 +150ms: 21 / 121 over threshold =="
before=$(gj show statistics aggregated 21 --details)
netem_add tgt-r1 delay 150ms
sleep 30
after=$(gj show statistics aggregated 21 --details)
hist=$(gj show history 21)
after121=$(gj show statistics 121)
overth=$(gj show operations --rc overThreshold)
g show statistics 21 | indent
g show statistics aggregated 21 --details | indent
netem_del tgt-r1
b_over=$(printf '%s' "$before" | jq '.hours[-1].counters.over_thresholds')
b_last=$(printf '%s' "$before" | jq '.hours[-1].dist[-1].completions')
check "21 latest rc=overThreshold" jqtrue "$after" '.latest.code == "overThreshold" and .latest.rtt_ms >= 150'
check "21 aggregated OVERTH increased ($b_over -> $(printf '%s' "$after" | jq '.hours[-1].counters.over_thresholds'))" \
  jqtrue "$after" ".hours[-1].counters.over_thresholds >= $b_over + 4"
check "21 last distribution bucket (>=40ms) increased" \
  jqtrue "$after" ".hours[-1].dist[-1].lower_ms == 40 and .hours[-1].dist[-1].upper_ms == null and .hours[-1].dist[-1].completions >= $b_last + 4"
# The history RTT of an overThreshold sample is 0: the history records the
# RTT for ok only ("0 for anything other than ok"; see docs/statistics.md), so only the sense is checked here.
check "21 history has overThreshold rows" jqtrue "$hist" 'any(.history[]; .code == "overThreshold")'
check "121 latest rc=overThreshold" jqtrue "$after121" '.latest.code == "overThreshold"'
# 32 is an icmp-jitter operation to the same target; only icmp-echo is checked here.
check "only 21 and 121 (icmp-echo) are over threshold (--rc overThreshold)" jqtrue "$overth" '[.[] | select(.type == "icmp-echo") | .id] == [21, 121]'

echo "== 4. tgt-l3 stopped: 13 / 113 time out =="
before=$(gj show statistics aggregated 13)
service_stop tgt-l3
sleep 30
after=$(gj show statistics aggregated 13)
hist=$(gj show history 13)
after113=$(gj show statistics 113) # read before tgt-l3 comes back
g show statistics 13 | indent
g show history 13 | tail -n 4 | indent
service_restore tgt-l3
b_to=$(printf '%s' "$before" | jq '.hours[-1].counters.timeouts')
check "13 latest rc=timeout" jqtrue "$after" '.latest.code == "timeout" and .latest.rtt_ms == null'
check "13 aggregated TIMEOUT increased ($b_to -> $(printf '%s' "$after" | jq '.hours[-1].counters.timeouts'))" \
  jqtrue "$after" ".hours[-1].counters.timeouts >= $b_to + 4"
check "13 history has timeout rows with RTT 0" jqtrue "$hist" 'any(.history[]; .code == "timeout" and .rtt_ms == 0)'
check "113 latest rc=timeout" jqtrue "$after113" '.latest.code == "timeout"'

echo "== 5. tgt-r2 +2500ms (> timeout 2000ms): 22 late replies =="
before=$(gj show statistics aggregated 22 --details)
netem_add tgt-r2 delay 2500ms
sleep 20
netem_del tgt-r2
sleep 3 # replies already in flight arrive late
after=$(gj show statistics aggregated 22 --details)
g show statistics aggregated 22 | indent
b_seq=$(printf '%s' "$before" | jq '.hours[-1].counters.sequence_errors')
b_to=$(printf '%s' "$before" | jq '.hours[-1].counters.timeouts')
check "22 aggregated SEQERR increased ($b_seq -> $(printf '%s' "$after" | jq '.hours[-1].counters.sequence_errors'))" \
  jqtrue "$after" ".hours[-1].counters.sequence_errors >= $b_seq + 2"
check "22 aggregated TIMEOUT increased ($b_to -> $(printf '%s' "$after" | jq '.hours[-1].counters.timeouts'))" \
  jqtrue "$after" ".hours[-1].counters.timeouts >= $b_to + 3"
check "122 aggregated SEQERR increased" jqtrue "$(gj show statistics aggregated 122)" '.hours[-1].counters.sequence_errors >= 2'

echo "== 6. enhanced history of 21: 60 s buckets =="
enh=$(gj show enhanced-history 21)
g show enhanced-history 21 | indent
check "at least 2 buckets" jqtrue "$enh" '(.enhanced | length) >= 2'
check "indexes 1, 2, ... and starts 60 s apart" jqtrue "$enh" '
  .enhanced as $e | [range(0; $e | length)] | all(. as $i | $e[$i].index == $i + 1) and
  ([range(1; $e | length)] | all(. as $i |
     (($e[$i].start | sub("\\.[0-9]+"; "") | fromdate) - ($e[$i - 1].start | sub("\\.[0-9]+"; "") | fromdate)) == 60))'

echo "== 7. -o json matches the table =="
# The table and the JSON are read at slightly different times; retry if an
# attempt completed in between.
same=false
for _ in 1 2 3; do
  # Find SUCC by its heading; LAST ("3s ago") is two words in a data row, so
  # the data field is one to the right of the heading's position.
  tsucc=$(g show operations | awk 'NR == 1 { for (i = 1; i <= NF; i++) if ($i == "SUCC") c = i } $1 == 21 { print $(c + 1) }')
  js=$(gj show statistics 21)
  jsucc=$(printf '%s' "$js" | jq '.totals.successes')
  [ "$tsucc" = "$jsucc" ] && { same=true; break; }
  sleep 1
done
check "21 SUCC in the table ($tsucc) = .totals.successes in JSON ($jsucc)" [ "$same" = true ]
check "JSON totals are consistent (successes = completions - over_thresholds, failures = initiations - successes)" \
  jqtrue "$js" '.totals as $t | $t.successes == $t.completions - $t.over_thresholds and $t.failures == $t.initiations - $t.successes and $t.completions > 0'
check "JSON carries the effective configuration" jqtrue "$js" '.config.frequency == "5s" and .config.history.enhanced.interval == "60s"'

echo "== 8. stop goipslad and restore the environment =="
check "13 recovered to ok after tgt-l3 restart" jqtrue "$(gj show statistics 13)" '.latest.code == "ok"'
pid=$own_pid
stop_own_daemon
check "goipslad (pid $pid) exited" own_daemon_gone "$pid"
check "clean shutdown logged" sh -c "tail -n 3 '$repo/staging/run/goipslad.log' | grep -q 'goipslad stopped'"
check "API socket removed" sh -c "! docker compose -f '$repo/staging/compose.yaml' exec -T source test -e $SOCK"
check "tgt-r1: no netem, root qdisc as before ($(qdisc_saved tgt-r1))" qdisc_restored tgt-r1
check "tgt-r2: no netem, root qdisc as before ($(qdisc_saved tgt-r2))" qdisc_restored tgt-r2
check "tgt-l3 running again" sh -c "[ -n \"\$(docker compose -f '$repo/staging/compose.yaml' ps --status running -q tgt-l3)\" ]"
check "tgt-l3 reachable (IPv4)" src ping -4 -c 1 -W 2 -q 10.100.1.13
check "tgt-l3 reachable (IPv6)" src ping -6 -c 1 -W 2 -q fd00:100:1::13

echo
[ "$fail" -eq 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"
