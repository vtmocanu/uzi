#!/usr/bin/env bash
# merge.sh — the guarded merge: re-check the head and the rework lane at the last moment,
# admin-merge past the ruleset, then CONFIRM it merged and print the merge SHA for the
# post-merge watch. The decision to merge is the caller's; this is only the mechanics.
#
# Usage: merge.sh OWNER/REPO PR [--expect-head SHA] [--method squash|merge] [--no-admin] [--no-delete-branch] [--no-rework-check] [--confirm-only]
#   --expect-head   the head you reviewed/watched; a different current head refuses (exit 8)
#                   so a push that landed after your last look is never merged unseen. The
#                   merge itself passes --match-head-commit, so a push in the window between
#                   the preflight and the merge is refused by GitHub as well.
#   --no-rework-check  skip the mr_rework guard (ONLY for a repo that is not on uzi).
#   --confirm-only  do NOT merge: the PR was already merged OUT OF BAND (e.g. the harness
#                   classifier refused the in-script `gh pr merge --admin` and the user ran it
#                   via a `!` line, so merge.sh's own confirm/trail block never executed).
#                   Confirm the PR is MERGED, print MERGE_SHA, write the terminal trail line,
#                   and release the claim while retaining the trail for the post-merge CI
#                   result — the evidence merge.sh writes itself on its own merge. Exit 9 if
#                   the PR is not MERGED yet (nothing is written or released).
#   --method        squash (default; the convention for agent/issue-* branches) or merge
#                   (rarely wanted: squash subjects carry (#PR), which the CHANGELOG cites).
#   --no-admin      drop --admin (needs the ruleset satisfied: review + up-to-date + checks).
#
# Coordination: a repo-wide MERGE LOCK (<state dir>/locks/merge, 10-min TTL) serialises
# landers so two admin merges do not land seconds apart and cancel each other's `main` CI
# by concurrency; on MERGED the PR's claim is released so the shared list only shows live work,
# while its trail survives until the post-merge CI result is appended and printed.
#
# Exit codes:
#   0  merged — prints MERGE_SHA=<sha>; next: watch-run-ci.sh --sha <sha>
#   1  a required check is failing on the head — not merged
#   2  usage, or required checks still pending, not yet all reported, unreadable, none
#      reported, none passed, or gh failed without a failing/pending check explaining it —
#      not merged
#   3  gh error, or the merge command was refused (classifier block, ruleset, conflict):
#      the exact command is printed for the user to run via a `!` line
#   4  an mr_rework run is active on this MR — defer
#   7  the merge lock is held by another live session (owner printed) — wait, re-run
#   8  head mismatch vs --expect-head
#   9  the merge command returned but the PR is not MERGED (auto-merge deferred / async
#      mergeability lag) — poll `gh pr view --json state` yourself before the CI watch
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/state.sh
. "$HERE/lib/state.sh"
# shellcheck source=lib/required-checks.sh
. "$HERE/lib/required-checks.sh"

REPO=""; PR=""; EXPECT=""; METHOD="squash"; ADMIN=1; DELETE=1; REWORK_CHECK=1; CONFIRM_ONLY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --expect-head) EXPECT="${2:?}"; shift 2;;
    --method) METHOD="${2:?}"; shift 2;;
    --no-admin) ADMIN=0; shift;;
    --no-delete-branch) DELETE=0; shift;;
    --no-rework-check) REWORK_CHECK=0; shift;;
    --confirm-only) CONFIRM_ONLY=1; shift;;
    -h|--help) sed -n '2,25p' "$0"; exit 2;;
    -*) echo "unknown flag: $1" >&2; exit 2;;
    *) if [ -z "$REPO" ]; then REPO="$1"; elif [ -z "$PR" ]; then PR="$1"; else echo "unexpected arg: $1" >&2; exit 2; fi; shift;;
  esac
done
[ -n "$REPO" ] && [ -n "$PR" ] || { echo "usage: merge.sh OWNER/REPO PR [--expect-head SHA] [--method squash|merge] [--no-admin]" >&2; exit 2; }
case "$METHOD" in squash|merge) ;; *) echo "bad --method" >&2; exit 2;; esac

pj=$(gh pr view "$PR" --repo "$REPO" --json state,headRefOid,mergeStateStatus,mergeable,mergeCommit,baseRefName 2>/dev/null) || { echo "gh pr view failed" >&2; exit 3; }
state=$(printf '%s' "$pj" | jq -r .state); head=$(printf '%s' "$pj" | jq -r .headRefOid)
ms=$(printf '%s' "$pj" | jq -r .mergeStateStatus); mg=$(printf '%s' "$pj" | jq -r .mergeable)
base=$(printf '%s' "$pj" | jq -r '.baseRefName // empty')

