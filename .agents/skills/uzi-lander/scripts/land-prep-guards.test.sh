#!/usr/bin/env bash
# Hermetic regressions for land-prep's pre-push guards:
#   1. a migration-number collision is detected even when the base lists many migrations
#      (the old `ls-tree | xargs basename | grep -q` pipeline SIGPIPEd under pipefail and
#      skipped the renumber);
#   2. a branch that deletes CHANGELOG.md lines the base carries stops with exit 9;
#   3. a gate on a worktree without node_modules installs them with --ignore-scripts first;
#   4. a reused worktree reinstalls when the recorded package-lock.json hash is absent or stale;
#   5. a workflow edit on a uzi-owned branch stops before the push (exit 10) unless overridden;
#   6. a checkout of the PR branch that land-prep did not create is refused (exit 3), at
#      the default path too, unless --worktree names it.
#   7. an active ci_fix on this repo/branch stops before worktree creation or push (exit 4);
#      terminal runs and active runs on another branch or repo do not block.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/land-prep.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

ORIGIN="$WORK/origin.git"
SEED="$WORK/seed"
ROOT="$WORK/root"
mkdir -p "$WORK/bin"
git init -q --bare "$ORIGIN"
git init -q -b main "$SEED"
git -C "$SEED" config user.name test
git -C "$SEED" config user.email test@example.com
MIG="$SEED/api/internal/store/migrations"
mkdir -p "$MIG" "$SEED/agent"
# Many base migrations, with the colliding number EARLY in the listing, so a reader that
# stops at the first match leaves the rest of the list unread (the SIGPIPE shape).
for i in $(seq -w 1 400); do printf -- '-- %s\n' "$i" > "$MIG/00${i}_m.sql"; done
printf '## [Unreleased]\n\n- base entry one\n- base entry two\n' > "$SEED/CHANGELOG.md"
printf '{"lockfileVersion":3}\n' > "$SEED/agent/package-lock.json"
printf 'node_modules/\n' > "$SEED/.gitignore" # as in the repo: an install leaves the tree clean
git -C "$SEED" add -A
git -C "$SEED" commit -qm base
git -C "$SEED" remote add origin "$ORIGIN"
git -C "$SEED" push -q -u origin main
git --git-dir="$ORIGIN" symbolic-ref HEAD refs/heads/main

mk_branch() { # name, then a function body applied on top of main
  git -C "$SEED" switch -q main
  git -C "$SEED" switch -qc "$1"
}
# collide: adds 00005_new.sql (00005 exists on main)
mk_branch collide
printf -- '-- new\n' > "$MIG/00005_new.sql"
git -C "$SEED" add -A && git -C "$SEED" commit -qm collide
# clrm: deletes a base CHANGELOG line
mk_branch clrm
printf '## [Unreleased]\n\n- base entry one\n- branch entry\n' > "$SEED/CHANGELOG.md"
git -C "$SEED" add -A && git -C "$SEED" commit -qm clrm
# deps: touches agent/ (gate:agent) and adds a CHANGELOG line only
mk_branch deps
printf 'x\n' > "$SEED/agent/x.ts"
printf '## [Unreleased]\n\n- base entry one\n- base entry two\n- deps entry\n' > "$SEED/CHANGELOG.md"
git -C "$SEED" add -A && git -C "$SEED" commit -qm deps
# agent/issue-9: a uzi-owned branch carrying a workflow edit; lander/wf: the same edit on a
# branch uzi never pushes to.
for b in agent/issue-9 lander/wf; do
  mk_branch "$b"
  mkdir -p "$SEED/.github/workflows"; printf 'on: push\n' > "$SEED/.github/workflows/ci.yml"
  git -C "$SEED" add -A && git -C "$SEED" commit -qm "wf on $b"
done
mk_branch cifix
printf 'guard\n' > "$SEED/guard.txt"
git -C "$SEED" add guard.txt && git -C "$SEED" commit -qm cifix
git -C "$SEED" push -q origin collide clrm deps agent/issue-9 lander/wf cifix
git clone -q "$ORIGIN" "$ROOT"
git -C "$ROOT" config user.name test
git -C "$ROOT" config user.email test@example.com

cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then printf '%s\n' "$PR_JSON"; exit 0; fi
echo "unexpected gh call: $*" >&2; exit 1
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then printf '%s\n' "$UZI_REPOS_JSON"; exit 0; fi
if [ "${1:-}" = run ] && [ "${2:-}" = list ]; then printf '%s\n' "$UZI_RUNS_JSON"; exit 0; fi
echo "unexpected uzi call: $*" >&2; exit 1
STUB
# task: records every call; migration:renumber refuses (so a detected collision is exit 6
# without touching the tree); gate:agent requires agent/node_modules like deps-check does.
cat > "$WORK/bin/task" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$TASK_LOG"
case "${1:-}" in
  migration:renumber) echo "simulated refusal" >&2; exit 1;;
  gate:agent) [ -d agent/node_modules ] || { echo "agent/node_modules does not exist" >&2; exit 2; };;
  gate:*) ;;
  *) echo "unexpected task: $*" >&2; exit 1;;
esac
STUB
cat > "$WORK/bin/npm" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$NPM_LOG"
[ -z "${NPM_FAIL:-}" ] || { echo "npm ERR! simulated install failure"; exit 1; }
[ "${1:-}" = ci ] || { echo "unexpected npm: $*" >&2; exit 1; }
case " $* " in *" --ignore-scripts "*) ;; *) echo "npm ci without --ignore-scripts" >&2; exit 1;; esac
mkdir -p node_modules
STUB
chmod +x "$WORK/bin/"*
export TASK_LOG="$WORK/task.log" NPM_LOG="$WORK/npm.log"
export UZI_REPOS_JSON='[]' UZI_RUNS_JSON='[]'
: > "$TASK_LOG"; : > "$NPM_LOG"

run() { # branch pr [extra args...] -> sets rc, output in $WORK/out.<pr>
  local br="$1" pr="$2"; shift 2
  PR_JSON=$(jq -cn --arg b "$br" --arg h "$(git --git-dir="$ORIGIN" rev-parse "refs/heads/$br")" \
    '{state:"OPEN",headRefName:$b,baseRefName:"main",headRefOid:$h,headRepository:{name:"uzi"},headRepositoryOwner:{login:"test"}}')
  export PR_JSON
  set +e
  PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi "$pr" --repo-root "$ROOT" --worktree "$WORK/wt-$pr" "$@" > "$WORK/out.$pr" 2>&1
  rc=$?
  set -e
}

# 7. Record actual push invocations: an unchanged remote SHA alone cannot prove no push.
REAL_GIT=$(command -v git)
export PUSH_LOG="$WORK/push.log"
: > "$PUSH_LOG"
cat > "$WORK/bin/git" <<STUB
#!/usr/bin/env bash
if [ "\$1" = push ]; then printf '%s\n' "\$*" >> "\$PUSH_LOG"; fi
exec "$REAL_GIT" "\$@"
STUB
chmod +x "$WORK/bin/git"
export UZI_REPOS_JSON='[{"id":"repo-1","path_with_namespace":"test/uzi"}]'
export UZI_RUNS_JSON='[{"id":"active-cifix","kind":"ci_fix","repo_id":"repo-1","pipeline_ref":"cifix","status":"running"}]'
run cifix 110 --gate none
[ "$rc" -eq 4 ] || fail "active ci_fix not stopped, rc=$rc: $(cat "$WORK/out.110")"
grep -Fq 'active-cifix' "$WORK/out.110" || fail "ci_fix refusal omitted the run id"
[ ! -s "$PUSH_LOG" ] || fail "push ran despite active ci_fix"
[ ! -e "$WORK/wt-110" ] || fail "worktree created despite active ci_fix"
[ "$(git -C "$ROOT" worktree list --porcelain | grep -c '^worktree ')" -eq 1 ] || fail "active ci_fix created a worktree elsewhere"
# Every terminal status passes, as do active runs on another branch or another repo.
for status in completed failed cancelled; do
  UZI_RUNS_JSON=$(jq -cn --arg s "$status" '[{id:"terminal-cifix",kind:"ci_fix",repo_id:"repo-1",pipeline_ref:"cifix",status:$s}]')
  export UZI_RUNS_JSON
  run cifix 110 --gate none --no-push
  [ "$rc" -eq 0 ] || fail "terminal ci_fix ($status) blocked preparation, rc=$rc: $(cat "$WORK/out.110")"
