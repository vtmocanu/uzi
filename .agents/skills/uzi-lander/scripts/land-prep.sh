#!/usr/bin/env bash
# land-prep.sh — the deterministic base-hygiene pipeline for a PR branch that cannot merge
# as-is: sibling worktree → fetch → rebase onto the base → renumber colliding migrations
# → run the touched component gates → force-with-lease push. Everything that needs no
# judgment; it STOPS (with a distinct exit code and the worktree left in place) at the
# points that do — a rebase conflict, a renumber that touched more than the migrations
# directory, a red gate.
#
# Usage: land-prep.sh OWNER/REPO PR [--worktree DIR] [--skip-rebase] [--gate auto|none|api,web,agent,controller]
#                     [--no-push] [--no-rework-check] [--allow-changelog-removals] [--repo-root DIR]
#   --worktree DIR   where the PR branch is checked out (default: <repo-root>-land-<PR>,
#                    a sibling of the repo root). Reused if it already exists on the branch.
#   --skip-rebase    re-entry after you resolved a conflict by hand (`git rebase --continue`
#                    done) or after a manual renumber fix: skips straight to gates + push.
#   --fresh          start the landing over: reset the (clean) worktree to origin/<branch>
#                    and drop the recorded lease. The answer to RESULT=remote_moved.
#                    Local commits the remote lacks are kept under
#                    refs/uzi-lander/fresh-backup/pr-<PR>/<old HEAD sha> (FRESH_BACKUP=) and
#                    named, for a cherry-pick back after the rebase.
#   --gate           which `task gate:<c>` targets to run before pushing. `auto` (default)
#                    derives them from the changed paths (api/, web/, agent/, controller/);
#                    `none` skips gates (only when CI is the arbiter, e.g. a docs-only PR).
#   --no-push        stop before the push (inspect the worktree first).
#   --no-rework-check  skip the mr_rework guard (ONLY for a repo that is not on uzi).
#   --allow-changelog-removals  push even though the branch deletes CHANGELOG.md lines the
#                    base carries (a deliberate reword); without it that stops with exit 9.
#   --repo-root DIR  the checkout whose .git the worktree is added to (default: cwd's root).
#
# Guards, in order: no active mr_rework on the MR (a rework push would collide; the check
# fails CLOSED when uzi cannot answer); the remote branch head and base SHA are persisted
# when the landing starts; the push uses a branch lease and refuses if either coordinate
# moved during gates or a --skip-rebase re-entry; the branch must be the PR's own head
# branch in THIS repository (never main, never a renovate branch, never a fork's).
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
#   8  the remote head or base moved since this landing started, or the worktree and the
#      remote diverged with no landing on record — start over with --fresh
#   9  the branch deletes CHANGELOG.md lines the base carries (usually a conflict resolved
#      from a stale copy, e.g. a --fresh backup); restore them, or pass
#      --allow-changelog-removals for a deliberate reword
set -uo pipefail

REPO=""; PR=""; WT=""; SKIP_REBASE=0; GATE="auto"; PUSH=1; ROOT=""; REWORK_CHECK=1; FRESH=0; ALLOW_CL_RM=0
while [ $# -gt 0 ]; do
  case "$1" in
    --worktree) WT="${2:?}"; shift 2;;
    --skip-rebase) SKIP_REBASE=1; shift;;
    --fresh) FRESH=1; shift;;
    --gate) GATE="${2:?}"; shift 2;;
    --no-push) PUSH=0; shift;;
    --no-rework-check) REWORK_CHECK=0; shift;;
    --allow-changelog-removals) ALLOW_CL_RM=1; shift;;
    --repo-root) ROOT="${2:?}"; shift 2;;
    -h|--help) sed -n '2,50p' "$0"; exit 2;;
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
pj=$(gh pr view "$PR" --repo "$REPO" --json state,headRefName,baseRefName,headRefOid,headRepository,headRepositoryOwner 2>/dev/null) \
  || { echo "gh pr view $PR failed" >&2; exit 3; }
