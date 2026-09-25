#!/usr/bin/env bash
# takeover.sh — one-shot snapshot of a uzi run and/or its PR, so a session taking over
# mid-flight starts from facts instead of a hand-rolled survey. Read-only.
#
# Usage: takeover.sh (<run-id> | <PR-number>) [--repo OWNER/REPO] [--no-claim]
#   A bare number is a PR; anything else is a uzi run id (prefix ok). Each resolves the
#   other when it can: a run's mr_iid -> PR; a PR -> the newest non-rework uzi run that
#   opened it. --repo defaults to the checkout's origin (gh repo view).
#   Unless --no-claim, an open PR is CLAIMED for this session (claims.sh) so other landers
#   see it; a PR another live session holds stops here with NEXT=claimed_by_other.
#
# Prints KEY=VALUE lines (empty when unknown), then NEXT=<state> — the branch point the
# uzi-lander SKILL.md decision tree keys on:
#   run_active:<status>   run not terminal (running, or a park: awaiting_*, limit_wait, …)
#   run_failed:<origin>   run failed/cancelled and no PR — hand to uzi-watcher recovery
#   claimed_by_other      another live session is landing this PR (CLAIM_HELD_BY printed)
#   merged | closed       nothing to land
#   conflict              mergeStateStatus DIRTY — resolve in a worktree
#   migration_collision   PR adds a migration whose number already exists on main — rebase + renumber
#   ci_red | ci_pending   required checks
#   mr_rework_active      defer to uzi's rework
#   cr_rate_limited       CR refused this head; CR_RESET_MIN set when known
#   review_pending        a bot is reviewing this head now
#   unknown               a lookup failed or returned an unreadable payload; never act on it
#   no_review             nothing reviewed this head and nothing is coming — trigger or fall back
#   findings              live inline findings on the head (LIVE_FINDINGS=n)
#   ready                 CI green, head reviewed, 0 live findings, no rework (BEHIND is fine
#                         under an admin merge; MERGE_STATE says so)
# Exit 0 on a snapshot, 3 on usage / could not resolve the target.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/greptile-verdict.sh
. "$HERE/lib/greptile-verdict.sh"
TARGET=""; REPO=""; CLAIM=1
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="${2:?}"; shift 2;;
    --no-claim) CLAIM=0; shift;;
    -h|--help) sed -n '2,29p' "$0"; exit 3;;
    -*) echo "unknown flag: $1" >&2; exit 3;;
    *) if [ -z "$TARGET" ]; then TARGET="$1"; else echo "unexpected arg: $1" >&2; exit 3; fi; shift;;
  esac
done
[ -n "$TARGET" ] || { echo "usage: takeover.sh (<run-id> | <PR-number>) [--repo OWNER/REPO]" >&2; exit 3; }
if [ -z "$REPO" ]; then
  REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) || { echo "cannot infer --repo" >&2; exit 3; }
fi
echo "REPO=$REPO"

have_uzi=0; command -v uzi >/dev/null 2>&1 && have_uzi=1
# UNKNOWN is set by any lookup that fails or returns an unreadable payload; a snapshot with
# UNKNOWN=1 never reaches NEXT=ready. Declared here because the first such lookup is next.
UNKNOWN=0
repo_id=""; repo_listed=0
if [ "$have_uzi" -eq 1 ]; then
  rl=$(uzi repo list --json 2>/dev/null || echo 'x')
  if printf '%s' "$rl" | jq -e 'type=="array"' >/dev/null 2>&1; then
    repo_listed=1
    repo_id=$(printf '%s' "$rl" | jq -r --arg p "$REPO" '[.[]|select(.path_with_namespace==$p)|.id]|first // ""' 2>/dev/null) || UNKNOWN=1
  else
    UNKNOWN=1; echo "UZI_REPO_LIST=unreadable"
  fi
fi

# ---- resolve run <-> PR --------------------------------------------------------------
PR=""; run_json=""
if printf '%s' "$TARGET" | grep -qE '^[0-9]+$'; then
  PR="$TARGET"
  if [ "$have_uzi" -eq 1 ] && [ -n "$repo_id" ]; then
    run_json=$(uzi run list --json 2>/dev/null | jq -c --arg repo "$repo_id" --argjson pr "$PR" \
      '[.[]|select(.repo_id==$repo and .mr_iid==$pr and .kind!="mr_rework")]|max_by(.created_at) // empty' 2>/dev/null || true)
  fi
