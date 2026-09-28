#!/bin/sh
# P7 integration check: checks the goipslad AgentX subagent and SNMP traps with net-snmp
# snmpd / snmptrapd (staging/compose.snmp.yaml).
#
#   staging/scripts/p7-verify.sh [--no-scale]
#
# 1. Bring up snmpd (10.100.1.20) and trapd (10.100.1.21) (service names are given, so existing
#    containers are not recreated). Start goipslad with staging/run/p7.yaml (written inside the container), which adds global.snmp
#    to staging/goipslad.yaml and sets the action of the rtt reaction to trap,
#    and wait for AgentX registration.
# 2. snmpwalk ciscoRttMonMIB runs to completion, and the main values are as expected.
# 3. The sum of rttMonStatsCaptureCompletions matches goipsla show statistics aggregated.
# 4. Applying netem delay 150ms to tgt-r1 makes the rtt reaction of op 21 violate its threshold, and trapd receives
#    rttMonNotificationV2 (rttMonReactOccurred = true(1)) exactly once. Removing it gives false(2) once.
# 5. Use scale-addrs.sh add 1000 to get 1,000 operations and measure the table walk time (skipped with --no-scale).
#
# It checks the state before the run (goipslad, qdisc, scale-test addresses, snmpd / trapd), and on exit (including
# interruption) restores only what it changed itself (staging/scripts/verify-lib.sh): the goipslad it started,
# the netem it applied, the addresses it assigned, and the snmpd / trapd it brought up.
# If goipslad is already running, netem is already applied, or scale-test addresses are already assigned,
# it aborts without changing anything. Requires jq on the host.
set -u
repo=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$repo"

SCALE=1
[ "${1:-}" = "--no-scale" ] && SCALE=0

RUN=/work/staging/run
LOG=$RUN/goipslad-p7.log
SOCK=$RUN/goipslad.sock
CONFIG=$RUN/p7.yaml
SCALE_CONFIG=$RUN/p7-scale.yaml
SNMP_BLOCK='  snmp: { agentx: "tcp:10.100.1.20:705", traps: [ { host: 10.100.1.21, community: public } ] }'
WALK_LIMIT_S=30

fail=0
. "$repo/staging/scripts/verify-lib.sh"
# Both compose files, so that snmpd and trapd are known services.
dc()    { docker compose -f "$repo/staging/compose.yaml" -f "$repo/staging/compose.snmp.yaml" "$@"; }
snmp()  { dc exec -T snmpd "$@"; }
g()     { src /work/bin/goipsla --socket "$SOCK" "$@"; }

started_snmp=0
scale_added=0

# serve <config>: start goipslad and wait until snmpd answers for the MIB
# (rttMonApplNumCtrlAdminEntry.0 is the constant 1000 once the subagent is
# registered).
serve() {
  start_daemon "$1" "$LOG"
  i=0
  until snmp snmpget -v2c -c public -Oqv localhost CISCO-RTTMON-MIB::rttMonApplNumCtrlAdminEntry.0 2>/dev/null | grep -q '^1000$' || [ $i -ge 40 ]; do
    sleep 0.5; i=$((i + 1))
  done
  if [ $i -lt 40 ]; then ok "goipslad (pid $own_pid) registered with snmpd over AgentX ($1)"; else
    ng "goipslad did not register with snmpd ($1)"; src tail -n 20 "$LOG" | indent; exit 1
  fi
}

# Scale test addresses (10.200.k.0/24) present?