state=$(printf '%s' "$pj" | jq -r .state)
BRANCH=$(printf '%s' "$pj" | jq -r .headRefName)
BASE=$(printf '%s' "$pj" | jq -r .baseRefName)
HEAD0=$(printf '%s' "$pj" | jq -r .headRefOid)
[ "$state" = "OPEN" ] || { echo "PR #$PR is $state, nothing to prepare" >&2; exit 3; }
# The head must be AFFIRMATIVELY this repository: owner/name equal to OWNER/REPO (case-
# insensitive, GitHub's rule). A fork, a same-owner different repo, or a deleted head repo
# (empty fields) means origin/$BRANCH is not the PR's head, and a lease push could overwrite
# an unrelated same-named branch.
head_repo=$(printf '%s' "$pj" | jq -r '"\(.headRepositoryOwner.login // "")/\(.headRepository.name // "")"')
if [ "$(printf '%s' "$head_repo" | tr '[:upper:]' '[:lower:]')" != "$(printf '%s' "$REPO" | tr '[:upper:]' '[:lower:]')" ]; then
  echo "refusing: PR #$PR's head repository is '${head_repo}', not '$REPO' (fork, other repo, or deleted); land it by hand" >&2; exit 3
fi
case "$BRANCH" in
  main|master|"$BASE") echo "refusing: PR head branch is '$BRANCH'" >&2; exit 3;;
  renovate/*) echo "refusing: '$BRANCH' is a renovate branch (renovate force-pushes it; make your own branch — see references/renovate.md)" >&2; exit 3;;
esac
log "branch=$BRANCH base=$BASE head=${HEAD0:0:8}"

# ---- guard: mr_rework ---------------------------------------------------------------------
# FAIL CLOSED: an absent `uzi`, a failed listing, or unreadable JSON cannot rule out an
# active rework, so each is a refusal (exit 4). Only a listing that SUCCEEDED and shows the
# repo is not connected to uzi skips the check; --no-rework-check is the explicit bypass
# for a repo that is not on uzi at all.
mrw_check() {
  local rl repo_id runs n
  [ "$REWORK_CHECK" -eq 1 ] || { log "rework check skipped by --no-rework-check"; return 0; }
  command -v uzi >/dev/null 2>&1 || { log "uzi CLI absent; cannot rule out an active mr_rework (pass --no-rework-check for a repo not on uzi)"; return 4; }
  rl=$(uzi repo list --json 2>/dev/null) && printf '%s' "$rl" | jq -e 'type=="array"' >/dev/null 2>&1 \
    || { log "uzi repo list failed; cannot rule out an active mr_rework"; return 4; }
  repo_id=$(printf '%s' "$rl" | jq -r --arg p "$REPO" '.[]|select(.path_with_namespace==$p)|.id' | head -1)
  [ -n "$repo_id" ] || { log "$REPO is not connected to uzi; mr_rework check skipped"; return 0; }
  runs=$(uzi run list --json 2>/dev/null) && printf '%s' "$runs" | jq -e 'type=="array"' >/dev/null 2>&1 \
    || { log "uzi run list failed; cannot rule out an active mr_rework"; return 4; }
  n=$(printf '%s' "$runs" | jq -r --arg repo "$repo_id" --argjson pr "$PR" \
    '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr and ((.status|test("completed|failed|cancelled"))|not))]|length') \
    || { log "could not parse uzi run list; cannot rule out an active mr_rework"; return 4; }
  if [ "${n:-0}" -gt 0 ]; then log "an mr_rework run is ACTIVE on #$PR — defer (scripts/wait-mrrework.sh)"; return 4; fi
  return 0
}
mrw_check || exit 4

# ---- worktree -----------------------------------------------------------------------------
git -C "$ROOT" fetch origin "$BRANCH" "$BASE" --quiet || { echo "git fetch failed" >&2; exit 3; }
base_now=$(git -C "$ROOT" rev-parse "origin/$BASE" 2>/dev/null) || { echo "cannot resolve origin/$BASE" >&2; exit 3; }
# A branch can be checked out in only one worktree: if one already holds it (this session
# or an earlier one made it), reuse that path instead of failing on `worktree add`.
existing=$(git -C "$ROOT" worktree list --porcelain | awk -v b="refs/heads/$BRANCH" '$1=="worktree"{p=$2} $1=="branch" && $2==b {print p}' | head -1)
if [ -n "$existing" ] && [ "$existing" != "$WT" ]; then
  log "branch $BRANCH is already checked out at $existing; using it"
  WT="$existing"
fi
if [ -d "$WT" ]; then
  cur=$(git -C "$WT" rev-parse --abbrev-ref HEAD 2>/dev/null || true)
  [ "$cur" = "$BRANCH" ] || { echo "$WT exists but is on '$cur', not '$BRANCH'; pass --worktree" >&2; exit 3; }
  log "reusing worktree $WT"
else
  git -C "$ROOT" worktree add "$WT" -B "$BRANCH" "origin/$BRANCH" --quiet || { echo "git worktree add failed" >&2; exit 3; }
  log "worktree $WT on $BRANCH"
fi
cd "$WT" || exit 3
# A dirty worktree stops everything BEFORE any lease is recorded: a fast-forward that a
# dirty tree silently prevented, with the lease written anyway, is exactly how a later
# clean re-run force-pushes a stale local head over the remote.
if [ -n "$(git status --porcelain)" ]; then
  if [ "$SKIP_REBASE" -eq 1 ]; then echo "worktree is dirty; commit the manual fix first, then --skip-rebase" >&2; else echo "worktree is dirty; commit or stash first" >&2; fi
  exit 3
fi
# The lease is the remote head this LANDING started from, persisted in the worktree's git
# dir. Every later run of the same landing (--skip-rebase, or a plain restart after a
# --no-push or a failed gate) pushes against THAT head, never a refreshed origin/$BRANCH:
# otherwise a restart would accept an intervening remote commit into the lease and then
# overwrite it with the stale local branch. A landing ends when the push succeeds (the file
# is removed) or when you start over with --fresh, which resets the worktree to the remote.
GIT_DIR=$(git rev-parse --git-dir)
LEASE_FILE="$GIT_DIR/uzi-lander-lease-$PR"
BASE_FILE="$GIT_DIR/uzi-lander-base-$PR"
remote_now=$(git ls-remote origin "refs/heads/$BRANCH" | cut -f1)
[ -n "$remote_now" ] || { echo "cannot read the remote head of $BRANCH" >&2; exit 3; }
if [ "$FRESH" -eq 1 ]; then
  [ -z "$(git status --porcelain)" ] || { echo "worktree is dirty; --fresh would discard it" >&2; exit 3; }
  # The reset would silently drop local commits the remote lacks (a local fix committed
  # after the rebase, #1574). Keep the old HEAD under a backup ref and name the commits
  # whose patch the remote does not carry, so they can be cherry-picked back.
  # A failed cherry must not read as "nothing local": abort before the reset.
  cherry=$(git cherry "origin/$BRANCH" HEAD) || { echo "git cherry failed; --fresh refuses to reset without knowing which local commits it would drop" >&2; exit 3; }
  local_only=$(printf '%s\n' "$cherry" | awk '$1=="+"{print $2}')
  if [ -n "$local_only" ]; then
    # One immutable ref per reset (keyed by the old HEAD), so a later --fresh never
    # overwrites an earlier backup.
    backup="refs/uzi-lander/fresh-backup/pr-$PR/$(git rev-parse HEAD)"
    git update-ref "$backup" HEAD || { echo "could not write $backup; not resetting" >&2; exit 3; }
    log "--fresh: kept the old HEAD $(git rev-parse --short HEAD) as $backup; local commit(s) not on origin/$BRANCH (re-apply with git cherry-pick after the rebase):"
    while IFS= read -r c; do log "  $(git log -1 --format='%h %s' "$c")"; done <<< "$local_only"
    echo "FRESH_BACKUP=$backup"
  fi
  git reset -q --hard "origin/$BRANCH"; rm -f "$LEASE_FILE" "$BASE_FILE"; log "--fresh: worktree reset to origin/$BRANCH (${remote_now:0:8})"
fi
if [ -f "$LEASE_FILE" ]; then
  LEASE=$(cat "$LEASE_FILE")
  [ -f "$BASE_FILE" ] || { log "landing has no recorded base; start over with --fresh"; echo "RESULT=base_unknown"; exit 8; }
  BASE_SHA=$(cat "$BASE_FILE")
  if [ "$remote_now" != "$LEASE" ]; then
    log "remote $BRANCH moved ${LEASE:0:8} -> ${remote_now:0:8} since this landing started; start over with --fresh (resets the worktree)"
    echo "RESULT=remote_moved"; exit 8
  fi
  if [ "$base_now" != "$BASE_SHA" ]; then
    log "remote $BASE moved ${BASE_SHA:0:8} -> ${base_now:0:8} since this landing started; start over with --fresh"
    echo "RESULT=base_moved"; exit 8
  fi
else
  [ "$SKIP_REBASE" -eq 0 ] || { echo "no lease recorded for #$PR in this worktree; run once without --skip-rebase" >&2; exit 8; }
  # A NEW landing: the worktree must already contain the remote head, or be fast-forwarded
  # to it NOW (a failed fast-forward records no lease); anything else is a divergence no
  # lease can vouch for.
  if git merge-base --is-ancestor HEAD "origin/$BRANCH"; then
    if ! git merge -q --ff-only "origin/$BRANCH"; then
      echo "fast-forward of $WT to origin/$BRANCH failed; no lease recorded" >&2; exit 3
    fi
    [ "$(git rev-parse HEAD)" = "$remote_now" ] || { echo "worktree is not at the remote head after fast-forward; no lease recorded" >&2; exit 3; }
  elif ! git merge-base --is-ancestor "origin/$BRANCH" HEAD; then
    log "worktree HEAD $(git rev-parse --short HEAD) and remote ${remote_now:0:8} have diverged with no landing in progress; --fresh resets the worktree to the remote"
    echo "RESULT=diverged"; exit 8
  fi
  LEASE="$remote_now"
  BASE_SHA="$base_now"
  printf '%s' "$LEASE" > "$LEASE_FILE"
  printf '%s' "$BASE_SHA" > "$BASE_FILE"
fi

# ---- rebase -----------------------------------------------------------------------------
if [ "$SKIP_REBASE" -eq 0 ]; then
  if git rebase "origin/$BASE" --quiet; then
    log "rebased onto origin/$BASE ($(git rev-list --count "origin/$BASE..HEAD") commits)"
  else
    log "REBASE CONFLICT — worktree left mid-rebase in $WT:"
    git status --short | grep -E '^(UU|AA|DU|UD|DD)' || git status --short
    echo "RESULT=conflict WORKTREE=$WT"
    exit 5
  fi
else
  # Resolve the state dirs through git: in a worktree `.git` is a FILE, so `.git/rebase-merge`
  # never exists there. REBASE_HEAD is not a signal: git can leave it behind after a rebase
  # that completed (seen on #1574, 2026-09-23), which blocked a finished landing.
  if [ -d "$(git rev-parse --git-path rebase-merge)" ] || [ -d "$(git rev-parse --git-path rebase-apply)" ]; then
    echo "a rebase is still in progress in $WT; finish it (git rebase --continue) first" >&2; exit 5
  fi
  log "skip-rebase: continuing from $(git rev-parse --short HEAD)"
fi

# ---- migrations -----------------------------------------------------------------------------
# Read each listing into a variable first and fail CLOSED on a git error. Piping
# `git ls-tree | xargs basename | grep -q` under pipefail made an early grep match SIGPIPE
# xargs, so the pipeline reported failure on a real collision and the renumber was skipped.
if ! added_paths=$(git diff --name-only --diff-filter=A "origin/$BASE...HEAD" -- api/internal/store/migrations/); then
  echo "cannot list the branch's added migrations" >&2; exit 3
fi
if [ -n "$added_paths" ]; then
  if ! base_paths=$(git ls-tree --name-only "origin/$BASE" -- api/internal/store/migrations/); then
    echo "cannot list origin/$BASE migrations" >&2; exit 3
  fi
  base_names=$(printf '%s\n' "$base_paths" | sed 's|.*/||')
  collide=0
  while IFS= read -r m; do
    [ -z "$m" ] && continue
    pfx=$(printf '%s' "${m##*/}" | grep -oE '^[0-9]+' || true)
    [ -n "$pfx" ] || continue
    if printf '%s\n' "$base_names" | grep -E "^${pfx}_" >/dev/null; then collide=1; fi
  done <<< "$added_paths"
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

