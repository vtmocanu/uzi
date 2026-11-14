#!/usr/bin/env bash
# renovate-batch.sh: combine several Renovate PRs into one own branch off origin/main, so they
# share one CI run (references/renovate.md, "Batch the CI-sufficient ones").
#
# Usage: renovate-batch.sh [--repo OWNER/REPO] [--branch NAME] [--dir PATH] PR [PR...]
#
# Run from any worktree of the repo; a relative --dir is relative to the caller's cwd. Every PR
# must be open, based on main, with a renovate/* head whose fetched commit equals the head gh
# reports; otherwise it refuses before creating a branch or worktree. It then pins BASE (the
# freshly fetched origin/main commit), makes worktree DIR on new branch NAME (default
# chore/renovate-batch-<UTC date>, DIR ../<repo>-renovate-batch) at BASE, and cherry-picks each
# PR's BASE..head non-merge commits unchanged (-x), in the order given. A PR whose pick fails
# (or whose range holds a merge commit) is skipped once its rollback is verified (HEAD back,
# clean tree, no pick in progress); later PRs still apply. An unverified rollback or a failed
# git read stops the batch (exit 4) with DIR left for inspection. It never pushes, never
# touches a renovate/* branch and never regenerates a lockfile.
#
# Output: KEY=VALUE lines (DIR, BRANCH, BASE, HEAD, PICKED, SKIPPED, one SKIP_REASON_<PR> each),
# then a Markdown table (Renovate PR | Update | Files) for the PR body.
# Exit: 0 all picked; 1 some skipped (the branch holds the rest); 2 usage;
#       3 refused, or nothing picked (no usable batch); 4 stopped mid-batch: inspect DIR.
set -uo pipefail

REPO=""; BRANCH=""; DIR=""; PRS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="${2:?}"; shift 2;;
    --branch) BRANCH="${2:?}"; shift 2;;
    --dir) DIR="${2:?}"; shift 2;;
    -h|--help) sed -n '2,21p' "$0"; exit 2;;
    -*) echo "unknown arg: $1" >&2; exit 2;;
    *) case "$1" in *[!0-9]*|'') echo "not a PR number: $1" >&2; exit 2;; esac
       PRS+=("$1"); shift;;
  esac
done
[ "${#PRS[@]}" -ge 1 ] || { echo "usage: renovate-batch.sh [--repo R] [--branch B] [--dir D] PR [PR...]" >&2; exit 2; }
die() { echo "renovate-batch: $*" >&2; exit 3; }

TOP=$(git rev-parse --show-toplevel 2>/dev/null) || die "not inside a git worktree"
if [ -z "$REPO" ]; then
  REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) || die "cannot infer --repo"
fi
[ -n "$BRANCH" ] || BRANCH="chore/renovate-batch-$(date -u +%Y-%m-%d)"
[ -n "$DIR" ] || DIR="$(dirname "$TOP")/$(basename "$TOP")-renovate-batch"
case "$DIR" in /*) ;; *) DIR="$PWD/$DIR";; esac
case "$BRANCH" in renovate/*) die "refusing a renovate/* branch name: $BRANCH";; esac
[ -e "$DIR" ] && die "dir exists: $DIR"
git -C "$TOP" show-ref --verify --quiet "refs/heads/$BRANCH" && die "branch exists: $BRANCH"

# Temporary refs are owned by this invocation alone and removed on every exit.
NS="refs/renovate-batch/$$-$RANDOM$RANDOM"
cleanup() {
  git -C "$TOP" for-each-ref --format='%(refname)' "$NS/" 2>/dev/null |
    while read -r r; do git -C "$TOP" update-ref -d "$r" 2>/dev/null; done
}
trap cleanup EXIT

git -C "$TOP" fetch -q origin main 2>/dev/null || die "fetch of origin main failed"
BASE=$(git -C "$TOP" rev-parse --verify -q 'refs/remotes/origin/main^{commit}') || die "origin/main does not resolve"

# Validate every PR and fetch its exact head before creating anything.
declare -A OID TITLE
for n in "${PRS[@]}"; do
  j=$(gh pr view "$n" --repo "$REPO" --json number,title,headRefName,headRefOid,baseRefName,state 2>/dev/null) \
    || die "gh pr view $n failed"
  read -r state base head oid < <(printf '%s' "$j" | jq -er '[.state,.baseRefName,.headRefName,.headRefOid]|@tsv' 2>/dev/null)
  [ -n "${oid:-}" ] || die "gh pr view $n unreadable"
  [ "$state" = OPEN ] || die "#$n is $state, not OPEN"
  [ "$base" = main ] || die "#$n targets $base, not main"
  case "$head" in renovate/*) ;; *) die "#$n head $head is not renovate/*";; esac
  git -C "$TOP" fetch -q origin "+refs/pull/$n/head:$NS/pr-$n" 2>/dev/null || die "fetch of #$n failed"
  got=$(git -C "$TOP" rev-parse --verify -q "$NS/pr-$n^{commit}") || die "#$n fetched ref missing"
  [ "$got" = "$oid" ] || die "#$n fetched $got but gh reports head $oid (moved mid-call?)"
  OID[$n]=$oid
  TITLE[$n]=$(printf '%s' "$j" | jq -r '.title' | sed -E 's/^[a-z]+(\([^)]*\))?!?: *//; s/^[Uu]pdate +//')
