#!/bin/sh
# P5 integration check: threshold reactions, tracking, and events (exec / syslog / API / Prometheus).
#
#   staging/scripts/p5-verify.sh
#
# Copies staging/goipslad.yaml (react on template lab and jitter 31 / 32, tracks 1 / 2, and an action that passes all events
# to staging/scripts/hook.sh) to staging/run/p5.yaml inside the container, starts goipslad with it,
# and, by stopping, restoring, and delaying targets, counts in staging/run/hooks-p5.log that "a notification is emitted exactly once per transition".
# The exec in p5.yaml is replaced with a wrapper (staging/run/p5-hook.sh) that sets GOIPSLA_HOOK_LOG
# to this file and calls hook.sh. It does not touch the shared staging/run/hooks.log.
# The reload in step 6 rewrites only the upper of jitter 32 in p5.yaml (the original configuration is not changed).
#
# Takes about 3.5 minutes. It checks the state before the run, and on exit (including interruption) restores only what it changed itself
# (staging/scripts/verify-lib.sh). If goipslad is already running or netem is
# already applied, it aborts without changing anything. JSON is checked with jq on the host.
set -u
repo=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$repo"

RUN=/work/staging/run
SOCK=$RUN/goipslad.sock
LOG=$RUN/goipslad-p5.log
CONF=$RUN/p5.yaml
HOOKS=$RUN/hooks-p5.log
HOOK=$RUN/p5-hook.sh
URL=http://127.0.0.1:9818/metrics

fail=0
. "$repo/staging/scripts/verify-lib.sh"
g()   { src /work/bin/goipsla --socket "$SOCK" "$@"; }
gj()  { g "$@" -o json; }

# hooks: the events hook.sh received, as a JSON array.
hooks() { src sh -c "cat $HOOKS 2>/dev/null" | jq -s '.'; }
# nhooks <jq filter>: how many received events match.
nhooks() { hooks | jq "[.[] | select($1)] | length"; }
# track <n> <field>, reaction <op> <element> <field>
track() { gj show track "$1" | jq -r ".$2"; }
reaction() { gj show reactions "$1" | jq -r ".[] | select(.element == \"$2\") | .$3"; }
metric() { src /work/bin/metrics-get -url "$URL" 2>/dev/null | grep -F "$1" | awk '{print $NF}'; }

