# Shared helpers of the staging verification scripts (p2-verify.sh, p4-verify.sh).
# Source it after setting $repo. Every change a script makes to the staging
# environment goes through these functions, which remember what they changed,
# so that restore_env undoes exactly that and nothing else:
#
#   - goipslad: a script refuses to run if a goipslad is already running in
#     the source container (it would share the API socket and the metrics
#     port), and it only ever stops the process it started (by PID).
#   - qdisc: the root qdisc of a target is saved before the script touches
#     it; the script refuses to add netem on top of an existing netem, and
#     restore removes only the netem it added.
#   - containers: a target is started again only if the script stopped it.
#
# shellcheck shell=sh

dc()  { docker compose -f "$repo/staging/compose.yaml" "$@"; }
src() { dc exec -T source "$@"; }

# Reporting and small predicates shared by the verification scripts. A script
# sets fail=0 before sourcing this file; ng sets it to 1.
ok()   { printf '  OK   %s\n' "$1"; }
ng()   { printf '  NG   %s\n' "$1"; fail=1; }
info() { printf '  INFO %s\n' "$1"; }
indent() { sed 's/^/       /'; }
# check <description> <command...>: OK if the command succeeds (its output is hidden).
check() {
  what=$1; shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else ng "$what"; fi
}
# has <text> <string>: whether text contains string (fixed string, not a pattern).
has() { printf '%s\n' "$1" | grep -qF "$2"; }
# expect_has <text> <string>: OK / NG on whether text contains string.
expect_has() { if has "$1" "$2"; then ok "$2"; else ng "missing: $2"; fi; }
# jqtrue <json> <jq expression>: whether the expression is true.
jqtrue() { [ "$(printf '%s' "$1" | jq -r "$2" 2>/dev/null)" = true ]; }
# scale_present: whether the scale test addresses (10.200.k.0/24) are configured.
scale_present() { "$repo/staging/scripts/scale-addrs.sh" list 2>/dev/null | grep -q '^targets: { [0-9]'; }

VERIFY_PIDFILE=/work/staging/run/verify-goipslad.pid
own_pid=
changed_qdisc=
stopped_services=

# qdisc_root <service>: the root qdisc line(s) of eth0 in a container.
qdisc_root() { dc exec -T "$1" tc qdisc show dev eth0 root 2>/dev/null; }

# has_netem <service>: whether the root qdisc of eth0 is netem.
has_netem() { qdisc_root "$1" | grep -q '^qdisc netem '; }

# preflight_daemon: refuse to run next to a goipslad this script did not start.
preflight_daemon() {
  if src pgrep -x goipslad >/dev/null 2>&1; then
    echo "$(basename "$0"): a goipslad is already running in the source container (pid $(src pgrep -x goipslad | tr '\n' ' '));" >&2
    echo "  stop it first; this script does not stop processes it did not start" >&2
    exit 2
  fi
}

# preflight_qdisc <service>...: save the root qdiscs and refuse to run if one
# is already netem (the script would replace it).
preflight_qdisc() {
  for s in "$@"; do
    before=$(qdisc_root "$s")
    eval "qdisc_before_$(echo "$s" | tr '-' '_')=\$before"
    if printf '%s\n' "$before" | grep -q '^qdisc netem '; then
      echo "$(basename "$0"): $s already has a netem root qdisc: $before" >&2
      echo "  remove it first (tc qdisc del dev eth0 root); this script does not overwrite it" >&2
      exit 2
    fi
  done
}

# qdisc_saved <service>: the root qdisc saved by preflight_qdisc.
qdisc_saved() { eval "printf '%s\n' \"\$qdisc_before_$(echo "$1" | tr '-' '_')\""; }

# start_daemon <config> <log>: start goipslad in the background and record its PID.
start_daemon() {
  dc exec -d source sh -c 'echo $$ >"$0" && exec /work/bin/goipslad --config "$1" >"$2" 2>&1' \
    "$VERIFY_PIDFILE" "$1" "$2"
  i=0
  while [ -z "$own_pid" ] && [ $i -lt 20 ]; do
    own_pid=$(src cat "$VERIFY_PIDFILE" 2>/dev/null)
    [ -n "$own_pid" ] || { sleep 0.2; i=$((i + 1)); }
  done
}

# stop_own_daemon: stop the goipslad this script started, and nothing else.
stop_own_daemon() {
  [ -n "$own_pid" ] || return 0
  if src kill -0 "$own_pid" 2>/dev/null; then
    src kill -INT "$own_pid"
    i=0
    while src kill -0 "$own_pid" 2>/dev/null && [ $i -lt 20 ]; do sleep 0.5; i=$((i + 1)); done
  fi
  src rm -f "$VERIFY_PIDFILE" 2>/dev/null || true
  own_pid=
}

# own_daemon_gone: whether the goipslad this script started has exited.
own_daemon_gone() { [ -n "$1" ] && ! src kill -0 "$1" 2>/dev/null; }

# netem_add <service> <netem args...>
netem_add() {
  s=$1; shift
  dc exec -T "$s" tc qdisc add dev eth0 root netem "$@" && changed_qdisc="$changed_qdisc $s"
}

# netem_del <service>: remove the netem this script added.
netem_del() {
  case " $changed_qdisc " in *" $1 "*) ;; *) return 0 ;; esac
  dc exec -T "$1" tc qdisc del dev eth0 root 2>/dev/null
  changed_qdisc=$(printf '%s\n' $changed_qdisc | grep -vx "$1" | tr '\n' ' ')
}

# qdisc_restored <service>: no netem, and the root qdisc is what it was.
qdisc_restored() {
  ! has_netem "$1" && [ "$(qdisc_root "$1" | sed 's/ refcnt [0-9]*//')" = "$(qdisc_saved "$1" | sed 's/ refcnt [0-9]*//')" ]
}

# service_stop <service>: stop a running target and remember it.
service_stop() {
  if [ -n "$(dc ps --status running -q "$1" 2>/dev/null)" ]; then
    dc stop "$1" >/dev/null 2>&1 && stopped_services="$stopped_services $1"
  fi
}

# service_restore <service>: start it again if this script stopped it.
service_restore() {
  case " $stopped_services " in *" $1 "*) ;; *) return 0 ;; esac
  dc start "$1" >/dev/null 2>&1
  stopped_services=$(printf '%s\n' $stopped_services | grep -vx "$1" | tr '\n' ' ')
}

# restore_env: undo whatever is still changed (used by the EXIT trap).
restore_env() {
  for s in $changed_qdisc; do netem_del "$s"; done
  for s in $stopped_services; do service_restore "$s"; done
  stop_own_daemon
}
