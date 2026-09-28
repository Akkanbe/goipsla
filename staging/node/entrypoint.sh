#!/bin/sh
# Set static routes from the ROUTES / ROUTES6 environment variables, then run the command.
# Format: "destination via gateway" entries separated by ';' (e.g. "default via 10.100.2.254;10.9.0.0/16 via 10.100.2.254")
set -eu

apply_routes() {
  family="$1"; spec="$2"
  [ -z "$spec" ] && return 0
  echo "$spec" | tr ';' '\n' | while read -r route; do
    [ -z "$route" ] && continue
    # shellcheck disable=SC2086
    ip "$family" route replace $route
    echo "[entrypoint] ip $family route replace $route"
  done
}

apply_routes -4 "${ROUTES:-}"
apply_routes -6 "${ROUTES6:-}"

exec "$@"
