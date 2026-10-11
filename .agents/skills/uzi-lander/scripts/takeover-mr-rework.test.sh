#!/usr/bin/env bash
# Hermetic regression: takeover.sh reports the run's mr_rework setting on both entry paths
# (by run id and by PR number) and warns on stderr only when it is on. A handover said a run
# had rework off while it was on, and an unwanted rework started on a bot comment. The script
# must stay read-only: the stub fails on any uzi call that is not a read.
set -eu
export NET_RETRY_BASE_SLEEP=0

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/takeover.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "repo list --json") echo '[{"id":"r1","path_with_namespace":"test/repo"}]' ;;
  # The list view deliberately omits mr_rework_enabled: only the detail view carries it.
  "run list --json") echo '[{"id":"run-1","repo_id":"r1","mr_iid":42,"kind":"issue","status":"running","created_at":"2026-01-01T00:00:00Z"}]' ;;
  "run get run-1 --json") [ "${GET_FAIL:-0}" = 1 ] && exit 1; cat "$WORK/run.json" ;;
  *) echo "unexpected (non-read) uzi call: $*" >&2; exit 99 ;;
esac
STUB
chmod +x "$WORK/bin/uzi"
export PATH="$WORK/bin:$PATH" WORK

# setrun <jq-fragment merged into the run>: the run detail the stub serves.
setrun() { jq -n "{id:\"run-1\",status:\"running\",kind:\"issue\",mr_iid:42} + $1" > "$WORK/run.json"; }
# snap TAG TARGET: one snapshot; stdout and stderr kept apart.
snap() { bash "$SCRIPT" "$2" --repo test/repo --no-claim > "$WORK/$1.out" 2> "$WORK/$1.err" || fail "$1: exit $?: $(cat "$WORK/$1.out" "$WORK/$1.err")"; }
want() { grep -qxF -- "$2" "$WORK/$1.out" || fail "$1: want line '$2': $(cat "$WORK/$1.out")"; }

for entry in run-1 42; do
  setrun '{mr_rework_enabled:true}'
  snap "true-$entry" "$entry"
  want "true-$entry" 'MR_REWORK_ENABLED=true'
  grep -q 'uzi run mr-rework <RUN> --enabled=false' "$WORK/true-$entry.err" || fail "true-$entry: no stderr warning: $(cat "$WORK/true-$entry.err")"
  if grep -q 'MR_REWORK_ENABLED=' "$WORK/true-$entry.err"; then fail "true-$entry: the key=value line leaked to stderr"; fi

  setrun '{mr_rework_enabled:false}'
  snap "false-$entry" "$entry"
  want "false-$entry" 'MR_REWORK_ENABLED=false'
  if grep -qi 'mr_rework is ENABLED' "$WORK/false-$entry.err"; then fail "false-$entry: warned though rework is off"; fi

  setrun '{mr_rework_enabled:null}'
  snap "null-$entry" "$entry"
  want "null-$entry" 'MR_REWORK_ENABLED=unknown'

  setrun '{}'
  snap "absent-$entry" "$entry"
  want "absent-$entry" 'MR_REWORK_ENABLED=unknown'
done

# By PR, the detail lookup failing is unknown, not a crash and not a guess.
GET_FAIL=1 snap getfail 42
want getfail 'MR_REWORK_ENABLED=unknown'

echo "PASS: takeover reports MR_REWORK_ENABLED on both entry paths"
