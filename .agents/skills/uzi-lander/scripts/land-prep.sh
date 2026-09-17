#!/usr/bin/env bash
# land-prep.sh — the deterministic base-hygiene pipeline for a PR branch that cannot merge
# as-is: sibling worktree → fetch → rebase onto the base → renumber colliding migrations
# → run the touched component gates → force-with-lease push. Everything that needs no
# judgment; it STOPS (with a distinct exit code and the worktree left in place) at the
# points that do — a rebase conflict, a renumber that touched more than the migrations
# directory, a red gate.
#
# Usage: land-prep.sh OWNER/REPO PR [--worktree DIR] [--skip-rebase] [--gate auto|none|api,web,agent,controller]
#                     [--no-push] [--repo-root DIR]
#   --worktree DIR   where the PR branch is checked out (default: <repo-root>-land-<PR>,
#                    a sibling of the repo root). Reused if it already exists on the branch.
#   --skip-rebase    re-entry after you resolved a conflict by hand (`git rebase --continue`
#                    done) or after a manual renumber fix: skips straight to gates + push.
#   --gate           which `task gate:<c>` targets to run before pushing. `auto` (default)
#                    derives them from the changed paths (api/, web/, agent/, controller/);
#                    `none` skips gates (only when CI is the arbiter, e.g. a docs-only PR).
#   --no-push        stop before the push (inspect the worktree first).
#   --repo-root DIR  the checkout whose .git the worktree is added to (default: cwd's root).
#
# Guards, in order: no active mr_rework on the MR (a rework push would collide); the remote
# branch head is read at the start and the push is `--force-with-lease=<branch>:<that head>`,
# so a push that landed in between (a rework, a human) fails instead of being overwritten;
# the branch must be the PR's own head branch (never main, never a renovate branch).
#
# Exit codes (callers branch on these; keep them stable):
#   0  pushed (or, with --no-push, prepared) — prints NEW_HEAD=<sha>; a push re-triggers
#      CodeRabbit, so re-run watch-pr.sh afterwards
#   2  usage
#   3  gh/git error, or refusing (branch is main / not the PR's head branch)
#   4  an mr_rework run is active on this MR — defer (scripts/wait-mrrework.sh)
#   5  rebase conflict — the worktree is left mid-rebase: resolve, `git add`, `git rebase
#      --continue`, then re-run with --skip-rebase
#   6  migration renumber needs hand work — the helper's report is printed and the tree is
#      left dirty in the worktree: fix the other references, commit, re-run --skip-rebase
#   7  a gate failed — log path printed; fix in the worktree, commit, re-run --skip-rebase
#   8  the remote head moved since the start (lease would fail) — re-run from scratch
set -uo pipefail

REPO=""; PR=""; WT=""; SKIP_REBASE=0; GATE="auto"; PUSH=1; ROOT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --worktree) WT="${2:?}"; shift 2;;
    --skip-rebase) SKIP_REBASE=1; shift;;
    --gate) GATE="${2:?}"; shift 2;;
    --no-push) PUSH=0; shift;;
    --repo-root) ROOT="${2:?}"; shift 2;;
    -h|--help) sed -n '2,38p' "$0"; exit 2;;
    -*) echo "unknown flag: $1" >&2; exit 2;;
    *) if [ -z "$REPO" ]; then REPO="$1"; elif [ -z "$PR" ]; then PR="$1"; else echo "unexpected arg: $1" >&2; exit 2; fi; shift;;
  esac
done
[ -n "$REPO" ] && [ -n "$PR" ] || { echo "usage: land-prep.sh OWNER/REPO PR [--worktree DIR] [--skip-rebase] [--gate auto|none|list] [--no-push]" >&2; exit 2; }

if [ -z "$ROOT" ]; then
  ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "not inside a git checkout; pass --repo-root" >&2; exit 3; }
fi
# The worktree default is a SIBLING of the repo root, the repo convention (never the
# root worktree itself, which stays on main).
[ -n "$WT" ] || WT="${ROOT}-land-${PR}"

log() { printf '%s [land-prep #%s] %s\n' "$(date +%H:%M:%S)" "$PR" "$*"; }

# ---- PR coordinates ---------------------------------------------------------------------
pj=$(gh pr view "$PR" --repo "$REPO" --json state,headRefName,baseRefName,headRefOid,headRepositoryOwner 2>/dev/null) \
  || { echo "gh pr view $PR failed" >&2; exit 3; }