done
export UZI_RUNS_JSON='[{"id":"other-branch","kind":"ci_fix","repo_id":"repo-1","pipeline_ref":"other","status":"running"},{"id":"other-repo","kind":"ci_fix","repo_id":"repo-2","pipeline_ref":"cifix","status":"running"}]'
run cifix 110 --gate none --no-push
[ "$rc" -eq 0 ] || fail "unrelated active ci_fix blocked preparation, rc=$rc: $(cat "$WORK/out.110")"
export UZI_REPOS_JSON='[]' UZI_RUNS_JSON='[]'
rm -f "$WORK/bin/git"

# 1. collision detected: the renumber helper is invoked (it refuses here, so exit 6).
run collide 101 --no-push --gate none
[ "$rc" -eq 6 ] || fail "collision not acted on, rc=$rc: $(cat "$WORK/out.101")"
grep -q '^migration:renumber$' "$TASK_LOG" || fail "migration:renumber was never invoked"
grep -q 'terminated with signal' "$WORK/out.101" && fail "a SIGPIPE surfaced in the collision check"

# 2. a deleted base CHANGELOG line stops before gates and push, with exit 9.
: > "$TASK_LOG"
run clrm 102 --gate auto
[ "$rc" -eq 9 ] || fail "CHANGELOG removal not stopped, rc=$rc: $(cat "$WORK/out.102")"
grep -q '^RESULT=changelog_removal ' "$WORK/out.102" || fail "CHANGELOG removal not named"
grep -q 'base entry two' "$WORK/out.102" || fail "the removed line was not printed"
[ ! -s "$TASK_LOG" ] || fail "gates ran despite the CHANGELOG removal: $(cat "$TASK_LOG")"
[ "$(git --git-dir="$ORIGIN" rev-parse refs/heads/clrm)" = "$(git -C "$SEED" rev-parse clrm)" ] || fail "clrm was pushed"
# ...and the explicit override lets it through to preparation.
run clrm 102 --skip-rebase --allow-changelog-removals --no-push --gate none
[ "$rc" -eq 0 ] || fail "--allow-changelog-removals did not pass, rc=$rc: $(cat "$WORK/out.102")"

# 5. a workflow edit on a uzi-owned branch stops before the push (exit 10): uzi's worker PAT
# lacks workflow scope, so every later uzi push there would fail. Not pushed.
before=$(git --git-dir="$ORIGIN" rev-parse refs/heads/agent/issue-9)
run agent/issue-9 105 --gate none
[ "$rc" -eq 10 ] || fail "workflow edit on a uzi branch not stopped, rc=$rc: $(cat "$WORK/out.105")"
grep -q '^RESULT=workflow_edit ' "$WORK/out.105" || fail "workflow stop not named: $(cat "$WORK/out.105")"
grep -q 'workflow_scope_missing' "$WORK/out.105" || fail "the consequence was not named"
# (the rebase onto main is a no-op here, so a push would not have moved the ref either; the
# rc and RESULT are the evidence)
[ "$(git --git-dir="$ORIGIN" rev-parse refs/heads/agent/issue-9)" = "$before" ] || fail "agent/issue-9 was pushed"
# ...the explicit override pushes, and a non-uzi branch is not stopped at all.
run agent/issue-9 105 --skip-rebase --gate none --allow-workflow-edit
[ "$rc" -eq 0 ] || fail "--allow-workflow-edit did not pass, rc=$rc: $(cat "$WORK/out.105")"
# ...and an unreadable workflow diff fails closed (exit 3) instead of reading as "no edit".
REAL_GIT=$(command -v git)
cat > "$WORK/bin/git" <<STUB
#!/usr/bin/env bash
if [ -n "\${WF_DIFF_FAIL:-}" ] && [ "\$1" = diff ] && [ "\${*: -1}" = .github/workflows/ ]; then exit 128; fi
exec "$REAL_GIT" "\$@"
STUB
chmod +x "$WORK/bin/git"
WF_DIFF_FAIL=1 run agent/issue-9 107 --gate none --allow-workflow-edit
rm -f "$WORK/bin/git"
[ "$rc" -eq 3 ] || fail "an unreadable workflow diff did not fail closed, rc=$rc: $(cat "$WORK/out.107")"
run lander/wf 106 --gate none
[ "$rc" -eq 0 ] || fail "a non-uzi branch with a workflow edit was stopped, rc=$rc: $(cat "$WORK/out.106")"
grep -q 'NOTE: branch changes workflow files' "$WORK/out.106" || fail "the workflow note is missing on a non-uzi branch"

