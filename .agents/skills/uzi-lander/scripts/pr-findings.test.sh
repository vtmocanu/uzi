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
        cr_resolved|head_unreadable) echo '[{"id":9,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]' ;;
        head_two_runs) echo '[{"id":77,"user":{"login":"greptile-apps[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"COMMENTED","body":""}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/issues/42/comments'*)
      case "$MODE" in
        prior_requested) printf '[{"user":{"login":"lander","type":"User"},"created_at":"%s","body":"@greptileai review"}]\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" ;;
        issue_unreadable) exit 1 ;;
        *) echo '[]' ;;
      esac ;;
    *'/pulls/42/commits'*)
      [ "$MODE" = prior_unreadable ] && exit 1
      echo '[{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}]' ;;
    *'/commits/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/check-runs'*)
      case "$MODE" in
        prior_clean|head_unreadable|head_failed|prior_requested|issue_unreadable) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
        prior_pending) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"in_progress","conclusion":null,"output":{"summary":""}}]}' ;;
        *) echo '{"check_runs":[{"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","output":{"summary":""}}]}' ;;
      esac ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/status'*) echo '' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*)
      case "$MODE" in
        prior_*|issue_unreadable) echo '{"check_runs":[]}' ;;
        head_unreadable) exit 1 ;;
        head_failed) echo '{"check_runs":[{"id":10,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"failure","output":{"summary":""}}]}' ;;
        # Newest first, as the API lists them: the re-trigger (id 2) found something the first run did not.
        head_two_runs) echo '{"check_runs":[{"id":2,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 1 comments added"}},{"id":1,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
        race) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 1 comments added"}}]}' ;;
        in_progress) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"in_progress","conclusion":null,"output":{"summary":""}}]}' ;;
        *) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added"}}]}' ;;
      esac ;;
    *'/pulls/42/comments'*)
      case "$MODE" in
        race) echo '[]' ;;
        in_progress) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"partial.go","line":9,"body":"<img alt=\"P1\"> partial finding","pull_request_review_id":101}]' ;;
        head_two_runs) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"retrigger.go","line":8,"body":"<img alt=\"P1\"> found by the re-trigger","pull_request_review_id":77}]' ;;
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

# A Greptile review still RUNNING on a newer commit outranks an older verdict: deferred, and
# the comment stays listed rather than being cleared by the verdict behind it.
MODE="prior_pending"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-pending.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "in-flight newer Greptile review exited rc=$rc, want 3: $(cat "$WORK/prior-pending.out")"
grep -q 'after its last verdict; findings deferred' "$WORK/prior-pending.out" || fail "in-flight newer Greptile review was not surfaced: $(cat "$WORK/prior-pending.out")"
grep -q '^  GR  old.go:8' "$WORK/prior-pending.out" || fail "comment cleared while a newer review was running: $(cat "$WORK/prior-pending.out")"

# A review REQUESTED after the last verdict outranks it: `@greptileai review` only becomes a
# check-run ~12 s later, so until then the comment stays listed and the PR is deferred.
MODE="prior_requested"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-requested.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "just-requested Greptile review exited rc=$rc, want 3: $(cat "$WORK/prior-requested.out")"
grep -q 'after its last verdict; findings deferred' "$WORK/prior-requested.out" || fail "just-requested Greptile review was not deferred: $(cat "$WORK/prior-requested.out")"
grep -q '^  GR  old.go:8' "$WORK/prior-requested.out" || fail "comment cleared while a review was requested: $(cat "$WORK/prior-requested.out")"

# An issue-comments listing that could not be READ is not "nobody asked for a review": the
# trigger check fails closed, so the comment stays listed and the PR is unconfirmed.
MODE="issue_unreadable"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/issue-unreadable.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "unreadable issue comments exited rc=$rc, want 3: $(cat "$WORK/issue-unreadable.out")"
grep -q 'earlier verdict UNREADABLE' "$WORK/issue-unreadable.out" || fail "unreadable issue comments were not surfaced: $(cat "$WORK/issue-unreadable.out")"
grep -q '^  GR  old.go:8' "$WORK/issue-unreadable.out" || fail "comment cleared on unreadable issue comments: $(cat "$WORK/issue-unreadable.out")"

# A head check-runs request that FAILED is not "Greptile never ran": with CodeRabbit having
# approved the head, treating it as `absent` let a clean earlier verdict drop a real anchored
# finding from the listing at exit 0.
MODE="head_unreadable"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/head-unreadable.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "unreadable head check-runs exited rc=$rc, want 3: $(cat "$WORK/head-unreadable.out")"
grep -q 'check-runs on this head UNREADABLE' "$WORK/head-unreadable.out" || fail "unreadable head check-runs were not surfaced: $(cat "$WORK/head-unreadable.out")"
grep -q '^  GR  old.go:8' "$WORK/head-unreadable.out" || fail "finding dropped on an unreadable head: $(cat "$WORK/head-unreadable.out")"
if grep -q 'last verdict on' "$WORK/head-unreadable.out"; then fail "an earlier verdict was consulted on an unreadable head: $(cat "$WORK/head-unreadable.out")"; fi

# A Greptile run that FAILED on the head is Greptile evidence about this head: the raw count
# stands, an older clean verdict does not clear it.
MODE="head_failed"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/head-failed.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "failed head Greptile run exited rc=$rc, want 3: $(cat "$WORK/head-failed.out")"
grep -q '^  GR  old.go:8' "$WORK/head-failed.out" || fail "an older verdict overrode a failed head Greptile run: $(cat "$WORK/head-failed.out")"
if grep -q 'last verdict on' "$WORK/head-failed.out"; then fail "an earlier verdict was consulted past a failed head run: $(cat "$WORK/head-failed.out")"; fi

# Two Greptile runs on the head (a re-trigger needs no push), listed newest first: the
# NEWEST is the verdict. Reading the last entry took the stale clean run and hid the finding.
MODE="head_two_runs"; export MODE
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/head-two-runs.out" 2>&1 \
  || fail "two head Greptile runs did not satisfy the gate: $(cat "$WORK/head-two-runs.out")"
grep -q 'Greptile: completed on head.*1 comments added' "$WORK/head-two-runs.out" || fail "the newest head Greptile run was not the one read: $(cat "$WORK/head-two-runs.out")"
grep -q '^  GR  retrigger.go:8' "$WORK/head-two-runs.out" || fail "the re-trigger's finding was hidden by the older clean run: $(cat "$WORK/head-two-runs.out")"

echo "PASS pr-findings: settled, resolved current-head scope, earlier-verdict Greptile scope"
