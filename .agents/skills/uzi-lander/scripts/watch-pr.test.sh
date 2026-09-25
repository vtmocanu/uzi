#!/usr/bin/env bash
# Hermetic regression for CodeRabbit's "already reviewed; use full review" terminal reply.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/watch-pr.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then echo '[]'; exit 0; fi
echo "unexpected uzi call: $*" >&2
exit 1
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then
  case "$*" in
    *'--json headRefOid,state'*) echo '{"headRefOid":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"OPEN"}' ;;
    *'-q .headRefOid'*) echo deadbeefdeadbeefdeadbeefdeadbeefdeadbeef ;;
    *'-q .baseRefName'*) echo main ;;
    *) echo "unexpected pr view: $*" >&2; exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = pr ] && [ "${2:-}" = checks ]; then
  echo '[{"bucket":"pass"}]'
  exit 0
fi
if [ "${1:-}" = api ]; then
  case "$*" in
    *'graphql'*)
      if [ "$MODE" = pending_findings ]; then
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"databaseId":11,"author":{"login":"coderabbitai"},"body":"🟡 **partial finding**","path":"partial.go","line":7,"originalLine":7}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}'
      elif [ "$MODE" = cr_ca_findings ]; then
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"databaseId":21,"author":{"login":"coderabbitai"},"body":"🟠 **carried finding**","path":"ca.go","line":3,"originalLine":3}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}'
      elif [ "$MODE" = cr_resolved ]; then
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":true,"isOutdated":false,"comments":{"nodes":[{"databaseId":12,"author":{"login":"coderabbitai"},"body":"🟡 **resolved finding**","path":"resolved.go","line":8,"originalLine":8}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}'
      else
        echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}'
      fi ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/status'*)
      case "$MODE" in
        pending_findings|revovr_cr_pending) echo '{"statuses":[{"context":"CodeRabbit","description":"Review in progress"}]}' ;;
        cr_limited) echo '{"statuses":[{"context":"CodeRabbit","description":"Review rate limited","updated_at":"2026-09-23T10:00:00Z"}]}' ;;
        greptile_*|prior_*|head_*) echo '{"statuses":[]}' ;;
        *) echo '{"statuses":[{"context":"CodeRabbit","description":"Review completed"}]}' ;;
      esac ;;
    *'/pulls/42/reviews'*)
      case "$MODE" in
        pending_findings) echo '[{"id":1,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]' ;;
        greptile_race) echo '[{"id":7,"user":{"login":"greptile-apps[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"COMMENTED","body":""}]' ;;
        prior_headreview|head_two_runs) echo '[{"id":77,"user":{"login":"greptile-apps[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"COMMENTED","body":""}]' ;;
        prior_findings) echo '[{"id":44,"user":{"login":"greptile-apps[bot]"},"commit_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"COMMENTED","body":""},{"id":55,"user":{"login":"greptile-apps[bot]"},"commit_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","state":"COMMENTED","body":""}]' ;;
        cr_resolved) echo '[{"id":9,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/issues/42/comments'*) cat "$COMMENTS" ;;
    *'/pulls/42/comments'*)
      case "$MODE" in
        pending_findings) echo '[{"user":{"login":"coderabbitai[bot]"},"line":7,"body":"🟡 **partial finding**","pull_request_review_id":1}]' ;;
        prior_headreview|head_two_runs) echo '[{"user":{"login":"greptile-apps[bot]"},"line":8,"body":"<img alt=\"P1\"> current head finding","pull_request_review_id":77}]' ;;
        prior_clean|prior_none|prior_unreadable|prior_pending|prior_unparseable|head_unreadable) echo '[{"user":{"login":"greptile-apps[bot]"},"line":8,"body":"<img alt=\"P1\"> old addressed finding","pull_request_review_id":99}]' ;;
        prior_findings) echo '[{"user":{"login":"greptile-apps[bot]"},"line":8,"body":"<img alt=\"P1\"> superseded finding","pull_request_review_id":44},{"user":{"login":"greptile-apps[bot]"},"line":9,"body":"<img alt=\"P1\"> still open finding","pull_request_review_id":55}]' ;;
        greptile_clean) echo '[{"user":{"login":"greptile-apps[bot]"},"line":8,"body":"<img alt=\"P1\"> old addressed finding","pull_request_review_id":99}]' ;;
        cr_resolved) echo '[{"user":{"login":"coderabbitai[bot]"},"line":8,"body":"🟡 **resolved finding**","pull_request_review_id":9}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/pulls/42/commits'*)
      [ "$MODE" = prior_unreadable ] && exit 1
      echo '[{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}]' ;;
    *'/commits/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/check-runs'*)
      case "$MODE" in
        prior_clean|prior_headreview|prior_unparseable|head_unreadable) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
        prior_pending) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"in_progress","conclusion":null,"output":{"summary":""}}]}' ;;
        prior_findings) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 1 comments added"}}]}' ;;
        *) echo '{"check_runs":[{"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","output":{"summary":""}}]}' ;;
      esac ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*)
      case "$MODE" in
        greptile_clean|revovr_cr_pending) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added"}}]}' ;;
        greptile_race|greptile_outside) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 1 comments added"}}]}' ;;
        greptile_outside_clean) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added"}}]}' ;;
        prior_unparseable) echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Reviewed 90 files and raised 3 issues"}}]}' ;;
        head_unreadable) exit 1 ;;
        # Newest first, as the API lists them: the re-trigger (id 2) found something the first run did not.
        head_two_runs) echo '{"check_runs":[{"id":2,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 1 comments added"}},{"id":1,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
        *) echo '{"check_runs":[]}' ;;
      esac ;;
    *) echo "unexpected gh api: $*" >&2; exit 1 ;;
  esac
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi" "$WORK/bin/sleep"
export PATH="$WORK/bin:$PATH"
export COMMENTS="$WORK/comments.json"
MODE="full"; export MODE