cleanup() {
  restore_env
  [ "$scale_added" -eq 1 ] && staging/scripts/scale-addrs.sh del >/dev/null 2>&1
  [ "$started_snmp" -eq 1 ] && dc stop snmpd trapd >/dev/null 2>&1
  src rm -f "$CONFIG" "$SCALE_CONFIG" 2>/dev/null
  true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

command -v jq >/dev/null 2>&1 || { echo "p7-verify: jq is required on the host" >&2; exit 2; }

# with_snmp <config>: the config plus global.snmp, with the action of the rtt
# reactions (action: syslog in staging/goipslad.yaml) turned into trap.
with_snmp() {
  awk -v snmp="$SNMP_BLOCK" '
    /^global:/ { print; print snmp; next }
    /^  snmp:/ { next }
    { gsub(/element: rtt, threshold-type: immediate, upper: 100, lower: 50, action: syslog/,
           "element: rtt, threshold-type: immediate, upper: 100, lower: 50, action: trap"); print }
  ' "$1"
}

echo "== 1. snmpd / trapd and goipslad =="
preflight_daemon
preflight_qdisc tgt-r1
if [ "$SCALE" -eq 1 ] && scale_present; then
  echo "p7-verify.sh: scale test addresses (10.200.k.0/24) are already configured; run 'staging/scripts/scale-addrs.sh del' first" >&2
  exit 2
fi
if ! dc ps --status running --services 2>/dev/null | grep -qx snmpd; then
  started_snmp=1
fi
if dc up -d --build snmpd trapd >/dev/null 2>&1; then ok "snmpd (10.100.1.20) and trapd (10.100.1.21) up"; else ng "compose up snmpd trapd"; exit 1; fi
if make build >/dev/null; then ok "make build"; else ng "make build"; exit 1; fi
src mkdir -p "$RUN"
with_snmp staging/goipslad.yaml | src sh -c 'cat >"$0"' "$CONFIG"
# The rtt reactions must have become traps, or step 4 would count no trap for
# a reason that has nothing to do with goipslad.
n=$(src grep -c 'element: rtt, threshold-type: immediate, upper: 100, lower: 50, action: trap' "$CONFIG" || true)
if [ "${n:-0}" -ge 1 ]; then ok "p7.yaml: $n rtt reaction line(s) turned into traps"; else
  ng "p7.yaml: no rtt reaction was turned into a trap (staging/goipslad.yaml changed?)"; exit 1
fi
if src /work/bin/goipsla validate "$CONFIG" >/dev/null 2>&1; then ok "staging/run/p7.yaml is valid"; else
  ng "staging/run/p7.yaml is invalid"; src /work/bin/goipsla validate "$CONFIG" | indent; exit 1
fi
serve "$CONFIG"
sleep 15 # a few attempts of every operation

echo "== 2. snmpwalk ciscoRttMonMIB =="
start=$(date +%s)
walk=$(snmp snmpwalk -v2c -c public localhost CISCO-RTTMON-MIB::ciscoRttMonMIB 2>/dev/null)
rc=$?
end=$(date +%s)
n=$(printf '%s\n' "$walk" | grep -c .)
if [ $rc -eq 0 ] && [ "$n" -gt 100 ]; then ok "walk completed: $n instances in $((end - start)) s"; else ng "walk failed (rc $rc, $n lines)"; fi
has "$walk" "OID not increasing" && ng "snmpwalk reported OID not increasing"
expect_has "$walk" "rttMonCtrlAdminRttType.11 = INTEGER: echo(1)"
expect_has "$walk" "rttMonCtrlAdminRttType.31 = INTEGER: icmpjitter(16)"
expect_has "$walk" "rttMonLatestRttOperSense.11 = INTEGER: ok(1)"
expect_has "$walk" "rttMonEchoAdminTargetAddress.11 = Hex-STRING: 0A 64 01 0B"
expect_has "$walk" "rttMonEchoAdminTargetAddress.111 = Hex-STRING: FD 00 01 00 00 01 00 00 00 00 00 00 00 00 00 11"
expect_has "$walk" "rttMonLatestIcmpJitterNumRTT.31 = Gauge32: 10"
expect_has "$walk" "rttMonReactVar.11.1 = INTEGER: rtt(1)"
expect_has "$walk" "rttMonCtrlOperState.11 = INTEGER: active(6)"
printf '%s\n' "$walk" | grep -E '\.11 =|\.11\.1 =' | head -n 25 | indent

echo "== 3. rttMonStatsCaptureTable matches goipsla show statistics aggregated 11 =="
before=$(g show statistics aggregated 11 -o json | jq '[.hours[].counters.completions] | add // 0')
cap=$(snmp snmpwalk -v2c -c public -Oqv localhost CISCO-RTTMON-MIB::rttMonStatsCaptureCompletions.11 2>/dev/null | awk '{s += $1} END {print s + 0}')
after=$(g show statistics aggregated 11 -o json | jq '[.hours[].counters.completions] | add // 0')
if [ "$cap" -ge "$before" ] && [ "$cap" -le "$after" ]; then ok "completions: snmp $cap, goipsla $before..$after"; else ng "completions: snmp $cap, goipsla $before..$after"; fi

echo "== 4. traps: tgt-r1 +150 ms =="
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
netem_add tgt-r1 delay 150ms
sleep 20
netem_del tgt-r1
sleep 20
traps=$(dc logs --no-log-prefix --since "$since" trapd 2>/dev/null)
up=$(printf '%s\n' "$traps" | grep -c 'rttMonReactOccurred.21.1 = INTEGER: true(1)')
down=$(printf '%s\n' "$traps" | grep -c 'rttMonReactOccurred.21.1 = INTEGER: false(2)')
[ "$up" -eq 1 ] && ok "op 21: one trap with rttMonReactOccurred = true(1)" || ng "op 21: $up traps with true(1) (want 1)"
[ "$down" -eq 1 ] && ok "op 21: one trap with rttMonReactOccurred = false(2)" || ng "op 21: $down traps with false(2) (want 1)"
expect_has "$traps" "rttMonNotificationV2"
expect_has "$traps" "rttMonReactVar.21.1 = INTEGER: rtt(1)"
printf '%s\n' "$traps" | grep -B2 -A10 'rttMonReactVar.21.1' | head -n 14 | indent
has "$traps" "rttMonReactOccurred.11.1" && ng "op 11 (not delayed) sent a trap"

if [ "$SCALE" -eq 1 ]; then
  echo "== 5. 1,000 operations: walk time =="
  stop_own_daemon
  if staging/scripts/scale-addrs.sh add 1000 >/dev/null; then scale_added=1; ok "scale-addrs.sh add 1000"; else ng "scale-addrs.sh add 1000"; fi
  targets=$(staging/scripts/scale-addrs.sh list | grep '^targets:')
  src sh -c 'cat >"$0"' "$SCALE_CONFIG" <<EOF
# generated by staging/scripts/p7-verify.sh
global:
  api-socket: $SOCK
$SNMP_BLOCK
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
  sleep 25 # every operation has had an attempt
  for spec in "snmpwalk rttMonLatestRttOperTable" "snmpwalk rttMonCtrlAdminTable" "snmpbulkwalk rttMonLatestRttOperTable" "snmpbulkwalk rttMonCtrlAdminTable" "snmpbulkwalk ciscoRttMonMIB"; do
    set -- $spec
    bulk=
    [ "$1" = snmpbulkwalk ] && bulk=-Cr50
    start=$(date +%s%N)
    n=$(snmp "$1" -v2c -c public $bulk localhost "CISCO-RTTMON-MIB::$2" 2>/dev/null | grep -c .)
    ms=$(( ($(date +%s%N) - start) / 1000000 ))
    msg="$1 $2: $n instances in ${ms} ms"
    if [ "$2" = ciscoRttMonMIB ]; then echo "  INFO $msg"; elif [ "$ms" -le $((WALK_LIMIT_S * 1000)) ]; then ok "$msg"; else ng "$msg (limit ${WALK_LIMIT_S} s)"; fi
  done
  pid=$own_pid
  stop_own_daemon
  own_daemon_gone "$pid" && ok "goipslad (pid $pid) exited" || ng "goipslad (pid $pid) still running"
  if staging/scripts/scale-addrs.sh del >/dev/null; then scale_added=0; fi
  scale_present && ng "scale-addrs.sh del: addresses remain" || ok "scale-addrs.sh del: scale addresses removed"
fi

echo "== 6. environment restored =="
pid=$own_pid
stop_own_daemon
[ -z "$pid" ] || { own_daemon_gone "$pid" && ok "goipslad (pid $pid) exited" || ng "goipslad (pid $pid) still running"; }
qdisc_restored tgt-r1 && ok "tgt-r1: no netem, root qdisc as before" || ng "tgt-r1 qdisc not restored"

echo
[ "$fail" -eq 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"