# ---- CHANGELOG guard ------------------------------------------------------------------------
# A rebase conflict resolved from a stale copy (a --fresh backup, an older branch state)
# silently deletes entries that landed on the base meanwhile. The branch adds its own entry;
# it has no business removing the base's.
if [ "$ALLOW_CL_RM" -eq 0 ]; then
  if ! cl_diff=$(git diff --unified=0 "origin/$BASE..HEAD" -- CHANGELOG.md); then
    echo "cannot diff CHANGELOG.md against origin/$BASE" >&2; exit 3
  fi
  removed=$(printf '%s\n' "$cl_diff" | grep -E '^-' | grep -vE '^--- (a/|/dev/null)' || true)
  if [ -n "$removed" ]; then
    log "branch deletes CHANGELOG.md line(s) that origin/$BASE carries:"
    printf '%s\n' "$removed" | cut -c1-160
    echo "RESULT=changelog_removal WORKTREE=$WT"
    exit 9
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
  # A fresh sibling worktree has no node_modules, and deps-check then fails the gate as an
  # instrument failure. Install from the lockfile, always with --ignore-scripts (agent/'s
  # agent-browser postinstall rewrites a host-wide binary; deps-check-gate.sh).
  for g in "${GATES[@]}"; do
    case "$g" in gate:web) pkg=web;; gate:agent) pkg=agent;; *) continue;; esac
    if [ -f "$pkg/package-lock.json" ] && [ ! -d "$pkg/node_modules" ]; then
      npmlog=$(mktemp "${TMPDIR:-/tmp}/land-prep-${PR}-npm-ci-${pkg}.XXXXXX")
      log "installing $pkg/ dependencies (npm ci --ignore-scripts) -> $npmlog"
      if ! (cd "$pkg" && npm ci --ignore-scripts --no-audit --no-fund) > "$npmlog" 2>&1; then
        tail -n 40 "$npmlog"
        echo "RESULT=gate_failed GATE=$g LOG=$npmlog WORKTREE=$WT"; exit 7
      fi
    fi
  done
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
base_remote_now=$(git ls-remote origin "refs/heads/$BASE" | cut -f1)
[ -n "$base_remote_now" ] || { echo "cannot read the remote head of $BASE" >&2; exit 3; }
if [ "$base_remote_now" != "$BASE_SHA" ]; then
  log "remote $BASE moved ${BASE_SHA:0:8} -> ${base_remote_now:0:8} during preparation; refusing stale-base push"
  echo "RESULT=base_moved"
  exit 8
fi
if git push --force-with-lease="${BRANCH}:${LEASE}" origin "HEAD:refs/heads/${BRANCH}" --quiet; then
  rm -f "$LEASE_FILE" "$BASE_FILE"
  log "pushed ${NEW_HEAD:0:8} (lease ${LEASE:0:8}); a re-review follows — run watch-pr.sh"
  echo "RESULT=pushed NEW_HEAD=$NEW_HEAD OLD_HEAD=$HEAD0 WORKTREE=$WT"
  exit 0
fi
echo "RESULT=push_failed"
exit 3