# 3a. A failing install stops as a gate failure whose LOG is a real file carrying npm's output.
: > "$TASK_LOG"
NPM_FAIL=1 run deps 104 --no-push --gate auto
[ "$rc" -eq 7 ] || fail "npm ci failure not exit 7, rc=$rc: $(cat "$WORK/out.104")"
npmlog=$(sed -n 's/^RESULT=gate_failed GATE=gate:agent LOG=\([^ ]*\) .*/\1/p' "$WORK/out.104")
[ -n "$npmlog" ] && [ -f "$npmlog" ] || fail "LOG is not a file: $(cat "$WORK/out.104")"
grep -q 'simulated install failure' "$npmlog" || fail "the npm log lacks npm's output"
grep -q 'simulated install failure' "$WORK/out.104" || fail "no failure tail was printed"
grep -q '^gate:agent$' "$TASK_LOG" && fail "gate:agent ran after a failed install"
rm -f "$npmlog"

# 3. gate:agent on a worktree with no node_modules installs them first, with --ignore-scripts.
: > "$TASK_LOG"
run deps 103 --no-push --gate auto
[ "$rc" -eq 0 ] || fail "deps branch failed, rc=$rc: $(cat "$WORK/out.103")"
grep -q -- 'ci --ignore-scripts' "$NPM_LOG" || fail "npm ci --ignore-scripts was not run: $(cat "$NPM_LOG")"
grep -q '^gate:agent$' "$TASK_LOG" || fail "gate:agent did not run"
# An added-only CHANGELOG line is not a removal.
grep -q 'changelog_removal' "$WORK/out.103" && fail "an added CHANGELOG line was flagged as a removal"
# The deps branch is already checked out by 3a's worktree, which land-prep reuses.
DEPS_WT=$(sed -n 's/^RESULT=prepared .*WORKTREE=\([^ ]*\).*/\1/p' "$WORK/out.103")
[ -d "$DEPS_WT" ] || fail "no worktree named in: $(cat "$WORK/out.103")"
STAMP="$DEPS_WT/agent/node_modules/.uzi-lander-lock.sha256"
[ -s "$STAMP" ] || fail "the install recorded no lockfile hash"

# 4. A reused worktree: node_modules whose recorded hash matches the lockfile is kept...
: > "$NPM_LOG"
run deps 103 --skip-rebase --no-push --gate auto
[ "$rc" -eq 0 ] || fail "matching-hash re-run failed, rc=$rc: $(cat "$WORK/out.103")"
[ ! -s "$NPM_LOG" ] || fail "npm ci ran although the lockfile hash matched: $(cat "$NPM_LOG")"
# ...a lockfile changed since the install (a base move that bumped it) reinstalls...
printf '{"lockfileVersion":3,"bumped":true}\n' > "$DEPS_WT/agent/package-lock.json"
git -C "$DEPS_WT" commit -qam 'bump lockfile'
run deps 103 --skip-rebase --no-push --gate auto
[ "$rc" -eq 0 ] || fail "changed-lockfile re-run failed, rc=$rc: $(cat "$WORK/out.103")"
grep -q -- 'ci --ignore-scripts' "$NPM_LOG" || fail "a changed lockfile did not reinstall"
grep -q 'package-lock.json changed' "$WORK/out.103" || fail "the reinstall reason was not logged"
[ "$(cat "$STAMP")" = "$(shasum -a 256 "$DEPS_WT/agent/package-lock.json" | cut -d' ' -f1)" ] || fail "the new lockfile hash was not recorded"
# ...and an install with no recorded hash reinstalls once.
rm -f "$STAMP"
: > "$NPM_LOG"
run deps 103 --skip-rebase --no-push --gate auto
[ "$rc" -eq 0 ] || fail "missing-hash re-run failed, rc=$rc: $(cat "$WORK/out.103")"
grep -q -- 'ci --ignore-scripts' "$NPM_LOG" || fail "node_modules without a recorded hash was not reinstalled"
[ -s "$STAMP" ] || fail "the reinstall recorded no hash"

