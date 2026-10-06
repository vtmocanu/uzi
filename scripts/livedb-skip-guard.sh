#!/usr/bin/env bash
# Aggregate LiveDB gate on the gate, shared by e2e/run-store-it.sh and the CI store-it job
# so the two cannot drift. Reads a `go test -v` log and fails unless the suite RAN at least
# one top-level test and SKIPPED none at any depth: with the DSN unset or Postgres
# unreachable every test self-skips and `go test` still prints ok. `go test -v` indents
# subtest results, so a skipped subtest under a passing parent only shows as an indented
# `--- SKIP:` line; counting `^--- SKIP` alone read that as 0 skipped.
#
# Usage: livedb-skip-guard.sh LOG
# Exit: 0 ran and nothing skipped; 1 nothing ran or something skipped; 2 usage.
set -euo pipefail

if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
  echo "usage: livedb-skip-guard.sh LOG (an existing go test -v log)" >&2
  exit 2
fi
log="$1"
ran=$(grep -c '^--- PASS' "$log" || true)
skipped=$(grep -cE '^[[:space:]]*--- SKIP:' "$log" || true)
echo "LiveDB: $ran passed, $skipped skipped"
if [ "$ran" -eq 0 ] || [ "$skipped" -gt 0 ]; then
  echo "FAIL: the live-DB tests did not actually run against Postgres (ran=$ran skipped=$skipped)." >&2
  echo "They skip when UZI_TEST_DATABASE_URL is unset or the service is unreachable, and" >&2
  echo "'go test' still prints ok. A skipped subtest at any depth counts." >&2
  exit 1
fi
