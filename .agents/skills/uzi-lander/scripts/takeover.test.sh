#!/usr/bin/env bash
# Hermetic regression: takeover.sh's Greptile liveness. A push re-anchors Greptile's older
# comments onto the new head, so the raw anchored count over-reports; the snapshot must
# agree with watch-pr.sh and pr-findings.sh about which of them are live.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/takeover.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
# Not connected to uzi: the run/rework lookups are skipped, which is not under test here.
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then echo '[]'; exit 0; fi
echo "unexpected uzi call: $*" >&2; exit 1
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
PREV=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
greptile() { printf '{"check_runs":[{"id":10,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"%s","conclusion":%s,"output":{"summary":"%s"}}]}\n' "$1" "$2" "$3"; }
none() { echo '{"check_runs":[{"id":5,"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","output":{"summary":""}}]}'; }
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then
  printf '{"number":42,"state":"OPEN","isDraft":false,"headRefOid":"%s","headRefName":"agent/issue-1","baseRefName":"main","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"","title":"t"}\n' "$HEAD"
  exit 0
fi
if [ "${1:-}" = pr ] && [ "${2:-}" = checks ]; then echo '[{"bucket":"pass"}]'; exit 0; fi
[ "${1:-}" = api ] || { echo "unexpected gh call: $*" >&2; exit 1; }
# pushrace* modes: the PR #1698 race, shared with the other entrypoints' tests.
case "$MODE" in pushrace*) . "$RACE_FIXTURE"; shift; race_api "$@"; exit $? ;; esac
case "$*" in
  *"/commits/$HEAD/status"*) echo '{"statuses":[]}' ;;
  *'graphql'*)
    case "$MODE" in prior_resolved|prior_pending_resolved) ;; *) echo "unexpected gh api: $*" >&2; exit 1 ;; esac
    echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":true,"isOutdated":false,"comments":{"nodes":[{"databaseId":991,"author":{"login":"greptile-apps"},"body":"P1 finding","path":"x.go","line":8,"originalLine":8}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}' ;;
  *'/pulls/42/reviews'*)
    case "$MODE" in
      head_review_object) printf '[{"id":77,"user":{"login":"greptile-apps[bot]"},"commit_id":"%s","state":"COMMENTED","body":""}]\n' "$HEAD" ;;
      *) echo '[]' ;;
    esac ;;
  *'/issues/42/comments'*)
    if [ "$MODE" = prior_requested ]; then printf '[{"user":{"login":"lander","type":"User"},"created_at":"%s","body":"@greptileai review"}]\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    elif [ "$MODE" = outside_diff ]; then
      jq -n --arg h "$HEAD" '[{id:900,user:{login:"greptile-apps[bot]"},body:("<!-- greptile_outside_diff -->\n\n- <img alt=\"P1\">&nbsp;**Halt alert can be lost** `x.go:252` <a href=\"https://x/blob/" + $h + "/x.go#L252\">x</a>")}]'
    else echo '[]'; fi ;;
  *"/commits/$HEAD/check-runs"*)
    case "$MODE" in
      head_clean) greptile completed '"success"' 'Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added.' ;;
      head_findings) greptile completed '"success"' '90 files reviewed, 10 comments added' ;;
      outside_diff) greptile completed '"success"' '149 files reviewed, 1 comments added' ;;
      head_failed) greptile completed '"failure"' '' ;;
      head_unreadable) echo '[]' ;;
      # Newest first, as the API lists them: the re-trigger (id 2) found something the first run did not.
      head_two_runs) echo '{"check_runs":[{"id":2,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 1 comments added"}},{"id":1,"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"90 files reviewed, 0 comments added"}}]}' ;;
      *) none ;;
    esac ;;
  *'/pulls/42/comments'*)
    if [ "$MODE" = outside_diff ]; then echo '[]'
    else echo '[{"id":991,"user":{"login":"greptile-apps[bot]"},"line":8,"body":"<img alt=\"P1\"> finding","pull_request_review_id":99}]'; fi ;;
  *'/pulls/42/commits'*)
    [ "$MODE" = prior_unreadable ] && exit 1
    printf '[{"sha":"%s"},{"sha":"%s"}]\n' "$PREV" "$HEAD" ;;
  *"/commits/$PREV/check-runs"*)
    case "$MODE" in
      prior_clean|head_review_object|head_failed|head_unreadable|prior_requested) greptile completed '"success"' '90 files reviewed, 0 comments added' ;;
      prior_pending|prior_pending_resolved) greptile in_progress null '' ;;
      *) none ;;
    esac ;;
  *'/pulls/42/files'*) echo '[]' ;;
  *'/contents/'*) echo '[]' ;;
  *) echo "unexpected gh api: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi"
export PATH="$WORK/bin:$PATH"
export RACE_FIXTURE="$HERE/lib/greptile-race.fixture.sh"

# snap MODE — one snapshot; never claims (that writes shared state). A snapshot always exits 0.
snap() {
  MODE="$1"; export MODE
  bash "$SCRIPT" 42 --repo test/repo --no-claim > "$WORK/$1.out" 2>&1 || fail "$1: takeover.sh exited $?: $(cat "$WORK/$1.out")"
}
has() { grep -qF -- "$2" "$WORK/$1.out" || fail "$1: missing '$2': $(cat "$WORK/$1.out")"; }
hasnt() { if grep -qF -- "$2" "$WORK/$1.out"; then fail "$1: unexpected '$2': $(cat "$WORK/$1.out")"; fi; }

# An explicit clean pass on THIS head clears the older anchored comment...
snap head_clean
has head_clean 'GREPTILE_REVIEWED_HEAD=1'
has head_clean 'LIVE_FINDINGS=0 (cr=0 gr=0 '
# ...while ", 10 comments added" must not match the ", 0 comments added" clean test.
snap head_findings
has head_findings 'LIVE_FINDINGS=1 (cr=0 gr=1 '