else
  [ "$have_uzi" -eq 1 ] || { echo "uzi CLI not on PATH; cannot resolve a run id" >&2; exit 3; }
  run_json=$(uzi run get "$TARGET" --json 2>/dev/null) || { echo "uzi run get $TARGET failed" >&2; exit 3; }
  PR=$(printf '%s' "$run_json" | jq -r '.mr_iid // empty')
fi

if [ -n "$run_json" ]; then
  printf '%s' "$run_json" | jq -r '
    "RUN_ID=\(.id)", "RUN_STATUS=\(.status)", "RUN_KIND=\(.kind)", "ISSUE=\(.issue_iid // "")",
    "MR=\(.mr_iid // "")", "MR_URL=\(.mr_web_url // "")", "WORKER=\(.worker_id // "")",
    "FAIL_ORIGIN=\(.fail_origin // "")", "FAILURE_REASON=\((.failure_reason // "")|.[0:160]|gsub("\n";" "))",
    "HEALTH=\(.health_reason // "")"'
  run_status=$(printf '%s' "$run_json" | jq -r '.status')
  case "$run_status" in
    completed|failed|cancelled) ;;
    *) echo "NEXT=run_active:$run_status"; exit 0 ;;
  esac
  if [ -z "$PR" ]; then
    case "$run_status" in
      completed) echo "NEXT=run_completed_no_pr (report-only or PR not yet visible)";;
      *) echo "NEXT=run_failed:$(printf '%s' "$run_json" | jq -r '.fail_origin // "unknown"')";;
    esac
    exit 0
  fi
fi
[ -n "$PR" ] || { echo "NEXT=unresolved (no PR for $TARGET)"; exit 3; }

# ---- PR snapshot -----------------------------------------------------------------------
pj=$(gh pr view "$PR" --repo "$REPO" --json number,state,isDraft,headRefOid,headRefName,baseRefName,mergeable,mergeStateStatus,reviewDecision,title 2>/dev/null) \
  || { echo "gh pr view $PR failed" >&2; exit 3; }
printf '%s' "$pj" | jq -r '
  "PR=\(.number)", "PR_STATE=\(.state)", "DRAFT=\(.isDraft)", "HEAD=\(.headRefOid)", "BRANCH=\(.headRefName)",
  "BASE=\(.baseRefName)", "MERGEABLE=\(.mergeable)", "MERGE_STATE=\(.mergeStateStatus)",
  "REVIEW_DECISION=\(.reviewDecision)", "TITLE=\(.title|.[0:100])"'
state=$(printf '%s' "$pj" | jq -r .state); head=$(printf '%s' "$pj" | jq -r .headRefOid)
merge_state=$(printf '%s' "$pj" | jq -r .mergeStateStatus); base=$(printf '%s' "$pj" | jq -r .baseRefName)
case "$state" in MERGED) echo "NEXT=merged"; exit 0;; CLOSED) echo "NEXT=closed"; exit 0;; esac

# Every lookup below that fails or returns an unparseable payload sets UNKNOWN=1 (declared
# at the top), and an unknown snapshot never reaches NEXT=ready: a masked failure would
# otherwise read as "zero pending, zero findings".
arr_or_unknown() { printf '%s' "$1" | jq -e 'type=="array"' >/dev/null 2>&1 || UNKNOWN=1; }

# Required checks by bucket (must be a non-empty array; `{}`/`[]` are unknown).
ci_fail=0; ci_pend=0; ci_cancel=0
cj=$(gh pr checks "$PR" --repo "$REPO" --required --json bucket 2>/dev/null || true)
if printf '%s' "$cj" | jq -e 'type=="array" and length>0' >/dev/null 2>&1; then
  ci_fail=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="fail")]|length')
  ci_pend=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="pending")]|length')
  ci_cancel=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="cancel")]|length')
else
  UNKNOWN=1; echo "CI_CHECKS=unreadable-or-empty"
fi
echo "CI_FAIL=$ci_fail"; echo "CI_PENDING=$ci_pend"; echo "CI_CANCELLED=$ci_cancel"

# CodeRabbit on the head: commit-status description + a VERDICT review on the head (signal
# a: APPROVED or a tally) + the walkthrough marker (c). A tally-less non-APPROVED review on
# the head is an UNCONFIRMED finding (grouped findings live in its body).
st=$(gh api "repos/$REPO/commits/$head/status" 2>/dev/null || true)
if printf '%s' "$st" | jq -e 'has("statuses")' >/dev/null 2>&1; then
  cr_desc=$(printf '%s' "$st" | jq -r '[.statuses[]|select(.context=="CodeRabbit")]|last|.description // empty')
