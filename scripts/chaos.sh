#!/usr/bin/env bash
# Crash-consistency and concurrency checks for the write path.
#   OPENETL_TEST_DSN='user:pw@tcp(host:3306)/db?parseTime=true&multiStatements=true' scripts/chaos.sh
# The tests create a private database on that server (and drop it), build and run the real
# openetl-server binary, hammer it with concurrent writers, SIGKILL it mid-load several times, restart it,
# and check the event log (see docs/invariants.md INV-18..INV-21). Knobs (defaults in brackets):
#   CHAOS_KILLS [8]  CHAOS_ROUND_MS [3000]  CHAOS_WRITERS [24]
# Also runs the in-process races (merge cycles, one entity, one request_id) and the killed-connection test.
set -euo pipefail
cd "$(dirname "$0")/.."
: "${OPENETL_TEST_DSN:?set OPENETL_TEST_DSN}"
export CHAOS_KILLS="${CHAOS_KILLS:-8}" CHAOS_ROUND_MS="${CHAOS_ROUND_MS:-3000}" CHAOS_WRITERS="${CHAOS_WRITERS:-24}"
exec go test -count=1 -timeout 15m -v -run 'TestChaosKill9|TestConcurrent|TestKilledConnection' ./internal/store
