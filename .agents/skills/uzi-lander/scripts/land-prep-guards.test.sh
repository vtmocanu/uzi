#!/usr/bin/env bash
# Hermetic regressions for land-prep's pre-push guards:
#   1. a migration-number collision is detected even when the base lists many migrations
#      (the old `ls-tree | xargs basename | grep -q` pipeline SIGPIPEd under pipefail and
#      skipped the renumber);
#   2. a branch that deletes CHANGELOG.md lines the base carries stops with exit 9;
#   3. a gate on a worktree without node_modules installs them with --ignore-scripts first.
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
git -C "$SEED" push -q origin collide clrm deps
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
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then echo '[]'; exit 0; fi
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

# 1. collision detected: the renumber helper is invoked (it refuses here, so exit 6).
run collide 101 --no-push --gate none
[ "$rc" -eq 6 ] || fail "collision not acted on, rc=$rc: $(cat "$WORK/out.101")"
grep -q '^migration:renumber$' "$TASK_LOG" || fail "migration:renumber was never invoked"
grep -q 'terminated with signal' "$WORK/out.101" && fail "a SIGPIPE surfaced in the collision check"

# 2. a deleted base CHANGELOG line stops before gates and push, with exit 9.
: > "$TASK_LOG"
run clrm 102
[ "$rc" -eq 9 ] || fail "CHANGELOG removal not stopped, rc=$rc: $(cat "$WORK/out.102")"
grep -q '^RESULT=changelog_removal ' "$WORK/out.102" || fail "CHANGELOG removal not named"
grep -q 'base entry two' "$WORK/out.102" || fail "the removed line was not printed"
[ ! -s "$TASK_LOG" ] || fail "gates ran despite the CHANGELOG removal: $(cat "$TASK_LOG")"
[ "$(git --git-dir="$ORIGIN" rev-parse refs/heads/clrm)" = "$(git -C "$SEED" rev-parse clrm)" ] || fail "clrm was pushed"
# ...and the explicit override lets it through to preparation.
run clrm 102 --skip-rebase --allow-changelog-removals --no-push --gate none
[ "$rc" -eq 0 ] || fail "--allow-changelog-removals did not pass, rc=$rc: $(cat "$WORK/out.102")"

# 3a. A failing install stops as a gate failure whose LOG is a real file carrying npm's output.
: > "$TASK_LOG"
NPM_FAIL=1 run deps 104 --no-push
[ "$rc" -eq 7 ] || fail "npm ci failure not exit 7, rc=$rc: $(cat "$WORK/out.104")"
npmlog=$(sed -n 's/^RESULT=gate_failed GATE=gate:agent LOG=\([^ ]*\) .*/\1/p' "$WORK/out.104")
[ -n "$npmlog" ] && [ -f "$npmlog" ] || fail "LOG is not a file: $(cat "$WORK/out.104")"
grep -q 'simulated install failure' "$npmlog" || fail "the npm log lacks npm's output"
grep -q 'simulated install failure' "$WORK/out.104" || fail "no failure tail was printed"
grep -q '^gate:agent$' "$TASK_LOG" && fail "gate:agent ran after a failed install"
rm -f "$npmlog"

# 3. gate:agent on a worktree with no node_modules installs them first, with --ignore-scripts.
: > "$TASK_LOG"
run deps 103 --no-push
[ "$rc" -eq 0 ] || fail "deps branch failed, rc=$rc: $(cat "$WORK/out.103")"
grep -q -- 'ci --ignore-scripts' "$NPM_LOG" || fail "npm ci --ignore-scripts was not run: $(cat "$NPM_LOG")"
grep -q '^gate:agent$' "$TASK_LOG" || fail "gate:agent did not run"
# An added-only CHANGELOG line is not a removal.
grep -q 'changelog_removal' "$WORK/out.103" && fail "an added CHANGELOG line was flagged as a removal"

echo "PASS land-prep guards: migration collision under pipefail, CHANGELOG removal stop, node_modules install"
