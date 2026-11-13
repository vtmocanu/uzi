#!/usr/bin/env bash
# Hermetic regression: CodeRabbit's head description is read from its check-run as well as
# its legacy commit status, the newer channel wins, and an unreadable channel fails closed.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=cr-signal.sh
. "$HERE/cr-signal.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
case "$*" in
  *'/status'*) cat "$STATUS_FILE" ;;
  *'/check-runs'*) [ -s "$RUNS_FILE" ] || exit 1; cat "$RUNS_FILE" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH" STATUS_FILE="$WORK/status.json" RUNS_FILE="$WORK/runs.json"

set_case() { printf '%s\n' "$1" > "$STATUS_FILE"; printf '%s\n' "$2" > "$RUNS_FILE"; }
want() {
  local got rc=0
  got=$(cr_head_desc test/repo deadbeef) || rc=$?
  [ "$rc" -eq 0 ] || fail "$1: rc=$rc"
  [ "$got" = "$2" ] || fail "$1: got '$got' want '$2'"
}
CR_RUN='{"id":%s,"app":{"slug":"coderabbitai"},"name":"CodeRabbit","status":"%s","conclusion":%s,"started_at":"%s","completed_at":%s,"output":{"title":"%s"}}'
run() { printf "$CR_RUN" "$@"; }

# The PR #4260 shape: no status, a skipped check-run.
set_case '{"statuses":[]}' "{\"check_runs\":[$(run 3 completed '"skipped"' 2026-11-13T06:52:01Z '"2026-11-13T06:52:01Z"' 'Review skipped: 133 files exceed the limit of 100')]}"
want "check-run skip" "Review skipped: 133 files exceed the limit of 100"

# Status only (legacy channel) still works.
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review rate limited","updated_at":"2026-09-23T10:00:00Z"}]}' '{"check_runs":[]}'
want "status only" "Review rate limited"

# Both channels: the newer one wins, in either direction.
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review rate limited","updated_at":"2026-09-23T10:00:00Z"}]}' \
  "{\"check_runs\":[$(run 5 completed '"success"' 2026-09-23T10:30:00Z '"2026-09-23T10:31:00Z"' 'Finished')]}"
want "newer check-run" "Finished"
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review rate limited","updated_at":"2026-09-23T11:00:00Z"}]}' \
  "{\"check_runs\":[$(run 5 completed '"success"' 2026-09-23T10:30:00Z '"2026-09-23T10:31:00Z"' 'Finished')]}"
want "newer status" "Review rate limited"

# A running check-run reads in progress; the newest run (max id) wins over an older one.
set_case '{"statuses":[]}' "{\"check_runs\":[$(run 9 in_progress null 2026-11-13T07:00:00Z null ''),$(run 8 completed '"skipped"' 2026-11-13T06:52:01Z '"2026-11-13T06:52:01Z"' 'Review skipped: x')]}"
want "in progress" "Review in progress"

# Conservative precedence: a pending review is never hidden.
# A queued check-run (no timestamps) beats an older skip status.
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review skipped: x","updated_at":"2026-11-13T06:00:00Z"}]}' \
  '{"check_runs":[{"id":7,"app":{"slug":"coderabbitai"},"name":"CodeRabbit","status":"queued","conclusion":null,"started_at":null,"completed_at":null,"output":{"title":null}}]}'
want "queued check-run vs older skip" "Review in progress"
# An in-progress status at the same time as a finished check-run stays in progress.
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review in progress","updated_at":"2026-11-13T07:00:00Z"}]}' \
  "{\"check_runs\":[$(run 5 completed '"success"' 2026-11-13T06:59:00Z '"2026-11-13T07:00:00Z"' 'Finished')]}"
want "equal-time in-progress status" "Review in progress"
# Strictly ordered: a later completion beats a stale in-progress status, and keeps its time.
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review in progress","updated_at":"2026-11-13T10:00:00Z"}]}' \
  "{\"check_runs\":[$(run 5 completed '"success"' 2026-11-13T10:04:00Z '"2026-11-13T10:05:00Z"' 'Finished')]}"
want "stale pending status" "Finished"
sig=$(cr_head_signal test/repo deadbeef); [ "${sig%%$'\t'*}" = "2026-11-13T10:05:00Z" ] || fail "completion lost its timestamp: $sig"
# Settled tie: the check-run (current channel) wins.
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review completed","updated_at":"2026-11-13T07:00:00Z"}]}' \
  "{\"check_runs\":[$(run 5 completed '"skipped"' 2026-11-13T06:59:00Z '"2026-11-13T07:00:00Z"' 'Review skipped: y')]}"
want "settled tie" "Review skipped: y"
# A settled status with no timestamp counts as newest.
set_case '{"statuses":[{"context":"CodeRabbit","description":"Review rate limited"}]}' \
  "{\"check_runs\":[$(run 5 completed '"success"' 2026-11-13T06:59:00Z '"2026-11-13T07:00:00Z"' 'Finished')]}"
want "undated status" "Review rate limited"

# Another app's run is ignored; nothing from CodeRabbit is absent ("").
set_case '{"statuses":[{"context":"ci","description":"ok","updated_at":"2026-11-13T06:00:00Z"}]}' \
  '{"check_runs":[{"id":1,"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","completed_at":"2026-11-13T06:00:00Z","output":{"title":"Review skipped: not CR"}}]}'
want "absent" ""

# Fail closed: an unreadable check-runs answer or a garbage status is rc 1.
set_case '{"statuses":[]}' ''
if cr_head_desc test/repo deadbeef >/dev/null; then fail "unreadable check-runs was not rc 1"; fi
set_case 'garbage' '{"check_runs":[]}'
if cr_head_desc test/repo deadbeef >/dev/null; then fail "garbage status was not rc 1"; fi

echo "PASS cr-signal: check-run skip, legacy status, newer channel wins, in progress, pending never hidden, ties to check-run, undated newest, other apps ignored, fail closed"
