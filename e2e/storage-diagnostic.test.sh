#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="${STORAGE_TEST_LIB:-$ROOT/e2e/lib.sh}"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
eval "$(awk '/^storage_diagnostic_snapshot\(\) \{/,/^\}/' "$LIB")"
# shellcheck disable=SC2034  # consumed by the extracted real helper
RUNROOT="$SCRATCH"
# shellcheck disable=SC2034  # consumed by the extracted real helper
COMPOSE=(docker compose -p cdr-storage-fixture)
# No daemon or stack: validate the actual bounded commands and allocation evidence.
df() { [ "$1" = -Pk ]; echo 'runner-filesystem 100 81 19 81% /'; }
timeout() {
  [ "$1" = 15 ]; shift
  [ "$*" = 'docker system df' ] || return 91
  echo 'Images 12000MB; Build Cache 2000MB'
}
docker() {
  [ "${*:1:12}" = 'compose -p cdr-storage-fixture exec -T -u 0 agent timeout 15 sh -ec' ] || return 92
  [[ "${13:-}" == 'df -Pk /data; du -kx -d 2 /data;'* ]] || return 93
  [[ "${13:-}" == *'recovery bundles_kib='* ]] || return 94
  echo 'agent-filesystem 100 81 19 81% /data'
  printf '12 /data/repos\n24 /data/runner\n2 /data/recovery\n1 /data/outbox\n'
  echo 'recovery bundles_kib=2 bundle_count=1 journals_kib=4 journal_count=1'
}
storage_diagnostic_snapshot restore
storage_diagnostic_snapshot fixture-settlement
for label in restore fixture-settlement; do
  files=("$SCRATCH/artifacts/storage/$label-"*.txt)
  [ "${#files[@]}" = 1 ]
  for expected in runner-filesystem Images agent-filesystem /data/repos /data/runner /data/recovery /data/outbox bundles_kib journals_kib elapsed_seconds; do
    grep -F "$expected" "${files[0]}" >/dev/null
  done
done
echo 'PASS: bounded runner, Docker and agent allocation captured at both boundaries'
# Command failures must remain visible and must not prevent the other measurements.
timeout() { return 124; }
docker() { return 1; }
storage_diagnostic_snapshot failed
files=("$SCRATCH/artifacts/storage/failed-"*.txt)
grep -F 'Docker allocation unavailable or timed out' "${files[0]}" >/dev/null
grep -F 'agent allocation unavailable or timed out' "${files[0]}" >/dev/null
echo 'PASS: diagnostic failures are named and do not fail the phase'
storage_diagnostic_snapshot fixture-settlement
files=("$SCRATCH/artifacts/storage/fixture-settlement-"*.txt)
[ "${#files[@]}" = 2 ]
echo 'PASS: later settlement snapshots do not erase the original evidence'
echo 'cases=3 passed=3'
