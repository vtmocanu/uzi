#!/usr/bin/env bash
# merge.sh — the guarded merge: re-check the head and the rework lane at the last moment,
# admin-merge past the ruleset, then CONFIRM it merged and print the merge SHA for the
# post-merge watch. The decision to merge is the caller's; this is only the mechanics.
#
# Usage: merge.sh OWNER/REPO PR [--expect-head SHA] [--method squash|merge] [--no-admin] [--no-delete-branch]
#   --expect-head   the head you reviewed/watched; a different current head refuses (exit 8)
#                   so a push that landed after your last look is never merged unseen.
#   --method        squash (default; the convention for agent/issue-* branches) or merge
#                   (uzi-release uses merge commits so the subject keeps the issue branch).
#   --no-admin      drop --admin (needs the ruleset satisfied: review + up-to-date + checks).
#
# Exit codes:
#   0  merged — prints MERGE_SHA=<sha>; next: watch-run-ci.sh --sha <sha>
#   1  a required check is failing on the head — not merged
#   2  usage, or required checks still pending — not merged
#   3  gh error, or the merge command was refused (classifier block, ruleset, conflict):
#      the exact command is printed for the user to run via a `!` line
#   4  an mr_rework run is active on this MR — defer
#   8  head mismatch vs --expect-head
#   9  the merge command returned but the PR is not MERGED (auto-merge deferred / async
#      mergeability lag) — poll `gh pr view --json state` yourself before the CI watch
set -uo pipefail

REPO=""; PR=""; EXPECT=""; METHOD="squash"; ADMIN=1; DELETE=1
while [ $# -gt 0 ]; do
  case "$1" in
    --expect-head) EXPECT="${2:?}"; shift 2;;
    --method) METHOD="${2:?}"; shift 2;;
    --no-admin) ADMIN=0; shift;;
    --no-delete-branch) DELETE=0; shift;;
    -h|--help) sed -n '2,24p' "$0"; exit 2;;
    -*) echo "unknown flag: $1" >&2; exit 2;;
    *) if [ -z "$REPO" ]; then REPO="$1"; elif [ -z "$PR" ]; then PR="$1"; else echo "unexpected arg: $1" >&2; exit 2; fi; shift;;
  esac
done
[ -n "$REPO" ] && [ -n "$PR" ] || { echo "usage: merge.sh OWNER/REPO PR [--expect-head SHA] [--method squash|merge] [--no-admin]" >&2; exit 2; }
case "$METHOD" in squash|merge) ;; *) echo "bad --method" >&2; exit 2;; esac

pj=$(gh pr view "$PR" --repo "$REPO" --json state,headRefOid,mergeStateStatus,mergeable 2>/dev/null) || { echo "gh pr view failed" >&2; exit 3; }
state=$(printf '%s' "$pj" | jq -r .state); head=$(printf '%s' "$pj" | jq -r .headRefOid)
ms=$(printf '%s' "$pj" | jq -r .mergeStateStatus); mg=$(printf '%s' "$pj" | jq -r .mergeable)
[ "$state" = "OPEN" ] || { echo "PR #$PR is $state"; exit 3; }
if [ -n "$EXPECT" ] && ! printf '%s' "$head" | grep -q "^$EXPECT"; then
  echo "HEAD MISMATCH: current ${head:0:8}, expected ${EXPECT:0:8} — a push landed after your last look; re-review"; exit 8
fi
echo "head=${head:0:8} mergeable=$mg mergeStateStatus=$ms"

# mr_rework guard, last moment.
if command -v uzi >/dev/null 2>&1; then
  repo_id=$(uzi repo list --json 2>/dev/null | jq -r --arg p "$REPO" '.[]|select(.path_with_namespace==$p)|.id' 2>/dev/null | head -1 || true)
  if [ -n "$repo_id" ]; then
    n=$(uzi run list --json 2>/dev/null | jq -r --arg repo "$repo_id" --argjson pr "$PR" \
      '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr and ((.status|test("completed|failed|cancelled"))|not))]|length' 2>/dev/null || echo "?")
    [ "$n" = "?" ] && { echo "uzi run list failed; cannot rule out an active mr_rework"; exit 4; }
    [ "${n:-0}" -gt 0 ] && { echo "mr_rework ACTIVE on #$PR — defer"; exit 4; }
  fi
fi

# Required checks on the head.
cj=$(gh pr checks "$PR" --repo "$REPO" --required --json bucket 2>/dev/null || true)
if printf '%s' "$cj" | jq -e . >/dev/null 2>&1; then
  f=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="fail")]|length'); p=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="pending")]|length')
  [ "$f" -gt 0 ] && { echo "required check failing on ${head:0:8}; not merging"; exit 1; }
  [ "$p" -gt 0 ] && { echo "required checks still pending on ${head:0:8}; not merging"; exit 2; }
fi
[ "$mg" = "CONFLICTING" ] && { echo "PR has merge conflicts (--admin does not bypass a git conflict); resolve with land-prep.sh"; exit 3; }

cmd=(gh pr merge "$PR" --repo "$REPO" "--$METHOD")
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
    sha=$(printf '%s' "$mj" | jq -r '.mergeCommit.oid // empty')
    echo "MERGED #$PR"; echo "MERGE_SHA=$sha"
    echo "next: watch-run-ci.sh --sha $sha --interval 60"
    exit 0
  fi
  sleep 10
done
echo "merge command returned but #$PR is not MERGED yet (auto-merge deferred or mergeability lag); poll gh pr view --json state"
exit 9
