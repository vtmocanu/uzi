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
      elif [ "$MODE" = cr_ca_findings ]; then
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"databaseId":31,"author":{"login":"coderabbitai"},"body":"🟠 **carried finding**","path":"ca.go","line":4,"originalLine":4}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}'
      else
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}'
      fi ;;
    *'/pulls/42/reviews'*)
      case "$MODE" in
        race|od_mixed) echo '[{"id":7,"user":{"login":"greptile-apps[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"COMMENTED","body":""}]' ;;
        in_progress) echo '[{"id":8,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]' ;;
        cr_resolved|head_unreadable) echo '[{"id":9,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]' ;;
        head_two_runs) echo '[{"id":77,"user":{"login":"greptile-apps[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"COMMENTED","body":""}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/issues/42/comments'*)
      case "$MODE" in
        prior_requested|prior_noanchor_requested) printf '[{"user":{"login":"lander","type":"User"},"created_at":"%s","body":"@greptileai review"}]\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" ;;
        issue_unreadable|od_issue_unreadable) exit 1 ;;
        od_only|od_mixed) jq -n --arg h deadbeefdeadbeefdeadbeefdeadbeefdeadbeef '[{id:900,user:{login:"greptile-apps[bot]"},body:("<!-- greptile_outside_diff -->\n\n- <img alt=\"P1\">&nbsp;**Outside bug** `out.go:5` <a href=\"https://x/blob/" + $h + "/out.go#L5\">x</a>")}]' ;;
        cr_ca_clean|cr_ca_findings) echo '[{"user":{"login":"coderabbitai[bot]"},"body":"<!-- walkthrough_start -->\n<!-- recent_review_start -->\nNo actionable comments were generated in the recent review. 🎉\n<!-- recent_review_end -->\n<!-- change_assessment_commit:\"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\" -->"}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/pulls/42/commits'*)
      [ "$MODE" = prior_unreadable ] && exit 1
      echo '[{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}]' ;;
    *'/commits/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/check-runs'*)
      case "$MODE" in
        prior_clean|prior_clean_noanchor|prior_clean_capped|prior_noanchor_requested|head_unreadable|head_failed|prior_requested|issue_unreadable) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
        prior_pending) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"in_progress","conclusion":null,"output":{"summary":""}}]}' ;;
        *) echo '{"check_runs":[{"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","output":{"summary":""}}]}' ;;
      esac ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/status'*) echo '' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*)
      case "$MODE" in
        prior_*|issue_unreadable|cr_ca_clean|cr_ca_findings) echo '{"check_runs":[]}' ;;
        head_unreadable) exit 1 ;;
        head_failed) echo '{"check_runs":[{"id":10,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"failure","output":{"summary":""}}]}' ;;
        # Newest first, as the API lists them: the re-trigger (id 2) found something the first run did not.
        head_two_runs) echo '{"check_runs":[{"id":2,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 1 comments added"}},{"id":1,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
        race|od_only|od_issue_unreadable) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 1 comments added"}}]}' ;;
        od_mixed) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 2 comments added"}}]}' ;;
        in_progress) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"in_progress","conclusion":null,"output":{"summary":""}}]}' ;;
        *) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added"}}]}' ;;
      esac ;;
    *'/pulls/42/comments'*)
      case "$MODE" in
        race|cr_ca_clean|cr_ca_findings|prior_clean_noanchor|prior_clean_capped|prior_noanchor_requested) echo '[]' ;;
        od_mixed) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"in.go","line":3,"body":"<img alt=\"P2\"> inline finding","pull_request_review_id":7},{"user":{"login":"greptile-apps[bot]"},"path":"old.go","line":8,"body":"<img alt=\"P1\"> old addressed finding","pull_request_review_id":99}]' ;;
        in_progress) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"partial.go","line":9,"body":"<img alt=\"P1\"> partial finding","pull_request_review_id":101}]' ;;
        head_two_runs) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"retrigger.go","line":8,"body":"<img alt=\"P1\"> found by the re-trigger","pull_request_review_id":77}]' ;;
        cr_resolved) echo '[{"user":{"login":"coderabbitai[bot]"},"path":"resolved.go","line":8,"body":"🟡 **resolved finding**","pull_request_review_id":9}]' ;;
        *) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"old.go","line":8,"body":"<img alt=\"P1\"> old addressed finding","pull_request_review_id":99}]' ;;
      esac ;;
    *'/compare/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb...deadbeefdeadbeefdeadbeefdeadbeefdeadbeef'*)
      if [ "$MODE" = prior_clean_capped ]; then jq -nc '{files:[range(300)|{filename:"docs/f\(.).md"}]}'
      else echo '{"files":[{"filename":"docs/a.md"},{"filename":"api/internal/uzidocs/embed/a.md"}]}'; fi ;;
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

# Greptile's head pass added ONE comment, outside the diff: an issue comment, no review
# object. The tally reconciles, the finding is listed, the older inline comment is superseded.
MODE="od_only"; export MODE
set +e; PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/od-only.out" 2>&1; rc=$?; set -e
[ "$rc" -eq 0 ] || fail "outside-diff-only pass was not confirmed, rc=$rc: $(cat "$WORK/od-only.out")"
grep -qF '  GR  out.go:5  [P1] Outside bug (outside diff)' "$WORK/od-only.out" || fail "outside-diff finding not listed: $(cat "$WORK/od-only.out")"
if grep -q 'review id is missing' "$WORK/od-only.out"; then fail "outside-diff tally still read as unscopable"; fi
if grep -q '^  GR  old.go:8' "$WORK/od-only.out"; then fail "superseded inline comment listed: $(cat "$WORK/od-only.out")"; fi