# Every finding the head pass added is outside the diff (one Greptile ISSUE comment, no
# inline comment, no review object): it is live, so the PR is never NEXT=ready (PR #1671).
snap outside_diff
has outside_diff 'LIVE_FINDINGS=1 (cr=0 gr=1 '
hasnt outside_diff 'NEXT=ready'

# No Greptile evidence on the head: the newest earlier verdict scopes the count, and the
# head still reads unreviewed (an earlier verdict never satisfies the gate).
snap prior_clean
has prior_clean 'GREPTILE_REVIEWED_HEAD=0'
has prior_clean 'GREPTILE_PRIOR_VERDICT=bbbbbbbb/0'
has prior_clean 'LIVE_FINDINGS=0 (cr=0 gr=0 '
has prior_clean 'NEXT=no_review'

# No earlier verdict: the comment stays live.
snap prior_none
hasnt prior_none 'GREPTILE_PRIOR_VERDICT='
has prior_none 'LIVE_FINDINGS=1 (cr=0 gr=1 '

# The same comment in a resolved thread is settled, as in watch-pr.sh and pr-findings.sh
# (#1710, 2026-09-26).
snap prior_resolved
has prior_resolved 'LIVE_FINDINGS=0 (cr=0 gr=0 '

# Resolved threads never hide a newer Greptile review still running.
snap prior_pending_resolved
has prior_pending_resolved 'GREPTILE_PRIOR_VERDICT=pending'
has prior_pending_resolved 'NEXT=unknown'

# A Greptile review object already on the head outranks an older clean verdict, exactly as
# in watch-pr.sh and pr-findings.sh.
snap head_review_object
hasnt head_review_object 'GREPTILE_PRIOR_VERDICT='
has head_review_object 'LIVE_FINDINGS=1 (cr=0 gr=1 '

# A review still running on a newer commit than the last verdict: unknown, never ready.
snap prior_pending
has prior_pending 'GREPTILE_PRIOR_VERDICT=pending'
has prior_pending 'UNKNOWN=1'
has prior_pending 'NEXT=unknown'

# ...and so is a review REQUESTED after the last verdict whose check-run has not appeared yet.
snap prior_requested
has prior_requested 'GREPTILE_PRIOR_VERDICT=pending'
has prior_requested 'NEXT=unknown'

# An unreadable history is unknown, and says so.
snap prior_unreadable
has prior_unreadable 'GREPTILE_PRIOR_VERDICT=unreadable'
has prior_unreadable 'LIVE_FINDINGS=1 (cr=0 gr=1 '
has prior_unreadable 'NEXT=unknown'

# A Greptile run that FAILED on the head is evidence about this head: the raw count stands.
snap head_failed
hasnt head_failed 'GREPTILE_PRIOR_VERDICT='
has head_failed 'LIVE_FINDINGS=1 (cr=0 gr=1 '

# An UNREADABLE head listing is not `absent`: no earlier verdict is consulted.
snap head_unreadable
has head_unreadable 'GREPTILE=unreadable'
hasnt head_unreadable 'GREPTILE_PRIOR_VERDICT='
has head_unreadable 'LIVE_FINDINGS=1 (cr=0 gr=1 '
has head_unreadable 'NEXT=unknown'

# Two Greptile runs on the head, listed newest first: the NEWEST is the verdict. Reading the
# last entry took the stale clean run and cleared the finding the re-trigger raised.
snap head_two_runs
has head_two_runs "GREPTILE_SUMMARY='90 files reviewed, 1 comments added'"
has head_two_runs 'LIVE_FINDINGS=1 (cr=0 gr=1 '

# ---- Greptile's run landed on an OLDER commit than the head it reviewed (PR #1698) ------
# The same modes watch-pr.test.sh and pr-findings.test.sh judge; the three must agree.
for m in pushrace pushrace_stale_dup; do
  snap "$m"
  has "$m" 'GREPTILE_REVIEWED_HEAD=1'
  has "$m" "GREPTILE_EVIDENCE='edit→deadbeef via run on bbbbbbbb'"
  has "$m" 'NEXT=ready'
done
for m in pushrace_new_trigger pushrace_new_trigger_absent; do
  snap "$m"
  has "$m" 'GREPTILE_REVIEWED_HEAD=0'
  has "$m" "GREPTILE_EVIDENCE='pending: trigger 2026-09-25T16:24:25Z"
  has "$m" 'NEXT=review_pending'
done
for m in pushrace_notrigger pushrace_forged pushrace_othersha pushrace_malformed pushrace_short pushrace_nodiff pushrace_truncated pushrace_nowindow pushrace_norun pushrace_early; do
  snap "$m"
  has "$m" 'GREPTILE_REVIEWED_HEAD=0'
  has "$m" 'NEXT=no_review'
done
for m in pushrace_two_runs pushrace_more_pages; do
  snap "$m"
  has "$m" 'GREPTILE_REVIEWED_HEAD=0'
  has "$m" 'NEXT=unknown'
done
snap pushrace_review_findings
has pushrace_review_findings 'GREPTILE_REVIEWED_HEAD=1'
has pushrace_review_findings 'LIVE_FINDINGS=1 (cr=0 gr=1 '
has pushrace_review_findings 'NEXT=findings'
# Comments added with no head review id to show them: never ready.
snap pushrace_findings_noreview
has pushrace_findings_noreview 'NEXT=unknown'
snap pushrace_pending_first
has pushrace_pending_first 'NEXT=review_pending'

echo "PASS takeover: Greptile liveness agrees with watch-pr and pr-findings, including a run on an older commit"