jq -n '[
  {user:{login:"tester"},body:"@coderabbitai review",created_at:"2026-09-18T11:12:00Z"},
  {user:{login:"coderabbitai[bot]"},body:"Action not completed: Already reviewed the last commit. Use @coderabbitai full review to rerun a review of the entire changeset.",created_at:"2026-09-18T11:12:56Z"}
]' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/required.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 7 ] || fail "missed full-review reply, rc=$rc: $(cat "$WORK/required.out")"
grep -q "^RESULT=cr_full_review_required command='@coderabbitai full review'$" "$WORK/required.out" \
  || fail "full-review action was not surfaced: $(cat "$WORK/required.out")"

# Once the prescribed command was posted, the same old reply must not request it again.
jq '. + [{user:{login:"tester"},body:"@coderabbitai full review",created_at:"2026-09-18T11:13:00Z"}]' "$COMMENTS" > "$WORK/comments.next"
mv "$WORK/comments.next" "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/already.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "posted full-review command was not suppressed, rc=$rc: $(cat "$WORK/already.out")"
if grep -q '^RESULT=cr_full_review_required' "$WORK/already.out"; then fail "full-review command would be duplicated"; fi

# A rate-limited status with no newer trigger acknowledgement is a real rate limit (exit 5).
MODE="cr_limited"; export MODE
printf '[]\n' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/limited.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 5 ] || fail "a genuine rate limit was not reported, rc=$rc: $(cat "$WORK/limited.out")"
# A "Review triggered." ack NEWER than that status means the review was accepted and the
# status is stale: keep polling (timeout at max 1 try), never exit 5 (#1574).
jq -n '[
  {user:{login:"tester"},body:"@coderabbitai review",created_at:"2026-09-23T10:31:39Z"},
  {user:{login:"coderabbitai[bot]"},body:"<details><summary>Action performed</summary>\n\nReview triggered.\n\n</details>",created_at:"2026-09-23T10:31:45Z"}
]' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/limited-stale.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "a stale rate-limit status overrode a newer review trigger, rc=$rc: $(cat "$WORK/limited-stale.out")"
grep -q 'superseded by review triggered' "$WORK/limited-stale.out" || fail "stale rate-limit reading was not visible: $(cat "$WORK/limited-stale.out")"
# An ack OLDER than the rate-limit status (a second refusal after the trigger) stays a rate limit.
jq -n '[
  {user:{login:"coderabbitai[bot]"},body:"Review triggered.",created_at:"2026-09-23T09:59:00Z"}
]' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/limited-old-ack.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 5 ] || fail "an ack older than the rate-limit status suppressed it, rc=$rc: $(cat "$WORK/limited-old-ack.out")"
# Findings are incomplete while a selected reviewer is still in progress; wait, do not edit.
MODE="pending_findings"; export MODE
printf '[]\n' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/pending.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "in-progress review surfaced partial findings, rc=$rc: $(cat "$WORK/pending.out")"
if grep -q '^RESULT=findings' "$WORK/pending.out"; then fail "partial findings became actionable"; fi

