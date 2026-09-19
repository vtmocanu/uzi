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
    *'/pulls/42/commits'*)
      [ "$MODE" = prior_unreadable ] && exit 1
      echo '[{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}]' ;;
    *'/commits/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/check-runs'*)
      case "$MODE" in
        prior_clean) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
        *) echo '{"check_runs":[{"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","output":{"summary":""}}]}' ;;
      esac ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/status'*) echo '' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*)
      case "$MODE" in
        prior_*) echo '{"check_runs":[]}' ;;
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

# A push Greptile never reviewed re-anchors its older comments onto the new head. A comment
# a LATER clean Greptile pass superseded must not be listed as live again (PR #1449), and the
# head must STILL read unreviewed: an earlier verdict scopes findings, never the gate.
MODE="prior_clean"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-clean.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "an earlier Greptile verdict cleared the exact-head gate, rc=$rc: $(cat "$WORK/prior-clean.out")"
grep -q 'NOT REVIEWED on head by any bot' "$WORK/prior-clean.out" || fail "unreviewed head was not reported: $(cat "$WORK/prior-clean.out")"
grep -q 'Greptile: last verdict on bbbbbbbb — 0 comments added' "$WORK/prior-clean.out" || fail "earlier clean verdict was not shown: $(cat "$WORK/prior-clean.out")"
if grep -q '^  GR  ' "$WORK/prior-clean.out"; then fail "superseded Greptile comment was listed as live: $(cat "$WORK/prior-clean.out")"; fi

# No earlier verdict: nothing superseded the comment, so it is still listed.
MODE="prior_none"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-none.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "unreviewed head exited rc=$rc, want 3: $(cat "$WORK/prior-none.out")"
grep -q '^  GR  old.go:8' "$WORK/prior-none.out" || fail "an unsuperseded Greptile comment was dropped: $(cat "$WORK/prior-none.out")"

# An unreadable history is never "clean": it is surfaced and the comment stays listed.
MODE="prior_unreadable"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-unreadable.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "unreadable Greptile history exited rc=$rc, want 3: $(cat "$WORK/prior-unreadable.out")"
grep -q "earlier verdict UNREADABLE" "$WORK/prior-unreadable.out" || fail "unreadable Greptile history was not surfaced: $(cat "$WORK/prior-unreadable.out")"
grep -q '^  GR  old.go:8' "$WORK/prior-unreadable.out" || fail "comment hidden on an unreadable history: $(cat "$WORK/prior-unreadable.out")"

echo "PASS pr-findings: settled, resolved current-head scope, earlier-verdict Greptile scope"
