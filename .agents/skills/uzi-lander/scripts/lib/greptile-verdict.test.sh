#!/usr/bin/env bash
# Hermetic regression: Greptile's newest EARLIER verdict scopes finding liveness on a head
# it never reviewed; anything Greptile said or is saying about a newer commit outranks it;
# and every unreadable or unscopable answer fails closed.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=greptile-verdict.sh
. "$HERE/greptile-verdict.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

HEAD_SHA=cccccccccccccccccccccccccccccccccccccccc
PREV_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
OLD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
HEAD_SHA=cccccccccccccccccccccccccccccccccccccccc
PREV_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
OLD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
echo "$*" >> "$CALLS"
run() { printf '{"id":%s,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"%s","conclusion":%s,"started_at":"%s","output":{"summary":"%s"}}' "$1" "$2" "$3" "${5:-2001-01-01T00:00:00Z}" "$4"; }
page() { printf '{"check_runs":[%s]}\n' "$1"; }
review() { page "$(run 10 completed '"success"' "Greptile has reviewed the Pull Request.\\n\\n90 files reviewed, $1 comments added.")"; }
nothing() { echo '{"check_runs":[{"id":5,"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","output":{"summary":""}}]}'; }
[ "${1:-}" = api ] || { echo "unexpected gh call: $*" >&2; exit 1; }
[ "$MODE" = no_api ] && { echo "the helper called the API when it must not: $*" >&2; exit 1; }
case "$*" in
  *'/pulls/44/commits'*) exit 1 ;;
  *'/pulls/43/commits'*)
    # A different PR that happens to share the head SHA and has no earlier commits.
    printf '[{"sha":"%s"}]\n' "$HEAD_SHA" ;;
  *'/pulls/42/commits'*)
    [ "$MODE" = commits_fail ] && exit 1
    # Oldest first, as the API returns them; the head is last.
    if [ "$MODE" = truncated ]; then printf '[{"sha":"%s"},{"sha":"%s"}]\n' "$OLD_SHA" "$PREV_SHA"
    else printf '[{"sha":"%s"},{"sha":"%s"},{"sha":"%s"}]\n' "$OLD_SHA" "$PREV_SHA" "$HEAD_SHA"; fi ;;
  *"/commits/$HEAD_SHA/check-runs"*)
    # The helper answers for EARLIER commits only; the callers own the head.
    echo "helper queried the head's check-runs" >&2; exit 1 ;;
  *"/commits/$PREV_SHA/check-runs"*)
    case "$MODE" in
      prior_clean) review 0 ;;
      # A clean verdict whose run STARTED an hour from now: any trigger posted "now" predates it.
      prior_clean_later) page "$(run 10 completed '"success"' '90 files reviewed, 0 comments added' "$(jq -rn 'now+3600|todate')")" ;;
      prior_findings) review 1 ;;
      unscoped) review 2 ;;
      # A completed check that is not a review: conclusion failure, even carrying a summary.
      skips_nonreview) page "$(run 10 completed '"failure"' '90 files reviewed, 0 comments added')" ;;
      pending_newer) page "$(run 10 in_progress null '')" ;;
      queued_newer) page "$(run 10 queued null '')" ;;
      # Newest first, as the API lists them: the re-trigger (id 2) found something the
      # first run (id 1) did not. `last` would pick the stale clean run.
      two_runs) page "$(run 2 completed '"success"' '90 files reviewed, 1 comments added'),$(run 1 completed '"success"' '90 files reviewed, 0 comments added')" ;;
      garbage) echo '[]' ;;
      *) nothing ;;
    esac ;;
  *"/commits/$OLD_SHA/check-runs"*)
    case "$MODE" in
      skips_nonreview|window|pending_newer|queued_newer) review 0 ;;
      *) nothing ;;
    esac ;;
  *'/pulls/42/reviews'*)
    case "$MODE" in
      prior_findings|two_runs) printf '[{"id":44,"user":{"login":"greptile-apps[bot]"},"commit_id":"%s"},{"id":55,"user":{"login":"greptile-apps[bot]"},"commit_id":"%s"}]\n' "$OLD_SHA" "$PREV_SHA" ;;
      *) echo '[]' ;;
    esac ;;
  *) echo "unexpected gh api: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH"
