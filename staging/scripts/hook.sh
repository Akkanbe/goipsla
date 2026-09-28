#!/bin/sh
# Receiver for the goipslad exec action (for the staging environment).
#
# ExecSink passes the Event JSON (one line) on standard input and sets environment variables such as GOIPSLA_EVENT_KIND.
# This script only appends the received JSON as one line to staging/run/hooks.log.
# p5-verify.sh counts this file to confirm that a notification was emitted "exactly once per transition".
# Each append is a single O_APPEND write, so lines do not interleave even with up to 4 running at the same time.
set -eu
log=${GOIPSLA_HOOK_LOG:-/work/staging/run/hooks.log}
line=$(tr -d '\n')
printf '%s\n' "$line" >>"$log"
