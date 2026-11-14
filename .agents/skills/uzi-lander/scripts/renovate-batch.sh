#!/usr/bin/env bash
# renovate-batch.sh: combine several Renovate PRs into one own branch off origin/main, so they
# share one CI run (references/renovate.md, "Batch the CI-sufficient ones").
#
# Usage: renovate-batch.sh [--repo OWNER/REPO] [--branch NAME] [--dir PATH] PR [PR...]
#
# Run from any worktree of the repo. Every PR must be open, based on main, with a renovate/*
# head; the whole call refuses before touching anything otherwise. Then it makes a new worktree
# DIR on new branch NAME (default chore/renovate-batch-<UTC date>, DIR ../<repo>-renovate-batch)
# off a fresh origin/main and cherry-picks each PR's non-merge commits unchanged (-x), in the
# order given. A PR whose pick conflicts (or whose range holds a merge commit) is aborted
# cleanly and skipped; later PRs still apply. It never pushes, never touches a renovate/*
# branch, and never regenerates a lockfile: that judgment stays with the lander.
#
# Output: KEY=VALUE lines (DIR, BRANCH, HEAD, PICKED, SKIPPED, one SKIP_REASON_<PR> each),
# then a Markdown table (Renovate PR | Update | Files) for the PR body.
# Exit: 0 all picked; 1 some skipped (the branch holds the rest); 2 usage;
#       3 a lookup or precondition failed, or nothing was picked (no usable branch).
set -uo pipefail

REPO=""; BRANCH=""; DIR=""; PRS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="${2:?}"; shift 2;;
    --branch) BRANCH="${2:?}"; shift 2;;
    --dir) DIR="${2:?}"; shift 2;;
    -h|--help) sed -n '2,19p' "$0"; exit 2;;
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
case "$BRANCH" in renovate/*) die "refusing a renovate/* branch name: $BRANCH";; esac
[ -e "$DIR" ] && die "dir exists: $DIR"
git -C "$TOP" show-ref --verify --quiet "refs/heads/$BRANCH" && die "branch exists: $BRANCH"

# Validate every PR and fetch its exact head before creating anything.
declare -A OID TITLE
for n in "${PRS[@]}"; do
  j=$(gh pr view "$n" --repo "$REPO" --json number,title,headRefName,headRefOid,baseRefName,state 2>/dev/null) \
    || die "gh pr view $n failed"
  read -r state base head oid < <(printf '%s' "$j" | jq -er '[.state,.baseRefName,.headRefName,.headRefOid]|@tsv' 2>/dev/null) \
    || die "gh pr view $n unreadable"
  [ "$state" = OPEN ] || die "#$n is $state, not OPEN"
  [ "$base" = main ] || die "#$n targets $base, not main"
  case "$head" in renovate/*) ;; *) die "#$n head $head is not renovate/*";; esac
  git -C "$TOP" fetch -q origin "+refs/pull/$n/head:refs/renovate-batch/pr-$n" 2>/dev/null || die "fetch of #$n failed"
  got=$(git -C "$TOP" rev-parse "refs/renovate-batch/pr-$n") || die "#$n fetched ref missing"
  [ "$got" = "$oid" ] || die "#$n fetched $got but gh reports head $oid (moved mid-call?)"
  OID[$n]=$oid
  TITLE[$n]=$(printf '%s' "$j" | jq -r '.title' | sed -E 's/^[a-z]+(\([^)]*\))?!?: *//; s/^[Uu]pdate +//')
done
git -C "$TOP" fetch -q origin main 2>/dev/null || die "fetch of origin main failed"
git -C "$TOP" worktree add -q -b "$BRANCH" "$DIR" origin/main 2>/dev/null || die "worktree add failed"

PICKED=(); SKIPPED=(); ROWS=(); REASONS=()
for n in "${PRS[@]}"; do
  before=$(git -C "$DIR" rev-parse HEAD)
  if [ -n "$(git -C "$DIR" rev-list --merges "origin/main..${OID[$n]}")" ]; then
    SKIPPED+=("$n"); REASONS+=("SKIP_REASON_$n=merge commit in range"); continue
  fi
  mapfile -t commits < <(git -C "$DIR" rev-list --reverse --no-merges "origin/main..${OID[$n]}")
  if [ "${#commits[@]}" -eq 0 ]; then
    SKIPPED+=("$n"); REASONS+=("SKIP_REASON_$n=no commits beyond origin/main"); continue
  fi
  if ! git -C "$DIR" cherry-pick -x "${commits[@]}" >/dev/null 2>&1; then
    git -C "$DIR" cherry-pick --abort >/dev/null 2>&1
    git -C "$DIR" reset -q --hard "$before"
    SKIPPED+=("$n"); REASONS+=("SKIP_REASON_$n=cherry-pick conflict"); continue
  fi
  files=$(git -C "$DIR" diff --name-only "$before" HEAD | sed 's/.*/`&`/' | paste -sd, - | sed 's/,/, /g')
  PICKED+=("$n"); ROWS+=("| #$n | ${TITLE[$n]} | $files |")
done
for n in "${PRS[@]}"; do git -C "$TOP" update-ref -d "refs/renovate-batch/pr-$n" 2>/dev/null; done

echo "DIR=$DIR"
echo "BRANCH=$BRANCH"
echo "HEAD=$(git -C "$DIR" rev-parse HEAD)"
echo "PICKED=${PICKED[*]:-}"
echo "SKIPPED=${SKIPPED[*]:-}"
for r in "${REASONS[@]:+${REASONS[@]}}"; do echo "$r"; done
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