export CALLS="$WORK/calls"; : > "$CALLS"

# want RC SHA ADDED REVIEW_ID — a COLD helper call under $MODE, comparing all four.
want() {
  local rc=0
  # shellcheck disable=SC2034  # read by the sourced lib: clearing it forces a cold (unmemoised) call.
  GRV_CACHE_KEY=""
  greptile_prior_verdict test/repo 42 "$HEAD_SHA" || rc=$?
  [ "$rc" -eq "$1" ] || fail "$MODE: rc=$rc, want $1"
  [ "$GRV_SHA" = "$2" ] || fail "$MODE: GRV_SHA='$GRV_SHA', want '$2'"
  [ "$GRV_ADDED" = "$3" ] || fail "$MODE: GRV_ADDED='$GRV_ADDED', want '$3'"
  [ "$GRV_REVIEW_ID" = "$4" ] || fail "$MODE: GRV_REVIEW_ID='$GRV_REVIEW_ID', want '$4'"
}

# ---- greptile_prior_verdict -----------------------------------------------------------
# The PR #1449 shape: fixed finding, clean re-review on PREV, then a push Greptile never saw.
MODE=prior_clean; export MODE
want 0 "$PREV_SHA" 0 ""

# The earlier verdict added comments: they are scoped to ITS review id, not an older one.
MODE=prior_findings; export MODE
want 0 "$PREV_SHA" 1 55

# A completed check that is not a review is skipped, not trusted.
MODE=skips_nonreview; export MODE
want 0 "$OLD_SHA" 0 ""

# A Greptile review still RUNNING on a newer commit must not be stepped over to reach an
# older clean verdict: that turned a deferring poll into a false ready.
MODE=pending_newer; export MODE
want 2 "" "" ""
MODE=queued_newer; export MODE
want 2 "" "" ""

# Two runs on one commit (a re-trigger needs no push): the NEWEST one is the verdict.
MODE=two_runs; export MODE
want 0 "$PREV_SHA" 1 55

# No earlier verdict at all: a trustworthy "none", so callers keep their raw count.
MODE=none; export MODE
want 0 "" "" ""

# The walk is bounded: a verdict outside the window is not found, and that is rc 0.
MODE=window; export MODE
GREPTILE_PRIOR_MAX=1 want 0 "" "" ""
want 0 "$OLD_SHA" 0 ""

# Fail closed: an unreadable commit list, one that does not end at the head (truncated past
# the API's 250-commit cap, or the head moved), an unreadable check-run page, and a verdict
# that added comments with no review object to scope them by.
MODE=commits_fail; export MODE
want 1 "" "" ""
MODE=truncated; export MODE
want 1 "" "" ""
MODE=garbage; export MODE
want 1 "" "" ""
MODE=unscoped; export MODE
want 1 "$PREV_SHA" 2 ""

# A definitive answer is memoised per head (a poll loop must not re-walk every minute);
# a pending or failed one never is.
MODE=prior_clean; export MODE
want 0 "$PREV_SHA" 0 ""
: > "$CALLS"
MODE=no_api; export MODE
rc=0; greptile_prior_verdict test/repo 42 "$HEAD_SHA" || rc=$?
{ [ "$rc" -eq 0 ] && [ "$GRV_SHA" = "$PREV_SHA" ]; } || fail "memoised verdict was not reused (rc=$rc sha=$GRV_SHA)"
[ ! -s "$CALLS" ] || fail "memoised verdict still called the API: $(cat "$CALLS")"
MODE=pending_newer; export MODE
want 2 "" "" ""
MODE=prior_clean; export MODE
rc=0; greptile_prior_verdict test/repo 42 "$HEAD_SHA" || rc=$?
{ [ "$rc" -eq 0 ] && [ "$GRV_SHA" = "$PREV_SHA" ]; } || fail "a pending answer was cached (rc=$rc sha=$GRV_SHA)"