done
git -C "$TOP" worktree add -q -b "$BRANCH" "$DIR" "$BASE" 2>/dev/null || die "worktree add failed"

PICKED=(); SKIPPED=(); ROWS=(); REASONS=()
report() {
  echo "DIR=$DIR"
  echo "BRANCH=$BRANCH"
  echo "BASE=$BASE"
  echo "HEAD=$(git -C "$DIR" rev-parse HEAD 2>/dev/null || echo unknown)"
  echo "PICKED=${PICKED[*]:-}"
  echo "SKIPPED=${SKIPPED[*]:-}"
  for r in "${REASONS[@]:+${REASONS[@]}}"; do echo "$r"; done
}
stop() { report; echo "renovate-batch: stopped at #$1: $2; inspect $DIR (no later PR attempted)" >&2; exit 4; }
skip() { SKIPPED+=("$1"); REASONS+=("SKIP_REASON_$1=$2"); }
# A skip is safe only when the tree is exactly where the PR started. Every read must succeed:
# a failed lookup is "not verified", never "absent" (needs git 2.43+ for show-ref --exists).
gitpath_absent() {
  local p
  p=$(git -C "$DIR" rev-parse --git-path "$1" 2>/dev/null) && [ -n "$p" ] || return 1
  case "$p" in /*) ;; *) p="$DIR/$p";; esac
  [ ! -e "$p" ]
}
verified_at() {
  local h st
  h=$(git -C "$DIR" rev-parse --verify HEAD 2>/dev/null) && [ "$h" = "$1" ] || return 1
  st=$(git -C "$DIR" status --porcelain --untracked-files=all 2>/dev/null) && [ -z "$st" ] || return 1
  # A ref, not a file (reftable): show-ref --exists exits 2 only for absent; 0 or any error fails.
  git -C "$DIR" show-ref --exists CHERRY_PICK_HEAD >/dev/null 2>&1; [ $? -eq 2 ] || return 1
  gitpath_absent sequencer
}

for n in "${PRS[@]}"; do
  before=$(git -C "$DIR" rev-parse --verify HEAD 2>/dev/null) || stop "$n" "cannot read HEAD"
  merges=$(git -C "$DIR" rev-list --merges "$BASE..${OID[$n]}" 2>/dev/null) || stop "$n" "rev-list failed"
  if [ -n "$merges" ]; then skip "$n" "merge commit in range"; continue; fi
  list=$(git -C "$DIR" rev-list --reverse --no-merges "$BASE..${OID[$n]}" 2>/dev/null) || stop "$n" "rev-list failed"
  commits=()
  while read -r c; do [ -n "$c" ] && commits+=("$c"); done <<<"$list"
  if [ "${#commits[@]}" -eq 0 ]; then skip "$n" "no commits beyond BASE"; continue; fi
  if ! git -C "$DIR" cherry-pick -x "${commits[@]}" >/dev/null 2>&1; then
    reason="cherry-pick failed"
    [ -n "$(git -C "$DIR" diff --name-only --diff-filter=U 2>/dev/null)" ] && reason="cherry-pick conflict"
    git -C "$DIR" cherry-pick --abort >/dev/null 2>&1 || stop "$n" "$reason, then cherry-pick --abort failed"
    verified_at "$before" || stop "$n" "$reason, rollback not verified"
    skip "$n" "$reason"; continue
  fi
  names=$(git -C "$DIR" diff --name-only "$before" HEAD 2>/dev/null) || stop "$n" "diff failed"
  files=$(printf '%s\n' "$names" | sed 's/.*/`&`/' | paste -sd, - | sed 's/,/, /g')
  PICKED+=("$n"); ROWS+=("| #$n | ${TITLE[$n]} | $files |")
done

report
if [ "${#PICKED[@]}" -eq 0 ]; then
  echo "renovate-batch: nothing picked; remove $DIR and branch $BRANCH" >&2
  exit 3
fi
echo
echo "| Renovate PR | Update | Files |"
echo "|---|---|---|"
printf '%s\n' "${ROWS[@]}"
[ "${#SKIPPED[@]}" -eq 0 ] || exit 1
exit 0
