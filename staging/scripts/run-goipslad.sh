#!/bin/sh
# Run the goipslad built on the host in the source container of the staging environment.
#
#   staging/scripts/run-goipslad.sh [config] [additional goipslad flags...]
#
# How config is handled:
#   - If omitted, staging/goipslad.yaml if it exists, otherwise /dev/null.
#   - A file inside the repository (relative or absolute) is mapped to a path under /work.
#   - Anything else is passed as is as a path inside the container (e.g. /dev/null).
#
# Stopping the docker compose exec -T client does not deliver the signal
# to the process inside the container, which is left running. So the PID inside the container is recorded, and
# when this script receives SIGINT / SIGTERM it forwards the signal to goipslad. On SIGHUP (the terminal was closed)
# it sends SIGTERM to stop goipslad rather than reload it. To try reload, use
# pkill -HUP goipslad inside the container.
# When goipslad exits, the client exits too, and this script exits with that exit code.
set -eu

repo=$(cd "$(dirname "$0")/../.." && pwd -P)
compose() { docker compose -f "$repo/staging/compose.yaml" "$@"; }

"$repo/staging/scripts/build.sh"

if ! compose ps --status running --services 2>/dev/null | grep -qx source; then
  echo "run-goipslad: the staging source container is not running; start it with 'make staging-up'" >&2
  exit 1
fi

# Map a path on the host to a path inside the source container.
container_path() {
  p=$1
  if [ -e "$p" ] && [ "$p" != /dev/null ]; then
    abs=$(cd "$(dirname "$p")" && pwd -P)/$(basename "$p")
    case "$abs" in
      "$repo"/*) echo "/work/${abs#"$repo"/}"; return ;;
    esac
  fi
  echo "$p"
}

if [ $# -gt 0 ]; then
  cfg=$1
  shift
elif [ -f "$repo/staging/goipslad.yaml" ]; then
  cfg=$repo/staging/goipslad.yaml
else
  cfg=/dev/null
fi
cfg=$(container_path "$cfg")

pidfile=/tmp/run-goipslad.$$.pid
echo "run-goipslad: /work/bin/goipslad --config $cfg $* (in ipsla-source)" >&2

# Detach from the terminal's process group with setsid so that Ctrl-C does not kill the client directly.
# (This script's trap forwards Ctrl-C to goipslad, and the shutdown log is shown to the end.)
detach=
command -v setsid >/dev/null 2>&1 && detach=setsid
# shellcheck disable=SC2016 # $$ / $0 / $@ are expanded by the container's shell
$detach docker compose -f "$repo/staging/compose.yaml" exec -T source \
  sh -c 'echo $$ > "$0" && exec /work/bin/goipslad "$@"' "$pidfile" --config "$cfg" "$@" &
client=$!

signaled=0
forward() {
  signaled=1
  compose exec -T source sh -c 'test -s "$0" && kill -"$1" "$(cat "$0")"' "$pidfile" "$1" 2>/dev/null || true
}
trap 'forward TERM' TERM
trap 'forward INT' INT
trap 'forward TERM' HUP

# wait is interrupted by a trapped signal, so in that case wait again to get the client's exit code.
while :; do
  signaled=0
  if wait "$client"; then status=0; else status=$?; fi
  [ "$signaled" -eq 1 ] || break
done
compose exec -T source rm -f "$pidfile" 2>/dev/null || true
exit "$status"
