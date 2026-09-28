#!/bin/sh
# Build statically linked bin/goipslad, bin/goipsla, and bin/probe-echo on the host.
# The source container mounts the repository at /work, so it can run them as /work/bin/goipslad.
# Arguments are passed to make as is (e.g. build.sh VERSION=1.2.3).
set -eu
cd "$(dirname "$0")/../.."
exec make build "$@"
