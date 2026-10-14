#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="${STORAGE_TEST_LIB:-$ROOT/e2e/lib.sh}"
REAL_TIMEOUT="$(type -P timeout || type -P gtimeout)"
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
  local seconds="$1"; shift
  case "$seconds:$*" in
    '15:docker system df') echo 'Images 12000MB; Build Cache 2000MB' ;;
    '20:docker compose '*) "$@" ;;
    *) return 91 ;;
  esac
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
# The real host limiter enforces a short test deadline, while its wrapper pins
# the production 20s argument. A separate watchdog makes old unbounded code RED
# promptly. No Docker daemon, container or real resolver is involved.
tee "$SCRATCH/hang-compose" >/dev/null <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
touch "$STORAGE_COMPOSE_STARTED"
exec sleep 60
EOF
tee "$SCRATCH/host-probe" >/dev/null <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
eval "$(awk '/^storage_diagnostic_snapshot\(\) \{/,/^\}/' "$1")"
COMPOSE=("$2") ROOT=/fixture RUNROOT="$3"
STORAGE_TIMEOUT_BINARY="$4" STORAGE_TIMEOUT_MODE="$5"
export STORAGE_COMPOSE_STARTED="$RUNROOT/started-$STORAGE_TIMEOUT_MODE"
df() { :; }
command() {
  if [ "$STORAGE_TIMEOUT_MODE" = no-limiter ] && [ "${1:-}" = -v ]; then
    case "${2:-}" in timeout|gtimeout) return 1 ;; esac
  fi
  builtin command "$@"
}
timeout() {
  local seconds="$1"; shift
  if [ "$seconds:$*" = '15:docker system df' ]; then return 0; fi
  [ "$seconds" = 20 ] || return 91
  "$STORAGE_TIMEOUT_BINARY" 0.5 "$@"
}
storage_diagnostic_snapshot "host-$STORAGE_TIMEOUT_MODE"
EOF
chmod +x "$SCRATCH/hang-compose"
probe_failed=0
for mode in bounded no-limiter; do
  if ! "$REAL_TIMEOUT" 2 "$BASH" "$SCRATCH/host-probe" "$LIB" "$SCRATCH/hang-compose" "$SCRATCH" "$REAL_TIMEOUT" "$mode"; then
    echo "FAIL: $mode compose exec escaped the host bound" >&2
    probe_failed=1
    continue
  fi
  files=("$SCRATCH/artifacts/storage/host-$mode-"*.txt)
  if [ "$mode" = bounded ]; then
    [ -f "$SCRATCH/started-$mode" ] || { cat "${files[0]}"; echo 'FAIL: bounded compose never started' >&2; exit 1; }
    grep -F 'agent allocation unavailable or timed out' "${files[0]}" >/dev/null
    echo 'PASS: stalled compose exec is terminated by the host limiter'
  else
    [ ! -f "$SCRATCH/started-$mode" ]
    grep -F 'agent allocation skipped (no timeout command)' "${files[0]}" >/dev/null
    grep -F 'Docker allocation skipped (no timeout command)' "${files[0]}" >/dev/null
    echo 'PASS: no host limiter skips both daemon commands without starting compose'
  fi
done
[ "$probe_failed" = 0 ]
echo 'cases=5 passed=5'