# The terminal evidence contract, from the ONE place that knows the true merge SHA: print
# MERGED + MERGE_SHA, append the trail's merge line (trail.sh dedupes an identical last line),
# THEN release the claim while retaining the trail for the post-merge CI result. Called after
# this script's own merge, and by --confirm-only for a merge that already happened out of band.
report_merged() {  # $1 = merge commit sha; return 1 (write NOTHING) when the sha is empty
  local sha=$1
  # GitHub can report state=MERGED before mergeCommit is populated (eventual consistency).
  # Emitting MERGE_SHA= / a bare `admin-merged ` trail / a --sha-less next command would be
  # broken terminal evidence, so refuse instead and let the caller retry.
  [ -n "$sha" ] || return 1
  echo "MERGED #$PR"; echo "MERGE_SHA=$sha"
  "$HERE/trail.sh" "#$PR" "admin-merged ${sha:0:8}" 2>/dev/null || true
  "$HERE/claims.sh" release "#$PR" 2>/dev/null || true
  echo "next: watch-run-ci.sh --sha $sha --interval 60"
}

# --confirm-only reconciles a merge that landed OUTSIDE this script (the classifier refused
# the in-script admin merge and the user ran it via a `!` line, so the confirm/trail block
# below never ran). Write the evidence that was owed, or exit 9 if it is not merged yet —
# never a merge, never a purge of a still-open PR's claim.
if [ "$CONFIRM_ONLY" -eq 1 ]; then
  if [ "$state" = "MERGED" ]; then
    sha=$(printf '%s' "$pj" | jq -r '.mergeCommit.oid // empty')
    # Re-read a few times if the merge commit is not populated yet, rather than write an
    # empty MERGE_SHA / trail (report_merged refuses an empty sha with rc 1).
    for _ in 1 2 3; do
      [ -n "$sha" ] && break
      sleep 2
      sha=$(gh pr view "$PR" --repo "$REPO" --json mergeCommit -q '.mergeCommit.oid // empty' 2>/dev/null || true)
    done
    if report_merged "$sha"; then exit 0; fi
    echo "PR #$PR is MERGED but its merge commit is not available yet (GitHub lag); re-run --confirm-only shortly"; exit 9
  fi
  echo "PR #$PR is $state, not MERGED — nothing to confirm (run the guarded merge to land it)"; exit 9
fi

[ "$state" = "OPEN" ] || { echo "PR #$PR is $state"; exit 3; }
if [ -n "$EXPECT" ] && ! printf '%s' "$head" | grep -q "^$EXPECT"; then
  echo "HEAD MISMATCH: current ${head:0:8}, expected ${EXPECT:0:8} — a push landed after your last look; re-review"; exit 8
fi
echo "head=${head:0:8} mergeable=$mg mergeStateStatus=$ms"

# mr_rework guard, last moment — FAIL CLOSED: an absent uzi, a failed listing or unreadable
# JSON cannot rule out an active rework (exit 4). Only a successful listing that shows the
# repo is not connected skips it; --no-rework-check is the explicit bypass for such a repo.
if [ "$REWORK_CHECK" -eq 1 ]; then
  command -v uzi >/dev/null 2>&1 || { echo "uzi CLI absent; cannot rule out an active mr_rework (pass --no-rework-check for a repo not on uzi)"; exit 4; }
  rl=$(uzi repo list --json 2>/dev/null) && printf '%s' "$rl" | jq -e 'type=="array"' >/dev/null 2>&1 \
    || { echo "uzi repo list failed; cannot rule out an active mr_rework"; exit 4; }
  repo_id=$(printf '%s' "$rl" | jq -r --arg p "$REPO" '.[]|select(.path_with_namespace==$p)|.id' | head -1)
  if [ -n "$repo_id" ]; then
    runs=$(uzi run list --json 2>/dev/null) && printf '%s' "$runs" | jq -e 'type=="array"' >/dev/null 2>&1 \
      || { echo "uzi run list failed; cannot rule out an active mr_rework"; exit 4; }
    n=$(printf '%s' "$runs" | jq -r --arg repo "$repo_id" --argjson pr "$PR" \
      '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr and ((.status|test("completed|failed|cancelled"))|not))]|length') \
      || { echo "could not parse uzi run list; cannot rule out an active mr_rework"; exit 4; }
    [ "${n:-0}" -gt 0 ] && { echo "mr_rework ACTIVE on #$PR — defer"; exit 4; }
  else
    echo "note: $REPO is not connected to uzi; rework check skipped"
  fi
fi

# Required checks on the head — FAIL CLOSED: unreadable JSON is "not merging", an EMPTY list
# is "not merging" (a head with no check runs — CI skipped, a missed dispatch — would
# otherwise merge ungated), a cancelled required check is not green (supersession), pending
# is not green. gh exits non-zero while still printing the array when a check is failing or
# pending, so its status only matters when no array came back.
cj_rc=0
cj=$(gh pr checks "$PR" --repo "$REPO" --required --json name,bucket 2>/dev/null) || cj_rc=$?
if ! printf '%s' "$cj" | jq -e 'type=="array"' >/dev/null 2>&1; then
  echo "cannot read the required checks for #$PR (gh exit $cj_rc); not merging"; exit 2
fi
if [ "$(printf '%s' "$cj" | jq 'length')" -eq 0 ]; then
  echo "no required checks reported on ${head:0:8}; not merging"; exit 2