# ---- greptile_scope_live --------------------------------------------------------------
COMMENTS='[{"user":{"login":"greptile-apps[bot]"},"line":8,"pull_request_review_id":44},
           {"user":{"login":"greptile-apps[bot]"},"line":9,"pull_request_review_id":55},
           {"user":{"login":"greptile-apps[bot]"},"line":null,"pull_request_review_id":55}]'

# scope RC LIVE NOTE STATE HEAD_RID RAW [ISSUE_COMMENTS] — a cold call under $MODE.
scope() {
  local rc=0
  # shellcheck disable=SC2034  # read by the sourced lib: clearing it forces a cold (unmemoised) call.
  GRV_CACHE_KEY=""
  greptile_scope_live test/repo 42 "$HEAD_SHA" "$4" "$5" "$6" "$COMMENTS" "${7:-[]}" || rc=$?
  [ "$rc" -eq "$1" ] || fail "scope $MODE/$4/rid=$5: rc=$rc, want $1"
  [ "$GRL_LIVE" = "$2" ] || fail "scope $MODE/$4/rid=$5: GRL_LIVE='$GRL_LIVE', want '$2'"
  [ "$GRL_NOTE" = "$3" ] || fail "scope $MODE/$4/rid=$5: GRL_NOTE='$GRL_NOTE', want '$3'"
}

# Nothing anchored: decided without touching the API.
MODE=no_api; export MODE
scope 0 0 "" absent "" 0
# Anything Greptile said or is saying about THIS head outranks an older verdict, so the raw
# count stands and the API is never asked: a run in flight, a completed run of any kind
# (a failure, or a success whose summary this script cannot parse), or a head review object
# whose check-run is not exposed yet.
scope 0 2 "" in_progress "" 2
scope 0 2 "" queued "" 2
scope 0 2 "" completed "" 2
scope 0 2 "" absent 77 2

# A head with no Greptile evidence at all: the earlier verdict decides.
MODE=prior_clean; export MODE
scope 0 0 "bbbbbbbb/0" absent "" 2
MODE=prior_findings; export MODE
scope 0 1 "bbbbbbbb/1" absent "" 2
MODE=none; export MODE
scope 0 2 "" absent "" 2
# ...and it fails closed, keeping the raw count, when it cannot.
MODE=pending_newer; export MODE
scope 2 2 "" absent "" 2
MODE=commits_fail; export MODE
scope 1 2 "" absent "" 2

# An UNREADABLE head listing is not `absent`: callers pass another word and the raw count
# stands without the API being asked (a failed lookup must never read as "Greptile never ran").
MODE=no_api; export MODE
scope 0 2 "" unreadable "" 2

# The cache key is the whole REPO/PR/HEAD: a different PR sharing the head SHA must not be
# served the first PR's verdict (pr-findings.sh walks several PRs in one process).
MODE=prior_clean; export MODE
want 0 "$PREV_SHA" 0 ""
rc=0; greptile_prior_verdict test/repo 43 "$HEAD_SHA" || rc=$?
{ [ "$rc" -eq 0 ] && [ -z "$GRV_SHA" ]; } || fail "PR 43 was served PR 42's cached verdict (rc=$rc sha=$GRV_SHA)"