# Mixed: one inline (scoped to the head review) plus one outside the diff = the tally of 2.
MODE="od_mixed"; export MODE
set +e; PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/od-mixed.out" 2>&1; rc=$?; set -e
[ "$rc" -eq 0 ] || fail "mixed inline/outside pass was not confirmed, rc=$rc: $(cat "$WORK/od-mixed.out")"
grep -qF '  GR  in.go:3' "$WORK/od-mixed.out" || fail "inline finding missing: $(cat "$WORK/od-mixed.out")"
grep -qF '  GR  out.go:5  [P1] Outside bug (outside diff)' "$WORK/od-mixed.out" || fail "outside finding missing: $(cat "$WORK/od-mixed.out")"
if grep -q '^  GR  old.go:8' "$WORK/od-mixed.out"; then fail "older-review comment listed: $(cat "$WORK/od-mixed.out")"; fi

# Unreadable issue comments on a reviewed head: outside-diff findings unknown, never clean.
MODE="od_issue_unreadable"; export MODE
set +e; PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/od-unread.out" 2>&1; rc=$?; set -e
[ "$rc" -eq 3 ] || fail "unreadable issue comments read clean, rc=$rc: $(cat "$WORK/od-unread.out")"
grep -q 'outside-diff findings unknown' "$WORK/od-unread.out" || fail "unreadable outside-diff not surfaced: $(cat "$WORK/od-unread.out")"

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

# A clean earlier pass posts no comments, so nothing is anchored and the scoping above never
# runs. The earlier verdict and the delta since must still be named (a docs-only push after a
# clean Greptile pass read as "not triggered"), and the head must STILL read unreviewed.
MODE="prior_clean_noanchor"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-noanchor.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "an earlier clean verdict with no anchored comments cleared the gate, rc=$rc: $(cat "$WORK/prior-noanchor.out")"
grep -q 'NOT REVIEWED on head by any bot' "$WORK/prior-noanchor.out" || fail "unreviewed head was not reported: $(cat "$WORK/prior-noanchor.out")"
grep -q 'Greptile: no verdict on this head; last verdict on bbbbbbbb — 0 comments added. Changed since: docs/a.md, api/internal/uzidocs/embed/a.md' "$WORK/prior-noanchor.out" \
  || fail "earlier clean verdict and delta were not named: $(cat "$WORK/prior-noanchor.out")"

# GitHub's compare lists at most 300 files: a full page must read INCOMPLETE, never as a
# docs-only list that could hide code past file 300.
MODE="prior_clean_capped"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-capped.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "capped delta cleared the gate, rc=$rc: $(cat "$WORK/prior-capped.out")"
grep -q 'Changed since: INCOMPLETE (GitHub lists at most 300 files' "$WORK/prior-capped.out" \
  || fail "a 300-file compare was printed as exhaustive: $(cat "$WORK/prior-capped.out")"
if grep -q 'docs/f0.md' "$WORK/prior-capped.out"; then fail "capped file list was printed: $(cat "$WORK/prior-capped.out")"; fi

# A review requested after the earlier verdict may not have a check-run yet: say it may be pending.
MODE="prior_noanchor_requested"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/prior-noanchor-req.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "requested-after-verdict head cleared the gate, rc=$rc: $(cat "$WORK/prior-noanchor-req.out")"
grep -q 'a review was requested after that verdict' "$WORK/prior-noanchor-req.out" \
  || fail "pending Greptile request was not noted: $(cat "$WORK/prior-noanchor-req.out")"
if grep -q 'a review was requested after that verdict' "$WORK/prior-noanchor.out"; then fail "pending note shown with no request: $(cat "$WORK/prior-noanchor.out")"; fi

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

# Signal (e): CodeRabbit dropped the final_review_risk block and marks the reviewed head with
# change_assessment_commit:"<sha>" beside a clean recent_review block (#1502). pr-findings must
# recognize the exact-head marker as reviewed, not report "NOT REVIEWED" on a clean incremental.
MODE="cr_ca_clean"; export MODE
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/ca-clean.out" 2>&1 \
  || fail "change_assessment_commit head marker did not satisfy the gate: $(cat "$WORK/ca-clean.out")"
grep -q 'incremental pass covered the head' "$WORK/ca-clean.out" || fail "change_assessment head marker not recognized as reviewed: $(cat "$WORK/ca-clean.out")"
if grep -q 'NOT REVIEWED on head by any bot' "$WORK/ca-clean.out"; then fail "a clean change_assessment head read as unreviewed: $(cat "$WORK/ca-clean.out")"; fi

# ...and with a live CodeRabbit thread the same head is reviewed-with-findings: the finding is
# listed as current (not the stale-carried note), and the head is still recognized as reviewed.
MODE="cr_ca_findings"; export MODE
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/repo 42 > "$WORK/ca-findings.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "change_assessment head with a live CR thread exited rc=$rc, want 0: $(cat "$WORK/ca-findings.out")"
grep -q 'incremental pass covered the head' "$WORK/ca-findings.out" || fail "change_assessment head not recognized as reviewed with findings: $(cat "$WORK/ca-findings.out")"
grep -q '^  CR  ca.go:4' "$WORK/ca-findings.out" || fail "the current-head CR finding was not listed: $(cat "$WORK/ca-findings.out")"
if grep -q 'carried from an earlier review' "$WORK/ca-findings.out"; then fail "a current-head CR finding was mislabeled as carried/stale: $(cat "$WORK/ca-findings.out")"; fi

echo "PASS pr-findings: settled, resolved current-head scope, earlier-verdict Greptile scope, change_assessment head marker"