else cr_desc=""; UNKNOWN=1; fi
echo "CR_STATUS='${cr_desc:-absent}'"
cr_reviewed=0; cr_unconfirmed=0
rev_raw=$(gh api --paginate "repos/$REPO/pulls/$PR/reviews" 2>/dev/null | jq -s 'add // []' 2>/dev/null || echo 'x')
arr_or_unknown "$rev_raw"; [ "$UNKNOWN" -eq 1 ] && rev_raw='[]'
# shellcheck disable=SC2016
n=$(printf '%s' "$rev_raw" | jq -r --arg h "$head" '[.[]|select(.user.login=="coderabbitai[bot]" and .commit_id==$h and (.state=="APPROVED" or ((.body // "")|test("Actionable comments posted: [0-9]+"))))]|length' 2>/dev/null || echo 0)
[ "${n:-0}" -gt 0 ] && cr_reviewed=1
# shellcheck disable=SC2016
cr_unconfirmed=$(printf '%s' "$rev_raw" | jq -r --arg h "$head" '[.[]|select(.user.login=="coderabbitai[bot]" and .commit_id==$h and .state!="APPROVED" and (((.body // "")|test("Actionable comments posted: [0-9]+"))|not))]|length' 2>/dev/null || echo 0)
issue_c=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null | jq -s 'add // []' 2>/dev/null || echo 'x')
arr_or_unknown "$issue_c"; printf '%s' "$issue_c" | jq -e 'type=="array"' >/dev/null 2>&1 || issue_c='[]'
wt_body=$(printf '%s' "$issue_c" | jq -r '[.[]|select(.user.login=="coderabbitai[bot]" and (.body|contains("<!-- walkthrough_start -->") or contains("rate limited by coderabbit.ai")))]|last|.body // empty' 2>/dev/null || true)
# shellcheck disable=SC2016
fr_head=$(printf '%s' "$wt_body" | awk '/final_review_risk_start/{f=1} f{print} /final_review_risk_end/{f=0}' | grep -oE 'up to `[0-9a-f]{5,40}`' | tail -1 | grep -oE '[0-9a-f]{5,40}' || true)
[ -n "$fr_head" ] && printf '%s' "$head" | grep -q "^$fr_head" && cr_reviewed=1
echo "CR_REVIEWED_HEAD=$cr_reviewed"; echo "CR_UNCONFIRMED_ON_HEAD=$cr_unconfirmed"
cr_reset=$(printf '%s' "$wt_body" | awk '/auto-generated comment: rate limited by coderabbit.ai/{f=1} f{print} /end of auto-generated comment: rate limited/{f=0}' | grep -oE 'available in [0-9]+ minutes' | tail -1 | grep -oE '[0-9]+' || true)
[ -n "$cr_reset" ] && echo "CR_RESET_MIN=$cr_reset (as of the walkthrough's last edit; scripts/cr-rate-limit.sh for the live remainder)"
cr_tally=$(printf '%s' "$rev_raw" | jq -r '.[]|select(.user.login=="coderabbitai[bot]")|.body' 2>/dev/null | grep -oiE 'Actionable comments posted: [0-9]+' | tail -1 || true)
[ -n "$cr_tally" ] && echo "CR_TALLY='$cr_tally'"

# Greptile check-run on the head.
cr_pages=$(gh api --paginate "repos/$REPO/commits/$head/check-runs" 2>/dev/null || echo 'x')
printf '%s' "$cr_pages" | jq -es 'length>0 and all(.[]; type=="object" and has("check_runs"))' >/dev/null 2>&1 || UNKNOWN=1
# An unreadable listing is NOT `absent`: `absent` means "read, and Greptile has no run here",
# which is what lets an earlier verdict scope the findings below.
gr_state="absent"; gr_sum=""; gr_concl=""; gr_reviewed=0
if gr_json=$(printf '%s' "$cr_pages" | greptile_newest_run); then
  [ "$gr_json" = "{}" ] && gr_json=""
else
  gr_json=""; gr_state="unreadable"; UNKNOWN=1
fi
if [ -n "$gr_json" ]; then
  gr_state=$(printf '%s' "$gr_json" | jq -r '.status // "absent"')
  gr_concl=$(printf '%s' "$gr_json" | jq -r '.conclusion // ""')
  gr_sum=$(printf '%s' "$gr_json" | jq -r '.output.summary // ""' | grep -oE '[0-9]+ files reviewed, [0-9]+ comments added' || true)
  # Reviewed = completed AND success AND the review summary; a failed/cancelled/skipped
  # completed check is not a review.
  if [ "$gr_state" = "completed" ] && [ "$gr_concl" = "success" ] && [ -n "$gr_sum" ]; then gr_reviewed=1; fi