state=$(printf '%s' "$pj" | jq -r .state)
BRANCH=$(printf '%s' "$pj" | jq -r .headRefName)
BASE=$(printf '%s' "$pj" | jq -r .baseRefName)
HEAD0=$(printf '%s' "$pj" | jq -r .headRefOid)
[ "$state" = "OPEN" ] || { echo "PR #$PR is $state, nothing to prepare" >&2; exit 3; }
case "$BRANCH" in
  main|master|"$BASE") echo "refusing: PR head branch is '$BRANCH'" >&2; exit 3;;
  renovate/*) echo "refusing: '$BRANCH' is a renovate branch (renovate force-pushes it; make your own branch — see uzi-release)" >&2; exit 3;;
esac
log "branch=$BRANCH base=$BASE head=${HEAD0:0:8}"

# ---- guard: mr_rework ---------------------------------------------------------------------
mrw_check() {
  local repo_id
  command -v uzi >/dev/null 2>&1 || { log "uzi CLI absent; mr_rework check skipped (verify by hand)"; return 0; }
  repo_id=$(uzi repo list --json 2>/dev/null | jq -r --arg p "$REPO" '.[]|select(.path_with_namespace==$p)|.id' 2>/dev/null | head -1 || true)
  [ -n "$repo_id" ] || { log "repo not connected to uzi; mr_rework check skipped"; return 0; }
  local n
  n=$(uzi run list --json 2>/dev/null | jq -r --arg repo "$repo_id" --argjson pr "$PR" \
    '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr and ((.status|test("completed|failed|cancelled"))|not))]|length' 2>/dev/null || echo "?")
  if [ "$n" = "?" ]; then log "uzi run list failed; cannot rule out an active mr_rework"; return 4; fi
  if [ "${n:-0}" -gt 0 ]; then log "an mr_rework run is ACTIVE on #$PR — defer (scripts/wait-mrrework.sh)"; return 4; fi
  return 0
}
mrw_check || exit 4

# ---- worktree -----------------------------------------------------------------------------
git -C "$ROOT" fetch origin "$BRANCH" "$BASE" --quiet || { echo "git fetch failed" >&2; exit 3; }
if [ -d "$WT" ]; then
  cur=$(git -C "$WT" rev-parse --abbrev-ref HEAD 2>/dev/null || true)
  [ "$cur" = "$BRANCH" ] || { echo "$WT exists but is on '$cur', not '$BRANCH'; pass --worktree" >&2; exit 3; }
  log "reusing worktree $WT"
else
  git -C "$ROOT" worktree add "$WT" -B "$BRANCH" "origin/$BRANCH" --quiet || { echo "git worktree add failed" >&2; exit 3; }
  log "worktree $WT on $BRANCH"
fi
cd "$WT" || exit 3
# Record the lease target NOW (the remote head this run started from).
LEASE=$(git rev-parse "origin/$BRANCH")

# ---- rebase -----------------------------------------------------------------------------
if [ "$SKIP_REBASE" -eq 0 ]; then
  if [ -n "$(git status --porcelain)" ]; then echo "worktree is dirty; commit or stash first (or --skip-rebase after a manual step)" >&2; exit 3; fi
  if git rebase "origin/$BASE" --quiet; then
    log "rebased onto origin/$BASE ($(git rev-list --count "origin/$BASE..HEAD") commits)"
  else
    log "REBASE CONFLICT — worktree left mid-rebase in $WT:"
    git status --short | grep -E '^(UU|AA|DU|UD|DD)' || git status --short
    echo "RESULT=conflict WORKTREE=$WT"
    exit 5
  fi
else
  if git rev-parse -q --verify REBASE_HEAD >/dev/null 2>&1 || [ -d .git/rebase-merge ] || [ -d .git/rebase-apply ]; then
    echo "a rebase is still in progress in $WT; finish it (git rebase --continue) first" >&2; exit 5
  fi
  log "skip-rebase: continuing from $(git rev-parse --short HEAD)"
fi

# ---- migrations -----------------------------------------------------------------------------
added=$(git diff --name-only --diff-filter=A "origin/$BASE...HEAD" -- api/internal/store/migrations/ | xargs -n1 basename 2>/dev/null || true)
if [ -n "$added" ]; then
  collide=0
  while IFS= read -r m; do
    [ -z "$m" ] && continue
    pfx=$(printf '%s' "$m" | grep -oE '^[0-9]+')
    if git ls-tree --name-only "origin/$BASE" -- api/internal/store/migrations/ | xargs -n1 basename | grep -qE "^${pfx}_"; then collide=1; fi
  done <<< "$added"
  if [ "$collide" -eq 1 ]; then
    log "migration number collision with origin/$BASE — running task migration:renumber"
    if ! out=$(task migration:renumber 2>&1); then
      printf '%s\n' "$out"; echo "RESULT=renumber_refused WORKTREE=$WT"; exit 6
    fi
    printf '%s\n' "$out"
    # The helper leaves renames staged and comment edits unstaged, and REPORTS other
    # references it will not touch. Commit only when the dirty set is confined to the
    # migrations directory; anything else needs a hand fix first.
    outside=$(git status --porcelain | awk '{print $NF}' | grep -v '^api/internal/store/migrations/' || true)
    if [ -n "$outside" ] || printf '%s' "$out" | grep -qiE 'other reference|fix by hand|manual'; then
      echo "RESULT=renumber_needs_hand_fix WORKTREE=$WT"
      exit 6
    fi
    git add -A api/internal/store/migrations/
    git commit -q -m "chore: renumber migrations above the ${BASE} head (#${PR})" || { echo "commit failed" >&2; exit 3; }
    log "renumbered and committed"
  fi
fi

# ---- gates --------------------------------------------------------------------------------
if [ "$GATE" != "none" ]; then
  if [ "$GATE" = "auto" ]; then
    changed=$(git diff --name-only "origin/$BASE...HEAD")
    GATES=()
    printf '%s\n' "$changed" | grep -q '^api/' && GATES+=(gate:api)
    printf '%s\n' "$changed" | grep -q '^web/' && GATES+=(gate:web)
    printf '%s\n' "$changed" | grep -q '^agent/' && GATES+=(gate:agent)
    printf '%s\n' "$changed" | grep -q '^controller/' && GATES+=(gate:controller)
    [ "${#GATES[@]}" -gt 0 ] || GATES=(gate:repo)
  else
    IFS=',' read -r -a parts <<< "$GATE"
    GATES=()
    for p in "${parts[@]}"; do GATES+=("gate:$p"); done
  fi
  for g in "${GATES[@]}"; do
    logf=$(mktemp "${TMPDIR:-/tmp}/land-prep-${PR}-${g//:/-}.XXXXXX")
    log "running task $g -> $logf"
    if ! task "$g" > "$logf" 2>&1; then
      tail -n 40 "$logf"
      echo "RESULT=gate_failed GATE=$g LOG=$logf WORKTREE=$WT"
      exit 7
    fi
    log "task $g green"
  done
fi

# ---- workflow-file note ----------------------------------------------------------------------
wf=$(git diff --name-only "origin/$BASE..HEAD" -- .github/workflows/ || true)
[ -n "$wf" ] && log "NOTE: branch changes workflow files (your token has workflow scope; the worker's does not): $(printf '%s' "$wf" | tr '\n' ' ')"

NEW_HEAD=$(git rev-parse HEAD)
if [ "$PUSH" -eq 0 ]; then
  echo "RESULT=prepared NEW_HEAD=$NEW_HEAD WORKTREE=$WT (no push)"
  exit 0
fi

# ---- push -----------------------------------------------------------------------------------
mrw_check || exit 4
remote_now=$(git ls-remote origin "refs/heads/$BRANCH" | cut -f1)
if [ "$remote_now" != "$LEASE" ]; then
  log "remote $BRANCH moved ${LEASE:0:8} -> ${remote_now:0:8} since the start; re-run from scratch"
  echo "RESULT=remote_moved"
  exit 8
fi
if git push --force-with-lease="${BRANCH}:${LEASE}" origin "HEAD:refs/heads/${BRANCH}" --quiet; then
  log "pushed ${NEW_HEAD:0:8} (lease ${LEASE:0:8}); a re-review follows — run watch-pr.sh"
  echo "RESULT=pushed NEW_HEAD=$NEW_HEAD OLD_HEAD=$HEAD0 WORKTREE=$WT"
  exit 0
fi
echo "RESULT=push_failed"
exit 3