# 6. A checkout of the PR branch that land-prep did not create (another session's tree) is
# refused, not rebased in place; naming it with --worktree is the explicit opt-in.
git -C "$SEED" switch -q main
git -C "$SEED" switch -qc foreign
printf 'f\n' > "$SEED/f.txt"
git -C "$SEED" add -A && git -C "$SEED" commit -qm foreign
git -C "$SEED" push -q origin foreign
git -C "$ROOT" fetch -q origin foreign
FOREIGN="$WORK/someone-elses-tree"
git -C "$ROOT" worktree add -q "$FOREIGN" -b foreign origin/foreign
before=$(git -C "$FOREIGN" rev-parse HEAD)
run foreign 108 --gate none --no-push
[ "$rc" -eq 3 ] || fail "a foreign checkout was not refused, rc=$rc: $(cat "$WORK/out.108")"
grep -q 'land-prep did not create' "$WORK/out.108" || fail "the refusal does not name the cause: $(cat "$WORK/out.108")"
[ "$(git -C "$FOREIGN" rev-parse HEAD)" = "$before" ] || fail "the foreign tree's HEAD moved"
[ ! -e "$WORK/wt-108" ] || fail "a second worktree was created"
# ...and the explicit opt-in reuses it.
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 108 --repo-root "$ROOT" --worktree "$FOREIGN" --gate none --no-push > "$WORK/out.108b" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--worktree naming the tree did not reuse it, rc=$rc: $(cat "$WORK/out.108b")"
# ...and an unmarked checkout sitting at the DEFAULT path (no --worktree) is refused too.
git -C "$SEED" switch -q main
git -C "$SEED" switch -qc foreign2
printf 'g\n' > "$SEED/g.txt"
git -C "$SEED" add -A && git -C "$SEED" commit -qm foreign2
git -C "$SEED" push -q origin foreign2
git -C "$ROOT" fetch -q origin foreign2
DEFAULT_WT="${ROOT}-land-109"
git -C "$ROOT" worktree add -q "$DEFAULT_WT" -b foreign2 origin/foreign2
before=$(git -C "$DEFAULT_WT" rev-parse HEAD)
PR_JSON=$(jq -cn --arg h "$(git --git-dir="$ORIGIN" rev-parse refs/heads/foreign2)" \
  '{state:"OPEN",headRefName:"foreign2",baseRefName:"main",headRefOid:$h,headRepository:{name:"uzi"},headRepositoryOwner:{login:"test"}}')
export PR_JSON
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 109 --repo-root "$ROOT" --gate none --no-push > "$WORK/out.109" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "an unmarked checkout at the default path was not refused, rc=$rc: $(cat "$WORK/out.109")"
grep -q 'land-prep did not create' "$WORK/out.109" || fail "the default-path refusal does not name the cause: $(cat "$WORK/out.109")"
[ "$(git -C "$DEFAULT_WT" rev-parse HEAD)" = "$before" ] || fail "the default-path tree's HEAD moved"
git -C "$ROOT" worktree remove --force "$DEFAULT_WT"

echo "PASS land-prep guards: migration collision under pipefail, CHANGELOG removal stop, node_modules install, lockfile-hash reinstall, workflow edit on a uzi branch, foreign worktree refusal, branch-scoped active ci_fix refusal"