fi
echo "GREPTILE=$gr_state${gr_concl:+/$gr_concl}"; [ -n "$gr_sum" ] && echo "GREPTILE_SUMMARY='$gr_sum'"; echo "GREPTILE_REVIEWED_HEAD=$gr_reviewed"

# Live inline findings from either bot (+ the unconfirmed CodeRabbit review body).
pull_c=$(gh api --paginate "repos/$REPO/pulls/$PR/comments" 2>/dev/null | jq -s 'add // []' 2>/dev/null || echo 'x')
arr_or_unknown "$pull_c"; printf '%s' "$pull_c" | jq -e 'type=="array"' >/dev/null 2>&1 || pull_c='[]'
cr_live=$(printf '%s' "$pull_c" | jq '[.[]|select(.user.login=="coderabbitai[bot]" and .line!=null and ((.body|contains("Addressed in commit"))|not))]|length' 2>/dev/null || echo 0)
gr_live=$(printf '%s' "$pull_c" | jq '[.[]|select(.user.login=="greptile-apps[bot]" and .line!=null)]|length' 2>/dev/null || echo 0)
# Every push re-anchors Greptile's older comments onto the new head, so the raw anchored
# count above over-reports. An explicit clean pass on THIS head clears them; a head with no
# Greptile evidence at all is scoped to Greptile's newest EARLIER verdict by the same
# function watch-pr.sh and pr-findings.sh use (lib/greptile-verdict.sh), so the three decide alike.
# A current-head pass that added comments keeps the raw count: a superset, never an
# under-report. Liveness only; GREPTILE_REVIEWED_HEAD above stays the exact-head answer.
if [ "$gr_reviewed" -eq 1 ]; then
  case "$gr_sum" in *', 0 comments added') gr_live=0;; esac
else
  gr_head_rid=$(printf '%s' "$rev_raw" | jq -r --arg h "$head" '[.[]|select(.user.login=="greptile-apps[bot]" and .commit_id==$h)]|last|.id // empty' 2>/dev/null || echo unreadable)
  gr_rc=0
  greptile_scope_live "$REPO" "$PR" "$head" "$gr_state" "$gr_head_rid" "$gr_live" "$pull_c" "$issue_c" || gr_rc=$?
  if [ "$gr_rc" -eq 0 ]; then
    gr_live="$GRL_LIVE"
    [ -n "$GRL_NOTE" ] && echo "GREPTILE_PRIOR_VERDICT=$GRL_NOTE"
  else
    UNKNOWN=1
    if [ "$gr_rc" -eq 2 ]; then echo "GREPTILE_PRIOR_VERDICT=pending"; else echo "GREPTILE_PRIOR_VERDICT=unreadable"; fi
  fi
fi
# Greptile's outside-diff findings live in one issue comment, not in pulls/N/comments
# (lib/greptile-verdict.sh, greptile_outside_diff). Live unless THIS head's pass added 0.
case "$gr_reviewed:$gr_sum" in
  1:*', 0 comments added') ;;
  *) if greptile_outside_diff "$head" <<<"$issue_c"; then gr_live=$(( gr_live + GOD_TOTAL )); else UNKNOWN=1; fi ;;
esac
live=$((cr_live + gr_live + cr_unconfirmed))
echo "LIVE_FINDINGS=$live (cr=$cr_live gr=$gr_live cr_unconfirmed=$cr_unconfirmed)"

# Active mr_rework on this MR. A repo on uzi whose listing cannot be read is UNKNOWN.
mrw=0
if [ "$have_uzi" -eq 1 ] && [ -n "$repo_id" ]; then
  runs=$(uzi run list --json 2>/dev/null || echo 'x')
  if printf '%s' "$runs" | jq -e 'type=="array"' >/dev/null 2>&1; then
    mrw=$(printf '%s' "$runs" | jq -r --arg repo "$repo_id" --argjson pr "$PR" \
      '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr and ((.status|test("completed|failed|cancelled"))|not))]|length' 2>/dev/null) || UNKNOWN=1
  else
    UNKNOWN=1; echo "MR_REWORK=unreadable"
  fi
elif [ "$have_uzi" -eq 1 ] && [ "$repo_listed" -eq 1 ] && [ -z "$repo_id" ]; then
  echo "MR_REWORK=repo-not-on-uzi"
fi
echo "MR_REWORK_ACTIVE=$mrw"