# A current-head clean Greptile check is authoritative over old still-anchored comments.
MODE="greptile_clean"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/greptile.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "clean current Greptile review counted an old finding, rc=$rc: $(cat "$WORK/greptile.out")"
grep -q '^RESULT=ready$' "$WORK/greptile.out" || fail "clean Greptile review did not reach ready"

# The completed check/review may arrive before its inline comments; that is unknown, not clean.
MODE="greptile_race"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/greptile-race.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "incomplete Greptile findings read ready, rc=$rc: $(cat "$WORK/greptile-race.out")"
grep -q 'gr_scope=0+0od/1.*unknown=1' "$WORK/greptile-race.out" || fail "incomplete Greptile scope was not visible"

# Greptile posts findings on lines outside the diff as ONE issue comment, with no review
# object; its tally counts them. They are live findings, not an unscopable (unknown) set.
cp "$COMMENTS" "$WORK/comments.saved"
jq -n '[{id:900,user:{login:"greptile-apps[bot]"},created_at:"2026-09-25T10:51:43Z",
  body:"<!-- greptile_outside_diff -->\n\n<h3>Comments Outside Diff</h3>\n\n- <img alt=\"P1\" src=\"x\">&nbsp;**Halt alert can be lost** `api/x.go:252` <a href=\"https://github.com/test/repo/blob/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/api/x.go#L252\">▶</a>\n\n  detail"}]' > "$COMMENTS"
MODE="greptile_outside"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/greptile-outside.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "outside-diff Greptile finding was not live, rc=$rc: $(cat "$WORK/greptile-outside.out")"
grep -q 'gr_scope=0+1od/1' "$WORK/greptile-outside.out" || fail "outside-diff tally not reconciled: $(cat "$WORK/greptile-outside.out")"
grep -q 'unknown=1' "$WORK/greptile-outside.out" && fail "outside-diff finding read as unknown: $(cat "$WORK/greptile-outside.out")"

# A clean current-head pass (0 comments added) speaks for the whole diff: a stale
# outside-diff bullet left in the comment does not block.
MODE="greptile_outside_clean"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/greptile-outside-clean.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "clean Greptile pass blocked on a stale outside-diff bullet, rc=$rc: $(cat "$WORK/greptile-outside-clean.out")"
cp "$WORK/comments.saved" "$COMMENTS"

# A GraphQL-resolved CodeRabbit thread is not live even if its REST comment stays anchored.
MODE="cr_resolved"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/cr-resolved.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "resolved CR thread stayed live, rc=$rc: $(cat "$WORK/cr-resolved.out")"
grep -q '^RESULT=ready$' "$WORK/cr-resolved.out" || fail "resolved CR thread did not reach ready"

