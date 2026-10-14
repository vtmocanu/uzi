#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="${SETTLEMENT_TEST_LIB:-$ROOT/e2e/lib.sh}"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
eval "$(awk '/^wait_worker_heartbeat_after\(\) \{/,/^\}/' "$LIB")"
fail() { echo "$*" >&2; exit 1; }
dump_fixture_settlement() { echo diagnostic >&2; }
sleep() { :; }
printf '0\n' > "$SCRATCH/reads"
db_psql() {
  case "$1" in
    *"last_heartbeat_at > '2026-10-13T17:37:10Z'::timestamptz"*"interval '15 seconds'"*) ;;
    'SELECT clock_timestamp()') echo 2026-10-13T17:37:10Z; return ;;
    *) fail 'heartbeat must be newer than restore AND still fresh' ;;
  esac
  local n
  n="$(cat "$SCRATCH/reads")"; n=$((n + 1)); printf '%s\n' "$n" > "$SCRATCH/reads"
  if [ "$n" -lt 3 ]; then echo f; else echo t; fi
}
# Drive the actual phase ordering, including the gate before its cancel calls.
# shellcheck disable=SC2034  # extracted phase consumes these identifiers
WORKER1=fixture EA=fixture GA=fixture
cancel_run() { [ "$(cat "$SCRATCH/reads")" = 3 ] || fail 'cancel ran before fresh post-restore heartbeat'; }
eval "$(awk '
  /^API_A_RESTORED_AT=/ { inside=1 }
  inside && (/^API_A_RESTORED_AT=/ || /^wait_worker_heartbeat_after/ || /^cancel_run/) {print}
  /^wait_runs_terminal_off_worker 60 "\$EA"/ {inside=0}
' "$ROOT/e2e/phases/42-api-outage-readoption.sh")"
[ "$(cat "$SCRATCH/reads")" = 3 ] || fail 'unchanged status must not bypass post-restore heartbeat'
echo 'PASS: old heartbeat and stale heartbeat cannot open cancellation'
# shellcheck disable=SC2329  # called indirectly by the extracted real helper
if ( db_psql() { echo f; }; wait_worker_heartbeat_after fixture 2026-10-13T17:37:10Z 0 ) > "$SCRATCH/timeout.log" 2>&1; then
  fail 'missing heartbeat accepted at deadline'
fi
grep -F 'has no fresh heartbeat after API restore' "$SCRATCH/timeout.log" >/dev/null
grep -F diagnostic "$SCRATCH/timeout.log" >/dev/null
echo 'PASS: bounded timeout collects evidence and names the stale worker'
# shellcheck disable=SC2034  # extracted cancel helper consumes RUNROOT
RUNROOT="$SCRATCH"
eval "$(awk '/^cancel_run\(\) \{/,/^\}/' "$ROOT/e2e/phases/42-api-outage-readoption.sh")"
# shellcheck disable=SC2329  # called by extracted cancel helper
apipost() { printf '{"server_side":true,"ignored":"not retained"}\n'; }
cancel_run fixture
jq -e '.server_side == true and .request_failed == false and .ignored == null' "$SCRATCH/artifacts/fixture-settlement/cancel.jsonl" >/dev/null
echo 'PASS: cancellation disposition is recorded without unrelated response data'
apipost() { return 1; }
cancel_run failed-fixture
tail -1 "$SCRATCH/artifacts/fixture-settlement/cancel.jsonl" | jq -e '.request_failed == true and .server_side == null' >/dev/null
echo 'PASS: failed cancellation also leaves diagnostic evidence'
# Exercise the real dump with and without the separately shipped collector.
eval "$(awk '/^dump_fixture_settlement\(\) \{/,/^\}/' "$LIB")"
# shellcheck disable=SC2034  # extracted dump consumes this array
COMPOSE=(settlement_compose)
settlement_compose() { echo '{}'; }
db_psql() { echo '{}'; }
storage_diagnostic_snapshot() { [ "$1" = fixture-settlement ]; echo called > "$SCRATCH/storage-calls"; }
dump_fixture_settlement >/dev/null
[ -f "$SCRATCH/storage-calls" ] || fail 'settlement failed to collect storage evidence'
[ "$(cat "$SCRATCH/storage-calls")" = called ] || fail 'settlement failed to collect storage evidence'
echo 'PASS: real fixture settlement dump invokes the storage collector'
unset -f storage_diagnostic_snapshot
dump_fixture_settlement >/dev/null
echo 'PASS: settlement remains usable before the separate collector lands'
echo 'cases=6 passed=6'