# Workflow files and migrations in the PR diff (added files), vs the base's migration set.
# An unreadable files list or base listing is UNKNOWN: a missed migration collision
# bricks boot, so it must never read as "no collision".
files=$(gh api --paginate "repos/$REPO/pulls/$PR/files" 2>/dev/null | jq -s 'add // []' 2>/dev/null || echo 'x')
if ! printf '%s' "$files" | jq -e 'type=="array"' >/dev/null 2>&1; then UNKNOWN=1; echo "PR_FILES=unreadable"; files='[]'; fi
wf=$(printf '%s' "$files" | jq '[.[]|select(.filename|startswith(".github/workflows/"))]|length' 2>/dev/null || echo 0)
echo "WORKFLOW_FILES_IN_DIFF=$wf"
added_mig=$(printf '%s' "$files" | jq -r '.[]|select(.status=="added" and (.filename|startswith("api/internal/store/migrations/")))|.filename|split("/")|last' 2>/dev/null || true)
main_mig_raw=$(gh api "repos/$REPO/contents/api/internal/store/migrations?ref=$base" 2>/dev/null || echo 'x')
if printf '%s' "$main_mig_raw" | jq -e 'type=="array"' >/dev/null 2>&1; then
  main_mig=$(printf '%s' "$main_mig_raw" | jq -r '.[].name')
else
  UNKNOWN=1; echo "BASE_MIGRATIONS=unreadable"; main_mig=""
fi
main_head=$(printf '%s\n' "$main_mig" | grep -oE '^[0-9]+' | sort -n | tail -1 || true)
echo "MIGRATION_HEAD_ON_BASE=${main_head:-}"
collision=""
if [ -n "$added_mig" ]; then
  echo "MIGRATIONS_ADDED=$(printf '%s' "$added_mig" | tr '\n' ' ')"
  while IFS= read -r m; do
    [ -z "$m" ] && continue
    pfx=$(printf '%s' "$m" | grep -oE '^[0-9]+')
    if printf '%s\n' "$main_mig" | grep -qE "^${pfx}_" && ! printf '%s\n' "$main_mig" | grep -qxF "$m"; then
      collision="$collision $m"
    fi
  done <<< "$added_mig"
fi
[ -n "$collision" ] && echo "MIGRATION_COLLISION=$collision (renumber above $main_head: task migration:renumber)"
n_files=$(printf '%s' "$files" | jq 'length' 2>/dev/null || echo 0)
n_lines=$(printf '%s' "$files" | jq '[.[]|.additions+.deletions]|add // 0' 2>/dev/null || echo 0)
echo "SIZE_FILES=$n_files"; echo "SIZE_LINES=$n_lines"

# ---- claim ------------------------------------------------------------------------------
# Record that THIS session is landing the PR (priority defaults to the file count: spend
# CodeRabbit reviews on the large PRs first). Another live session's claim stops us here.
if [ "$CLAIM" -eq 1 ]; then
  if ! cl_out=$("$HERE/claims.sh" claim "#$PR" --repo "$REPO" --pr "$PR" --size "$n_files" --lines "$n_lines" 2>&1); then
    printf '%s\n' "$cl_out"
    echo "NEXT=claimed_by_other"; exit 4
  fi
  printf '%s\n' "$cl_out"
fi

# ---- NEXT -------------------------------------------------------------------------------
reviewed=$cr_reviewed
[ "$gr_reviewed" -eq 1 ] && reviewed=1
echo "UNKNOWN=$UNKNOWN"
if   [ "$UNKNOWN" -eq 1 ]; then echo "NEXT=unknown (a lookup failed or returned an unreadable payload; re-run before acting)"
elif [ -n "$collision" ]; then echo "NEXT=migration_collision"
elif [ "$merge_state" = "DIRTY" ]; then echo "NEXT=conflict"
elif [ "$ci_fail" -gt 0 ]; then echo "NEXT=ci_red"
elif [ "$mrw" -gt 0 ]; then echo "NEXT=mr_rework_active"
elif [ "$ci_pend" -gt 0 ] || [ "$ci_cancel" -gt 0 ]; then echo "NEXT=ci_pending"
elif [ "$reviewed" -eq 0 ]; then
  case "$cr_desc" in
    *"in progress"*) echo "NEXT=review_pending";;
    *"rate limited"*) echo "NEXT=cr_rate_limited";;
    *) if [ "$gr_state" = "in_progress" ] || [ "$gr_state" = "queued" ]; then echo "NEXT=review_pending"; else echo "NEXT=no_review"; fi;;
  esac
elif [ "$live" -gt 0 ]; then echo "NEXT=findings"
else echo "NEXT=ready${merge_state:+ (merge_state=$merge_state)}"
fi
exit 0