# A push Greptile never reviewed re-anchors its older comments onto the new head. A finding
# a LATER Greptile pass already cleared must not come back as live (PR #1449): with the local
# review lane (--reviewer none) the head reaches ready on the earlier clean verdict.
MODE="prior_clean"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-clean.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "superseded Greptile finding stayed live on an unreviewed head, rc=$rc: $(cat "$WORK/prior-clean.out")"
grep -q 'gr_prior=bbbbbbbb/0.*live=0' "$WORK/prior-clean.out" || fail "earlier clean verdict was not shown: $(cat "$WORK/prior-clean.out")"

# ...but it never satisfies the reviewer gate: the same head is still unreviewed by Greptile.
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/prior-gate.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 6 ] || fail "an earlier verdict satisfied the exact-head reviewer gate, rc=$rc: $(cat "$WORK/prior-gate.out")"

# An earlier verdict that ADDED comments keeps exactly its own, and drops the older pass's.
MODE="prior_findings"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-findings.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "earlier verdict's own finding was lost, rc=$rc: $(cat "$WORK/prior-findings.out")"
grep -q '^RESULT=findings live=1 cr=0 gr=1 ' "$WORK/prior-findings.out" || fail "scoping kept the wrong Greptile findings: $(cat "$WORK/prior-findings.out")"

# No earlier verdict: nothing superseded the comment, so it stays live.
MODE="prior_none"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-none.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "an unsuperseded Greptile finding was dropped, rc=$rc: $(cat "$WORK/prior-none.out")"

# An unreadable history is unknown, never clean.
MODE="prior_unreadable"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-unreadable.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "unreadable Greptile history read as a verdict, rc=$rc: $(cat "$WORK/prior-unreadable.out")"
grep -q 'unknown=1' "$WORK/prior-unreadable.out" || fail "unreadable Greptile history was not marked unknown"

# A Greptile review still RUNNING on a newer commit outranks an older clean verdict: the
# poll defers (unknown), it must not step over the running review and read ready.
MODE="prior_pending"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-pending.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "an in-flight newer Greptile review was stepped over, rc=$rc: $(cat "$WORK/prior-pending.out")"
grep -q 'gr_prior=pending.*unknown=1' "$WORK/prior-pending.out" || fail "pending newer review was not visible: $(cat "$WORK/prior-pending.out")"

# A Greptile review object already ON the head, its check-run not exposed yet: what Greptile
# said about this head stands, an older clean verdict does not speak over it.
MODE="prior_headreview"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-headreview.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "an older verdict overrode a current-head Greptile review, rc=$rc: $(cat "$WORK/prior-headreview.out")"

# A head Greptile DID review, with a summary this script cannot parse: keep the raw count
# rather than let an older clean verdict clear it.
MODE="prior_unparseable"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-unparseable.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "an older verdict overrode an unparseable current-head review, rc=$rc: $(cat "$WORK/prior-unparseable.out")"

# A review REQUESTED after the last verdict outranks it as well: `@greptileai review` only
# becomes a check-run ~12 s later, and until then the head reads `absent`. A lander that
# triggers and immediately watches must not read ready off the verdict it just asked to redo.
MODE="prior_clean"; export MODE
jq -n --arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '[{user:{login:"lander",type:"User"},body:"@greptileai review",created_at:$t}]' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/prior-requested.out" 2>&1
rc=$?
set -e
printf '[]\n' > "$COMMENTS"
[ "$rc" -eq 2 ] || fail "a just-requested Greptile review was ignored for the older verdict, rc=$rc: $(cat "$WORK/prior-requested.out")"
grep -q 'gr_prior=pending.*unknown=1' "$WORK/prior-requested.out" || fail "requested review was not visible: $(cat "$WORK/prior-requested.out")"

