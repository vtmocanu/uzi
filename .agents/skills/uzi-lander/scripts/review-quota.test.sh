#!/usr/bin/env bash
# Hermetic regression: review-quota.sh reads CodeRabbit from its check-run, and an unreadable
# head is counted as UNKNOWN, never as absent.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/review-quota.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin" "$WORK/state"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = pr ] && [ "${2:-}" = list ]; then
  echo '[{"number":1,"title":"a","author":{"login":"dev"},"isDraft":false,"headRefOid":"aaaa","updatedAt":"2026-11-13T00:00:00Z"},{"number":2,"title":"b","author":{"login":"dev"},"isDraft":false,"headRefOid":"bbbb","updatedAt":"2026-11-13T00:00:00Z"}]'
  exit 0
fi
case "$*" in
  *'/commits/'*'/status'*) echo '{"statuses":[]}' ;;
  *'/commits/aaaa/check-runs'*) echo '{"check_runs":[{"id":1,"app":{"slug":"coderabbitai"},"name":"CodeRabbit","status":"completed","conclusion":"neutral","completed_at":"2026-11-13T06:00:00Z","output":{"title":"Review rate limited"}}]}' ;;
  *'/commits/bbbb/check-runs'*) exit 1 ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
echo '[]'
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi"

PATH="$WORK/bin:$PATH" UZI_LANDER_STATE_DIR="$WORK/state" bash "$SCRIPT" test/repo > "$WORK/out" 2>&1 \
  || fail "review-quota exited non-zero: $(cat "$WORK/out")"
grep -q '^CR_LIMITED=1$' "$WORK/out" || fail "check-run rate limit not counted: $(cat "$WORK/out")"
grep -q '^CR_UNKNOWN=1$' "$WORK/out" || fail "unreadable head not counted UNKNOWN: $(cat "$WORK/out")"
grep -q "#2 .*cr='UNKNOWN'" "$WORK/out" || fail "unreadable head not shown UNKNOWN: $(cat "$WORK/out")"

echo "PASS review-quota: check-run rate limit counted, unreadable head UNKNOWN"
