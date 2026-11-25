#!/usr/bin/env bash
# Hermetic regression: wait-mrrework.sh stops on a current-cycle rework parked for the owner
# (awaiting_approval, awaiting_input, paused) with exit 6, instead of polling a run that
# never moves on its own until the budget elapses. A rework whose plan cross-check ended
# non-pass sat at a forced plan gate while the lander waited on completed|failed|cancelled.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/wait-mrrework.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "repo list --json") echo '[{"id":"r1","path_with_namespace":"test/repo"}]' ;;
  "run list --json") cat "$WORK/runs.json" ;;
  *) echo "unexpected uzi call: $*" >&2; exit 99 ;;
esac
STUB
chmod +x "$WORK/bin/uzi"
export PATH="$WORK/bin:$PATH" WORK

# setstatus STATUS: one current-cycle mr_rework run on PR 42 in that status, plus a stale
# completed prior cycle created before the SINCE baseline.
setstatus() {
  jq -n --arg st "$1" '[
    {id:"old",kind:"mr_rework",repo_id:"r1",mr_iid:42,status:"completed",created_at:"2026-01-01T00:00:00Z"},
    {id:"cur",kind:"mr_rework",repo_id:"r1",mr_iid:42,status:$st,created_at:"2026-01-02T00:00:00.123Z"}]' > "$WORK/runs.json"
}
run() { rc=0; bash "$SCRIPT" test/repo 42 2 0 2026-01-01T12:00:00Z > "$WORK/out" 2>&1 || rc=$?; }

for st in awaiting_approval awaiting_input paused; do
  setstatus "$st"; run
  [ "$rc" = 6 ] || fail "$st: want exit 6, got $rc: $(cat "$WORK/out")"
  grep -qxF "MRREWORK_ID=cur" "$WORK/out" || fail "$st: no MRREWORK_ID=cur: $(cat "$WORK/out")"
  grep -qxF "MRREWORK_STATUS=$st" "$WORK/out" || fail "$st: no MRREWORK_STATUS line: $(cat "$WORK/out")"
done

for st in completed failed cancelled; do
  setstatus "$st"; run
  [ "$rc" = 0 ] || fail "$st: want exit 0, got $rc: $(cat "$WORK/out")"
done

setstatus running; run
[ "$rc" = 3 ] || fail "running: want exit 3 (still reworking), got $rc: $(cat "$WORK/out")"

echo "wait-mrrework: parked/terminal/running exits OK"