# A head check-runs request that FAILED is not "Greptile never ran": no earlier verdict is
# consulted, whatever the separate unknown flag does.
MODE="head_unreadable"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer none > "$WORK/head-unreadable.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "unreadable head check-runs did not defer, rc=$rc: $(cat "$WORK/head-unreadable.out")"
grep -q 'greptile=unreadable' "$WORK/head-unreadable.out" || fail "unreadable head was reported as something else: $(cat "$WORK/head-unreadable.out")"
if grep -q 'gr_prior=' "$WORK/head-unreadable.out"; then fail "an earlier verdict was consulted on an unreadable head: $(cat "$WORK/head-unreadable.out")"; fi
grep -q ' live=1 ' "$WORK/head-unreadable.out" || fail "the anchored finding was cleared on an unreadable head: $(cat "$WORK/head-unreadable.out")"

# Two Greptile runs on the head (a re-trigger needs no push), listed newest first: the
# NEWEST is the verdict. Reading the last entry took the stale clean run and read ready.
MODE="head_two_runs"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/head-two-runs.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "the older clean head run hid the re-trigger's finding, rc=$rc: $(cat "$WORK/head-two-runs.out")"
grep -q 'gr_scope=1+0od/1' "$WORK/head-two-runs.out" || fail "the newest head Greptile run was not the one read: $(cat "$WORK/head-two-runs.out")"

# Signal (e): CodeRabbit dropped the final_review_risk block and marks the reviewed head with
# change_assessment_commit:"<full-sha>" beside a clean recent_review block (#1502). The exact
# head marker must count as reviewed so a genuinely-clean incremental reaches ready.
HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
MODE="cr_ca_clean"; export MODE
jq -n --arg h "$HEAD" '[{user:{login:"coderabbitai[bot]"},body:("<!-- walkthrough_start -->\n<!-- recent_review_start -->\n\nNo actionable comments were generated in the recent review. 🎉\n\n<!-- recent_review_end -->\n\n<!-- change_assessment_commit:\"" + $h + "\" -->"),created_at:"2026-09-21T09:00:00Z"}]' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/ca-clean.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "change_assessment_commit head marker was not counted as reviewed, rc=$rc: $(cat "$WORK/ca-clean.out")"
grep -q '^RESULT=ready$' "$WORK/ca-clean.out" || fail "clean recent_review head did not reach ready: $(cat "$WORK/ca-clean.out")"

# ...and the same marker with a live CodeRabbit thread is reviewed-with-findings, not timeout.
MODE="cr_ca_findings"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/ca-findings.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "change_assessment_commit head with a live thread did not surface findings, rc=$rc: $(cat "$WORK/ca-findings.out")"
grep -q '^RESULT=findings live=1 cr=1 ' "$WORK/ca-findings.out" || fail "the carried CR finding was not counted on the reviewed head: $(cat "$WORK/ca-findings.out")"
printf '[]\n' > "$COMMENTS"

# Reviewer override (--reviewer greptile): a CodeRabbit review still in progress must NOT block
# when Greptile has reviewed the exact head clean — the way to say "ignore CodeRabbit" (#1510).
MODE="revovr_cr_pending"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/revovr-ready.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--reviewer greptile blocked on a CodeRabbit review in progress, rc=$rc: $(cat "$WORK/revovr-ready.out")"
grep -q '^RESULT=ready$' "$WORK/revovr-ready.out" || fail "greptile-clean head did not reach ready while CR was in progress: $(cat "$WORK/revovr-ready.out")"

# ...but the override is scoped to an EXPLICIT bot: --reviewer any still waits for CR in flight.
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer any --reviewer-grace 0 > "$WORK/revovr-any.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "--reviewer any stopped waiting for a CodeRabbit review in progress, rc=$rc: $(cat "$WORK/revovr-any.out")"

echo "PASS watch-pr: settled reviews, resolved-thread scope, earlier-verdict Greptile scope, change_assessment head marker, reviewer override"
