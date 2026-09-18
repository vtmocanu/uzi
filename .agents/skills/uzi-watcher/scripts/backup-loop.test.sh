#!/usr/bin/env bash
# End-to-end regression for backup-loop.sh's detached-loop state machine.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/backup-loop.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail(){ echo "FAIL: $*" >&2; exit 1; }

cat > "$WORK/backup-runs" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CALLS"
exit 1
STUB
chmod +x "$WORK/backup-runs"

cat > "$WORK/uzi" <<'STUB'
#!/usr/bin/env bash
set -u
if [ "${1:-}" = run ] && [ "${2:-}" = get ] && [ "${4:-}" = --field ]; then
  case "${3:-}" in
    run-a) echo completed ;;
    run-b)
      n=0; [ -f "$COUNT" ] && n="$(cat "$COUNT")"
      n=$((n + 1)); echo "$n" > "$COUNT"
      if [ "$n" -eq 1 ]; then echo running; else echo completed; fi ;;
  esac
fi
STUB
chmod +x "$WORK/uzi"

export CALLS="$WORK/calls"
export COUNT="$WORK/count"
UZI_BIN="$WORK/uzi" UZI_BACKUP_RUNS_SCRIPT="$WORK/backup-runs" \
  UZI_BACKUP_DIR="$WORK/out" UZI_BACKUP_INTERVAL=1 UZI_BACKUP_MAX_HOURS=1 \
  UZI_BACKUP_RETENTION_DAYS=14 UZI_CTX=test-ctx UZI_WORKER_NS='ns-a ns-b' \
  bash "$SCRIPT" run-a run-b > "$WORK/loop.log" 2>&1

[ "$(awk 'NR==1{print; exit}' "$CALLS")" = "run-a run-b" ] \
  || fail "first cycle did not include both runs: $(cat "$CALLS")"
[ "$(awk 'NR==2{print; exit}' "$CALLS")" = "run-b" ] \
  || fail "terminal run-a was not pruned: $(cat "$CALLS")"
[ "$(wc -l < "$CALLS" | tr -d ' ')" = 2 ] \
  || fail "unexpected extra backup cycles: $(cat "$CALLS")"
[ ! -e "$WORK/out/backup-loop.pid" ] || fail "pid file survived loop exit"
grep -q '^context=test-ctx$' "$WORK/out/backup-loop.state" || fail "context missing from state"
grep -q '^namespaces=ns-a ns-b$' "$WORK/out/backup-loop.state" || fail "namespaces missing from state"
grep -q '^ends_at=' "$WORK/out/backup-loop.state" || fail "end time missing from state"
grep -q '^status=ended$' "$WORK/out/backup-loop.state" || fail "ended state missing"
grep -q 'backup cycle incomplete rc=1' "$WORK/loop.log" || fail "failed cycle was not surfaced"
grep -q 'retired terminal run run-a status=completed' "$WORK/loop.log" || fail "run-a retirement missing"
grep -q 'all runs terminal; exiting' "$WORK/loop.log" || fail "terminal exit missing"

echo "PASS backup-loop: state manifest, failed-cycle visibility, terminal pruning"