fi
f=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="fail")]|length')
p=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="pending")]|length')
c=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="cancel")]|length')
[ "$f" -gt 0 ] && { echo "required check failing on ${head:0:8}; not merging"; exit 1; }
[ "$p" -gt 0 ] && { echo "required checks still pending on ${head:0:8}; not merging"; exit 2; }
# A required context that has not registered yet is pending too (lib/required-checks.sh).
req=""; [ -n "$base" ] && req=$(required_contexts "$REPO" "$base")
[ -n "$req" ] || { echo "cannot read the required checks of ${base:-the base branch}; not merging"; exit 2; }
miss=$(missing_required "$req" "$cj") || { echo "cannot compare the required checks on ${head:0:8}; not merging"; exit 2; }
[ "$miss" -gt 0 ] && { echo "$miss required check(s) not yet reported on ${head:0:8}; not merging"; exit 2; }
[ "$c" -gt 0 ] && { echo "a required check on ${head:0:8} was cancelled (superseded?); not merging"; exit 2; }
# A non-zero gh exit that no failing or pending check explains is a partial read (an API
# failure mid-listing), not a verdict. And at least one required check must have PASSED:
# `skipping` alone means no required gate ran; path-filtered checks that skip next to a
# passing one are fine.
[ "$cj_rc" -ne 0 ] && { echo "gh pr checks exited $cj_rc with no failing or pending check on ${head:0:8} (partial read?); not merging"; exit 2; }
ok=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="pass")]|length')
[ "$ok" -gt 0 ] || { echo "no required check passed on ${head:0:8} (only: $(printf '%s' "$cj" | jq -r '[.[].bucket]|unique|join(",")')); not merging"; exit 2; }
[ "$mg" = "CONFLICTING" ] && { echo "PR has merge conflicts (--admin does not bypass a git conflict); resolve with land-prep.sh"; exit 3; }

# ---- merge lock (repo-wide, 10-min TTL) ----------------------------------------------------
SD=$(state_dir) || SD=""
LOCK=""
if [ -n "$SD" ]; then
  LOCK="$SD/locks/merge"
  me=$(self_identity 2>/dev/null); my_uuid=$(printf '%s' "$me" | cut -f2); my_name=$(printf '%s' "$me" | cut -f1)
  if ! mkdir "$LOCK" 2>/dev/null; then
    o_uuid=$(cat "$LOCK/uuid" 2>/dev/null || echo ""); o_name=$(cat "$LOCK/name" 2>/dev/null || echo "?")
    age=$(( $(date +%s) - $(stat -c %Y "$LOCK" 2>/dev/null || stat -f %m "$LOCK" 2>/dev/null || date +%s) ))
    # Break the lock when it is ours, older than the TTL, or its owner is provably dead
    # (is_live rc 1; rc 2 = registry unknown keeps it, the TTL covers that case).
    dead=0
    if [ -n "$o_uuid" ]; then is_live "$o_uuid"; [ $? -eq 1 ] && dead=1; fi
    if [ "$o_uuid" = "$my_uuid" ] || [ "$age" -gt 600 ] || [ "$dead" -eq 1 ]; then
      rm -rf "$LOCK"; mkdir "$LOCK" 2>/dev/null || { echo "cannot take the merge lock" >&2; exit 3; }
    else
      echo "MERGE_LOCK_HELD_BY=$o_name ($o_uuid, ${age}s) — another lander is merging; wait for its main CI run to appear, then re-run"; exit 7
    fi
  fi
  printf '%s' "$my_uuid" > "$LOCK/uuid"; printf '%s' "$my_name" > "$LOCK/name"
  trap 'rm -rf "$LOCK"' EXIT
fi

# --match-head-commit makes GitHub itself refuse the merge if the head moved after the
# preflight above (a push in the window), so --expect-head is enforced server-side too.
cmd=(gh pr merge "$PR" --repo "$REPO" "--$METHOD" --match-head-commit "$head")
[ "$DELETE" -eq 1 ] && cmd+=(--delete-branch)
[ "$ADMIN" -eq 1 ] && cmd+=(--admin)
echo "+ ${cmd[*]}"
if ! "${cmd[@]}"; then
  echo "merge command refused. If this is the harness classifier, hand the user:  ! ${cmd[*]}"
  exit 3
fi

# Confirm it actually merged (gh can fall back to a deferred auto-merge and return 0).
for _ in 1 2 3 4 5 6; do
  mj=$(gh pr view "$PR" --repo "$REPO" --json state,mergeCommit 2>/dev/null || true)
  if [ "$(printf '%s' "$mj" | jq -r .state 2>/dev/null)" = "MERGED" ]; then
    # report_merged writes nothing and returns 1 while the merge commit is not populated yet
    # (state MERGED can lead mergeCommit); keep polling rather than emit empty evidence.
    if report_merged "$(printf '%s' "$mj" | jq -r '.mergeCommit.oid // empty')"; then exit 0; fi
  fi
  sleep 10
done
echo "merge command returned but #$PR is not MERGED yet (auto-merge deferred or mergeability lag); poll gh pr view --json state"
exit 9
