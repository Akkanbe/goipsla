#!/bin/sh
# Run all integration checks of the staging environment together and summarize the results in one table.
#
#   staging/scripts/verify-all.sh [--no-integration] [--log-dir DIR]
#
# Runs the following stages in order. Even if a stage fails, it runs to the end, and finally prints the result of each stage
# (exit code, duration, last line of the log) as a table. Exit code 1 if any stage fails.
#
#   build        make build (equivalent to p1; statically links bin/goipslad, bin/goipsla, and tools)
#   verify       staging/scripts/verify.sh (equivalent to p1; reachability, routes, and ICMP Timestamp to all targets)
#   p2           staging/scripts/p2-verify.sh (statistics, history, enhanced history)
#   p4           staging/scripts/p4-verify.sh (reload, life, restart / reset, SIGHUP)
#   p5           staging/scripts/p5-verify.sh (threshold reactions, tracking, events, exec / syslog)
#   p6           staging/scripts/p6-verify.sh --no-scale (Prometheus exporter; the scale test is skipped)
#   p7           staging/scripts/p7-verify.sh --no-scale (AgentX subagent and SNMP traps;
#                brings up snmpd / trapd if absent and stops the ones it brought up at the end; the scale test is skipped)
#   integration  make staging-integration-test (runs the integration tests in the source container).
#                Skipped with --no-integration
#
# Each stage follows its script's contract (save the state before the run and restore only what it changed;
# abort without changing anything if goipslad is already running). This script itself
# changes nothing in the staging environment.
#
# The output of each stage is kept as <stage>.log in --log-dir (by default a temporary directory created with mktemp -d),
# and its location is printed at the end. The whole run takes about 14 to 17 minutes.
set -u

cd "$(dirname "$0")/../.." || exit 2

INTEGRATION=1
LOG_DIR=""
while [ $# -gt 0 ]; do
	case "$1" in
	--no-integration) INTEGRATION=0 ;;
	--log-dir)
		[ $# -ge 2 ] || { echo "verify-all: --log-dir needs a directory" >&2; exit 2; }
		LOG_DIR="$2"
		shift
		;;
	-h | --help)
		sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "verify-all: unknown argument: $1" >&2
		exit 2
		;;
	esac
	shift
done

if [ -z "$LOG_DIR" ]; then
	LOG_DIR=$(mktemp -d "${TMPDIR:-/tmp}/verify-all.XXXXXX") || exit 2
else
	mkdir -p "$LOG_DIR" || exit 2
fi

SUMMARY="$LOG_DIR/summary.tsv"
: >"$SUMMARY"
FAILED=0

# step <name> <command...>: run one stage, log its output, record the result.
step() {
	name=$1
	shift
	log="$LOG_DIR/$name.log"
	echo "==> $name: $*"
	start=$(date +%s)
	"$@" >"$log" 2>&1
	rc=$?
	end=$(date +%s)
	last=$(grep -v '^[[:space:]]*$' "$log" | tail -n 1 | cut -c1-100)
	if [ "$rc" -eq 0 ]; then
		result=PASS
	else
		result=FAIL
		FAILED=1
	fi
	echo "    $result (exit $rc, $((end - start))s): $last"
	printf '%s\t%s\t%s\t%s\t%s\n' "$name" "$result" "$rc" "$((end - start))" "$last" >>"$SUMMARY"
}

# A stage that is interrupted stops the whole run.
trap 'echo "verify-all: interrupted; logs in $LOG_DIR" >&2; exit 130' INT TERM

step build make build
step verify staging/scripts/verify.sh
step p2 staging/scripts/p2-verify.sh
step p4 staging/scripts/p4-verify.sh
step p5 staging/scripts/p5-verify.sh
step p6 staging/scripts/p6-verify.sh --no-scale
step p7 staging/scripts/p7-verify.sh --no-scale
if [ "$INTEGRATION" -eq 1 ]; then
	step integration make staging-integration-test
fi

echo
echo "== summary"
printf '%-12s %-6s %4s %6s  %s\n' STAGE RESULT EXIT TIME LAST-LINE
while IFS="$(printf '\t')" read -r name result rc secs last; do
	printf '%-12s %-6s %4s %5ss  %s\n' "$name" "$result" "$rc" "$secs" "$last"
done <"$SUMMARY"
echo "logs: $LOG_DIR"
exit "$FAILED"
