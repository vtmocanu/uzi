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
    *'graphql'*)
      if [ "$MODE" = cr_resolved ]; then
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":true,"isOutdated":false,"comments":{"nodes":[{"databaseId":12,"author":{"login":"coderabbitai"},"body":"🟡 **resolved finding**","path":"resolved.go","line":8,"originalLine":8}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}'
      else
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}'
      fi ;;
    *'/pulls/42/reviews'*)
      case "$MODE" in
        race) echo '[{"id":7,"user":{"login":"greptile-apps[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"COMMENTED","body":""}]' ;;
        in_progress) echo '[{"id":8,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]' ;;
        cr_resolved) echo '[{"id":9,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/issues/42/comments'*) echo '[]' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/status'*) echo '' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*)
      case "$MODE" in
        race) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 1 comments added"}}]}' ;;
        in_progress) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"in_progress","conclusion":null,"output":{"summary":""}}]}' ;;
        *) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added"}}]}' ;;
      esac ;;
    *'/pulls/42/comments'*)
      case "$MODE" in
        race) echo '[]' ;;
        in_progress) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"partial.go","line":9,"body":"<img alt=\"P1\"> partial finding","pull_request_review_id":101}]' ;;
        cr_resolved) echo '[{"user":{"login":"coderabbitai[bot]"},"path":"resolved.go","line":8,"body":"🟡 **resolved finding**","pull_request_review_id":9}]' ;;
        *) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"old.go","line":8,"body":"<img alt=\"P1\"> old addressed finding","pull_request_review_id":99}]' ;;
      esac ;;
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

MODE="in_progress"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/progress.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "in-progress review exited rc=$rc, want 3: $(cat "$WORK/progress.out")"
grep -q 'review still in progress; findings deferred' "$WORK/progress.out" || fail "in-progress review was not deferred"
if grep -q '^  GR  ' "$WORK/progress.out"; then fail "partial in-progress finding was printed"; fi

MODE="cr_resolved"; export MODE
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/cr-resolved.out" 2>&1 \
  || fail "resolved CR thread did not satisfy the gate: $(cat "$WORK/cr-resolved.out")"
if grep -q '^  CR  ' "$WORK/cr-resolved.out"; then fail "resolved CR thread was printed live"; fi

echo "PASS pr-findings: settled, resolved current-head scope"