cleanup() {
  restore_env
  src rm -f "$CONF" "$HOOK" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

command -v jq >/dev/null 2>&1 || { echo "p5-verify: jq is required on the host" >&2; exit 2; }

echo "== 1. start: reactions, tracks and the initial track-up events =="
preflight_daemon
preflight_qdisc tgt-r1
if make build >/dev/null; then
  ok "make build, bin/metrics-get"
else
  ng "build"; exit 1
fi
src mkdir -p "$RUN"
src rm -f "$HOOKS"
src sh -c 'printf "#!/bin/sh\nGOIPSLA_HOOK_LOG=%s exec /work/staging/scripts/hook.sh\n" "$1" >"$0" && chmod +x "$0"' "$HOOK" "$HOOKS"
sed "s#exec: /work/staging/scripts/hook.sh#exec: $HOOK#" staging/goipslad.yaml | src sh -c 'cat >"$0"' "$CONF"
check "p5.yaml sends the events to $HOOK" src grep -q "exec: $HOOK" "$CONF"
start_daemon "$CONF" "$LOG"
i=0
until g health >/dev/null 2>&1 || [ $i -ge 20 ]; do sleep 0.5; i=$((i + 1)); done
check "goipslad (pid $own_pid) answers" g health
sleep 15
check "show reactions: 14 operations x 2 rows" jqtrue "$(gj show reactions)" 'length == 28 and ([.[].op_id] | unique | length) == 14'
g show track | indent
check "show track: 2 tracks, both up" jqtrue "$(gj show track)" 'length == 2 and all(.[]; .state == "up")'
check "hooks.log: exactly 2 events, both track-up (the initial transitions)" \
  jqtrue "$(hooks)" 'length == 2 and all(.[]; .kind == "track-up") and ([.[].track_id] | sort) == [1, 2]'

echo "== 2. tgt-l3 stopped: track 1 down, timeout reaction of 13, once each =="
service_stop tgt-l3
sleep 30
g show track 1 | indent
check "track 1 is down" [ "$(track 1 state)" = down ]
check "reaction 13 timeout occurred" [ "$(reaction 13 timeout occurred)" = true ]
n_down=$(nhooks '.kind == "track-down" and .track_id == 1')
n_exc=$(nhooks '.kind == "threshold-exceeded" and .op_id == 13 and .element == "timeout"')
check "hooks.log: track-down for track 1 once ($n_down)" [ "$n_down" = 1 ]
check "hooks.log: threshold-exceeded (timeout) for 13 once ($n_exc)" [ "$n_exc" = 1 ]
check "hooks.log: 113 (same target, IPv6) also exceeded once" [ "$(nhooks '.kind == "threshold-exceeded" and .op_id == 113 and .element == "timeout"')" = 1 ]
total=$(nhooks 'true')
sleep 30
check "30 s later nothing more was notified ($total -> $(nhooks 'true') events)" [ "$(nhooks 'true')" = "$total" ]

echo "== 3. tgt-l3 back: track 1 up (after delay up 5 s), timeout cleared, once each =="
service_restore tgt-l3
sleep 30
g show track 1 | indent
check "track 1 is up" [ "$(track 1 state)" = up ]
check "reaction 13 timeout cleared" [ "$(reaction 13 timeout occurred)" = false ]
check "hooks.log: track-up for track 1 twice in all (initial + recovery)" [ "$(nhooks '.kind == "track-up" and .track_id == 1')" = 2 ]
check "hooks.log: threshold-cleared (timeout) for 13 once" [ "$(nhooks '.kind == "threshold-cleared" and .op_id == 13 and .element == "timeout"')" = 1 ]
up=$(hooks | jq -r '[.[] | select(.kind == "track-up" and .track_id == 1)] | last | .time')
last_timeout=$(src sh -c "grep 'op=13 ' $LOG | grep 'rc=timeout' | tail -n 1" | sed -n 's/^time=\([^ ]*\) .*/\1/p')
echo "       last timeout of 13 at $last_timeout, track 1 up at $up"

echo "== 4. tgt-r1 +150ms: track 2 (state) down, rtt reaction of 21 =="
netem_add tgt-r1 delay 150ms
sleep 30
g show track 2 | indent
check "track 2 is down (overThreshold counts as down in state mode)" [ "$(track 2 state)" = down ]
check "reaction 21 rtt occurred" [ "$(reaction 21 rtt occurred)" = true ]
check "hooks.log: track-down for track 2 once" [ "$(nhooks '.kind == "track-down" and .track_id == 2')" = 1 ]
exc=$(hooks | jq -c '[.[] | select(.kind == "threshold-exceeded" and .op_id == 21 and .element == "rtt")]')
check "hooks.log: threshold-exceeded (rtt) for 21 once, value 150 ($(printf '%s' "$exc" | jq -c '[.[].value]'))" \
  jqtrue "$exc" 'length == 1 and .[0].value >= 150 and .[0].value < 160 and .[0].upper == 100'

echo "== 7. Prometheus agrees with the API =="
m_t1=$(metric 'goipsla_track_state{mode="reachability",op="13",track="1"}')
m_t2=$(metric 'goipsla_track_state{mode="state",op="21",track="2"}')
m_r21=$(metric 'goipsla_reaction_occurred{element="rtt",id="21"}')
m_r13=$(metric 'goipsla_reaction_occurred{element="timeout",id="13"}')
check "goipsla_track_state track 1 = 1 (up): $m_t1" [ "$m_t1" = 1 ]
check "goipsla_track_state track 2 = 0 (down): $m_t2" [ "$m_t2" = 0 ]
check "goipsla_reaction_occurred 21 rtt = 1: $m_r21" [ "$m_r21" = 1 ]
check "goipsla_reaction_occurred 13 timeout = 0: $m_r13" [ "$m_r13" = 0 ]
src /work/bin/metrics-get -url "$URL" 2>/dev/null | grep '^goipsla_events_total' | indent

echo "== 6. reload: only the reactions of 32 change -> updated, statistics kept, occurred reset without notification =="
check "reaction 32 rtt occurred before the reload" [ "$(reaction 32 rtt occurred)" = true ]
b_att=$(gj show statistics 32 | jq '.totals.initiations')
b_life=$(gj show statistics 32 | jq '.life_index')
b_hooks=$(nhooks 'true')
src sh -c 'awk '\''/id: 32,/ { in32 = 1 } in32 && /upper: 100/ && !done { sub(/upper: 100/, "upper: 120"); done = 1 } { print }'\'' "$0" >"$0.new" && mv "$0.new" "$0"' "$CONF"
res=$(gj reload)
printf '%s\n' "$res" | jq -c . | indent
check "reload result: updated [32] only" jqtrue "$res" '.updated == [32] and .restarted == [] and .added == [] and .removed == []'
check "reaction 32 rtt: upper 120, occurred false" jqtrue "$(gj show reactions 32)" '.[] | select(.element == "rtt") | .upper == 120 and .occurred == false'
check "32 statistics kept (life $b_life, $b_att attempts)" jqtrue "$(gj show statistics 32)" ".life_index == $b_life and .totals.initiations >= $b_att"
check "the reload itself notified nothing ($b_hooks events)" [ "$(nhooks 'true')" = "$b_hooks" ]

echo "== 4 (cont.). tgt-r1 back: track 2 up, rtt of 21 cleared, once each =="
netem_del tgt-r1
sleep 30
check "track 2 is up" [ "$(track 2 state)" = up ]
check "reaction 21 rtt cleared" [ "$(reaction 21 rtt occurred)" = false ]
check "hooks.log: threshold-cleared (rtt) for 21 once" [ "$(nhooks '.kind == "threshold-cleared" and .op_id == 21 and .element == "rtt"')" = 1 ]
check "hooks.log: track-up for track 2 twice in all (initial + recovery)" [ "$(nhooks '.kind == "track-up" and .track_id == 2')" = 2 ]

echo "== 5. show events: every notification, in time order =="
ev=$(gj show events --limit 1000)
printf '%s' "$ev" | jq -r '.[] | select(.op_id == 13 or .op_id == 21) | "\(.time)  \(.kind)  \(.message)"' | indent
check "events are in time order" jqtrue "$ev" '[.[].time] == ([.[].time] | sort)'
check "events = what hook.sh received ($(printf '%s' "$ev" | jq length) / $(nhooks 'true'))" \
  jqtrue "$ev" "length == $(nhooks 'true')"
check "events: track 1 down -> up, track 2 down -> up" \
  jqtrue "$ev" '[.[] | select(.track_id == 1) | .kind] == ["track-up", "track-down", "track-up"] and [.[] | select(.track_id == 2) | .kind] == ["track-up", "track-down", "track-up"]'

echo "== 8. syslog: no syslog daemon; the other sinks keep going =="
warns=$(src sh -c "grep -c 'level=WARN.*syslog' $LOG" || true)
src sh -c "grep 'level=WARN.*syslog' $LOG | head -n 3" | indent
check "syslog connection warning logged ($warns lines)" [ "${warns:-0}" -ge 1 ]
st=$(src /work/bin/metrics-get -url "$URL" 2>/dev/null | grep '^goipsla_events_total')
printf '%s\n' "$st" | indent
check "exec sink delivered while syslog failed" has "$st" 'result="delivered",sink="exec'
check "syslog sink failed (no /dev/log)" has "$st" 'result="failed",sink="syslog"' 

echo "== 9. stop and restore =="
pid=$own_pid
stop_own_daemon
check "goipslad (pid $pid) exited" own_daemon_gone "$pid"
check "clean shutdown logged" sh -c "docker compose -f '$repo/staging/compose.yaml' exec -T source tail -n 3 $LOG | grep -q 'goipslad stopped'"
check "tgt-r1: no netem, root qdisc as before" qdisc_restored tgt-r1
check "tgt-l3 running" sh -c "[ -n \"\$(docker compose -f '$repo/staging/compose.yaml' ps --status running -q tgt-l3)\" ]"

echo
[ "$fail" -eq 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"
