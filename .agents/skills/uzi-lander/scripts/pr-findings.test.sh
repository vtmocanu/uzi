#!/usr/bin/env bash
# Hermetic regression: a clean current-head Greptile review hides older anchored comments.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/pr-findings.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then echo deadbeefdeadbeefdeadbeefdeadbeefdeadbeef; exit 0; fi
if [ "${1:-}" = api ]; then
  case "$*" in
    *'/pulls/42/reviews'*)
      if [ "$MODE" = race ]; then
        echo '[{"id":7,"user":{"login":"greptile-apps[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"COMMENTED","body":""}]'
      else echo '[]'; fi ;;
    *'/issues/42/comments'*) echo '[]' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/status'*) echo '' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*)
      if [ "$MODE" = race ]; then
        echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 1 comments added"}}]}'
      else
        echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added"}}]}'
      fi ;;
    *'/pulls/42/comments'*)
      if [ "$MODE" = race ]; then echo '[]'
      else echo '[{"user":{"login":"greptile-apps[bot]"},"path":"old.go","line":8,"body":"<img alt=\"P1\"> old addressed finding","pull_request_review_id":99}]'; fi ;;
    *) echo "unexpected gh api: $*" >&2; exit 1 ;;
  esac
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh"
MODE="clean"; export MODE

PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/out" 2>&1 \
  || fail "clean Greptile review did not satisfy the gate: $(cat "$WORK/out")"
grep -q 'Greptile: completed on head.*0 comments added' "$WORK/out" \
  || fail "current-head clean Greptile summary missing: $(cat "$WORK/out")"
if grep -q '^  GR  ' "$WORK/out"; then fail "old Greptile comment survived clean current-head review"; fi

MODE="race"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/race.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "incomplete current Greptile findings exited rc=$rc, want 3: $(cat "$WORK/race.out")"
grep -q 'Greptile finding set incomplete (0/1 current-review comments readable)' "$WORK/race.out" \
  || fail "incomplete current Greptile findings were not surfaced"

echo "PASS pr-findings: current-head clean and complete Greptile scope"