# A cache HIT must restore the whole verdict, not just find it. A failed or pending call for
# ANOTHER PR leaves the cache on the first PR but clears the globals, so the hit is the only
# thing that puts them back; a stale GRV_STARTED would mis-date the trigger check below.
MODE=prior_clean; export MODE
want 0 "$PREV_SHA" 0 ""
rc=0; greptile_prior_verdict test/repo 44 "$HEAD_SHA" || rc=$?
{ [ "$rc" -eq 1 ] && [ -z "$GRV_SHA$GRV_STARTED" ]; } || fail "PR 44's failed lookup did not clear the globals (rc=$rc sha=$GRV_SHA started=$GRV_STARTED)"
MODE=no_api; export MODE
rc=0; greptile_prior_verdict test/repo 42 "$HEAD_SHA" || rc=$?
{ [ "$rc" -eq 0 ] && [ "$GRV_SHA" = "$PREV_SHA" ] && [ "$GRV_ADDED" = "0" ] && [ "$GRV_STARTED" = "2001-01-01T00:00:00Z" ]; } \
  || fail "cache hit did not restore the verdict (rc=$rc sha=$GRV_SHA added=$GRV_ADDED started=$GRV_STARTED)"
# A review REQUESTED after the last verdict outranks it too: `@greptileai review` only
# becomes a check-run ~12 s later, and until then the head reads `absent`. The stub's verdict
# started 2001-01-01, so "now" is a trigger newer than it and inside the grace.
now_iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
trigger() { printf '[{"user":{"login":"lander","type":"User"},"created_at":"%s","body":"%s"}]' "$1" "$2"; }
MODE=prior_clean; export MODE
scope 2 2 "" absent "" 2 "$(trigger "$now_iso" '@greptileai review')"
scope 2 2 "" absent "" 2 "$(trigger "$now_iso" 'please @Greptile  Review this again')"
# ...but not a trigger OLDER than the verdict: that is the request the verdict answered. The
# first case isolates this guard (a recent trigger, so the grace alone would still count it).
MODE=prior_clean_later; export MODE
scope 0 0 "bbbbbbbb/0" absent "" 2 "$(trigger "$now_iso" '@greptileai review')"
MODE=prior_clean; export MODE
scope 0 0 "bbbbbbbb/0" absent "" 2 "$(trigger 2000-12-31T00:00:00Z '@greptileai review')"
# ...nor one past the grace: Greptile is evidently not coming, so the verdict stands,
scope 0 0 "bbbbbbbb/0" absent "" 2 "$(trigger 2001-01-02T00:00:00Z '@greptileai review')"
# ...nor a bot quoting the phrase, nor a comment that merely mentions Greptile.
scope 0 0 "bbbbbbbb/0" absent "" 2 '[{"user":{"login":"coderabbitai[bot]","type":"Bot"},"created_at":"'"$now_iso"'","body":"@greptileai review"}]'
scope 0 0 "bbbbbbbb/0" absent "" 2 "$(trigger "$now_iso" 'greptile already looked at this')"
# Unreadable issue comments fail closed and keep the raw count.
scope 1 2 "" absent "" 2 'x'

# ---- greptile_paired_verdict ----------------------------------------------------------
# The PR #1698 race fixture the entrypoint tests share, answered by a `gh` function in a
# subshell (the PATH stub above serves the other sections). Pins each outcome's rc:
# 0 = evidence or none, 1 = unknown, 2 = pending.
# paired MODE RC SHA8 ADDED [HEAD_REVIEW_AT]
paired() (
  # shellcheck source=greptile-race.fixture.sh
  . "$HERE/greptile-race.fixture.sh"
  gh() { shift; race_api "$@"; }
  MODE="$1"; export MODE
  rc=0
  greptile_paired_verdict test/repo 42 "$RACE_HEAD" "${5:-}" "$(race_api /issues/42/comments)" || rc=$?
  [ "$rc" -eq "$2" ] || fail "paired $1: rc=$rc, want $2"
  [ "${GRP_SHA:0:8}" = "$3" ] || fail "paired $1: GRP_SHA='$GRP_SHA', want '$3'"
  [ "$GRP_ADDED" = "$4" ] || fail "paired $1: GRP_ADDED='$GRP_ADDED', want '$4'"
)
paired pushrace 0 bbbbbbbb 0
paired pushrace_stale_dup 0 bbbbbbbb 0
paired pushrace_review_findings 0 bbbbbbbb 1 2026-09-25T16:18:37Z
paired pushrace_new_trigger 2 "" ""
for m in pushrace_notrigger pushrace_forged pushrace_othersha pushrace_malformed pushrace_short pushrace_nodiff pushrace_truncated pushrace_nowindow pushrace_norun pushrace_early pushrace_pending_first; do
  paired "$m" 0 "" ""
done
paired pushrace_two_runs 1 "" ""
paired pushrace_more_pages 1 "" ""

# greptile_body_sha parses only inside the block, and only a full, unambiguous SHA.
bs() { printf '%s' "$1" | greptile_body_sha; }
blk() { jq -n --arg t "$1" '{body:("desc\n<!-- greptile_comment -->\n" + $t + "\n<!-- /greptile_comment -->")}'; }
[ "$(bs "$(blk "Last reviewed commit: [x](https://g/o/r/commit/$HEAD_SHA)")")" = "$HEAD_SHA" ] || fail "body SHA not read"
[ -z "$(bs "$(blk 'Last reviewed commit: [x](https://g/o/r/commit/cccccccc)')")" ] || fail "a short body SHA was accepted"
[ -z "$(bs "$(blk "Last reviewed commit: [x](https://g/o/r/commit/${HEAD_SHA}ab)")")" ] || fail "an over-long body SHA was accepted"
[ -z "$(bs "$(blk "Last reviewed commit: [x](https://g/o/r/commit/$HEAD_SHA) [y](https://g/o/r/commit/$PREV_SHA)")")" ] || fail "an ambiguous body SHA was accepted"
[ -z "$(bs "$(blk "Reviewed [x](https://g/o/r/commit/$HEAD_SHA)")")" ] || fail "a SHA off the Last-reviewed line was accepted"
[ -z "$(bs "{\"body\":\"Last reviewed commit: [x](https://g/o/r/commit/$HEAD_SHA)\"}")" ] || fail "a SHA outside any block was accepted"
[ -z "$(bs '{"body":null}')" ] || fail "a null body produced a SHA"
if bs '[]' >/dev/null; then fail "a non-object pull did not fail"; fi

# ---- greptile_outside_diff -------------------------------------------------------------
# Here-strings, not a pipe: the function sets GOD_* in the CALLER's shell.
od() { greptile_outside_diff "$HEAD_SHA" <<<"$1"; }
OD_JSON=$(jq -n --arg h "$HEAD_SHA" --arg p "$PREV_SHA" '[{id:1,user:{login:"greptile-apps[bot]"},
  body:("<!-- greptile_outside_diff -->\n\n- <img alt=\"P1\">&nbsp;**Head bug** `a.go:3` <a href=\"https://x/blob/" + $h + "/a.go#L3\">x</a>\n- <img alt=\"P2\">&nbsp;**Old bug** `b.go:9` <a href=\"https://x/blob/" + $p + "/b.go#L9\">x</a>")}]')
od "$OD_JSON" || fail "outside-diff read failed"
[ "$GOD_TOTAL" = 2 ] && [ "$GOD_HEAD" = 1 ] || fail "outside-diff counts: total=$GOD_TOTAL head=$GOD_HEAD"
printf '%s' "$GOD_LINES" | grep -qF '  GR  a.go:3  [P1] Head bug (outside diff)' || fail "outside-diff row: $GOD_LINES"
od '[{"id":2,"user":{"login":"someone"},"body":"<!-- greptile_outside_diff -->\n- spoof"}]' || fail "non-greptile comment read failed"
[ "$GOD_TOTAL" = 0 ] || fail "a non-Greptile comment was counted"
if od '{"not":"array"}'; then fail "unreadable issue comments did not fail closed"; fi

echo "PASS greptile-verdict: earlier verdict scoped, newer evidence outranks it, run-on-older-commit head verdict, unreadable fails closed"
