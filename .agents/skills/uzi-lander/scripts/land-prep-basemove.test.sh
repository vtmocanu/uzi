#!/usr/bin/env bash
# Hermetic regressions for land-prep's base-move tolerance and CHANGELOG auto-resolution:
#   A. the base moves during the gate with a delta disjoint from the branch: rebased and
#      pushed without re-running the gate;
#   B. the delta touches a branch path (even one that would rebase cleanly): exit 8, no push;
#   C. the delta is disjoint but the rebase conflicts (file/directory): exit 8, worktree
#      restored to its pre-rebase head;
#   D. a --skip-rebase re-entry after a disjoint base move rebases and continues;
#   E. a rebase stopping on CHANGELOG.md alone is resolved as a union and continued;
#   F. a stop on CHANGELOG.md plus another path is still exit 5;
#   G. the CHANGELOG-removal guard (exit 9) still runs after an auto-resolution.
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
mkdir -p "$SEED/web"
printf 'base\n' > "$SEED/base.txt"
printf 'l1\nl2\nl3\nl4\nl5\n' > "$SEED/web/shared.txt"
printf '## [Unreleased]\n\n### Fixed\n\n- **one**\n- **two**\n- **three**\n- **four**\n\n## [0.1.0] - 2026-01-01\n\n- **old**\n' > "$SEED/CHANGELOG.md"
git -C "$SEED" add -A
git -C "$SEED" commit -qm base
git -C "$SEED" remote add origin "$ORIGIN"
git -C "$SEED" push -q -u origin main
git --git-dir="$ORIGIN" symbolic-ref HEAD refs/heads/main
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
# task: records every call; a gate runs $GATE_HOOK once (then deletes it), which is how a
# base move lands "during" the gate.
cat > "$WORK/bin/task" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$TASK_LOG"
case "${1:-}" in
  gate:*) if [ -f "$GATE_HOOK" ]; then bash "$GATE_HOOK" > /dev/null 2>&1; rm -f "$GATE_HOOK"; fi;;
  *) echo "unexpected task: $*" >&2; exit 1;;
esac
STUB
chmod +x "$WORK/bin/"*
export TASK_LOG="$WORK/task.log" GATE_HOOK="$WORK/gate-hook.sh" SEED
: > "$TASK_LOG"

# mk_branch NAME: branch off the current main in the seed (the caller commits and pushes).
mk_branch() { git -C "$SEED" switch -q main; git -C "$SEED" switch -qc "$1"; }
commit_push() { git -C "$SEED" add -A; git -C "$SEED" commit -qm "$2"; git -C "$SEED" push -q origin "$1"; }

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
# hook FILE CONTENT MSG: on the next gate, main gains FILE=CONTENT (mkdir -p'd) as commit MSG.
hook() {
  cat > "$GATE_HOOK" <<EOF
set -e
git -C "\$SEED" switch -q main
mkdir -p "\$(dirname "\$SEED/$1")"
printf '$2' > "\$SEED/$1"
git -C "\$SEED" add -A
git -C "\$SEED" commit -qm '$3'
git -C "\$SEED" push -q origin main
EOF
}
origin_main() { git --git-dir="$ORIGIN" rev-parse refs/heads/main; }

# A. disjoint move during the gate: rebased, pushed, the gate NOT re-run.
mk_branch ba; printf 'a\n' > "$SEED/web/a.txt"; commit_push ba 'branch a'
hook prds/1650-new.md 'prd\n' 'unrelated prd'
: > "$TASK_LOG"
run ba 201
[ "$rc" -eq 0 ] || fail "A: disjoint base move not tolerated, rc=$rc: $(cat "$WORK/out.201")"
grep -q '^RESULT=pushed ' "$WORK/out.201" || fail "A: not pushed: $(cat "$WORK/out.201")"
grep -q 'delta disjoint from the branch (1 files); rebased without re-gating: CI on the pushed head is the authoritative gate' "$WORK/out.201" \
  || fail "A: the tolerance was not logged: $(cat "$WORK/out.201")"
[ "$(grep -c '^gate:web$' "$TASK_LOG")" -eq 1 ] || fail "A: gate ran $(grep -c '^gate:web$' "$TASK_LOG") times, want 1"
git --git-dir="$ORIGIN" merge-base --is-ancestor "$(origin_main)" refs/heads/ba || fail "A: the pushed head is not on the moved base"

# B. the delta touches web/shared.txt, which the branch also edits (a clean rebase): exit 8.
mk_branch bb; printf 'l1-branch\nl2\nl3\nl4\nl5\n' > "$SEED/web/shared.txt"; commit_push bb 'branch b'
BB_HEAD=$(git --git-dir="$ORIGIN" rev-parse refs/heads/bb)
hook web/shared.txt 'l1\nl2\nl3\nl4\nl5-main\n' 'main edits shared'
run bb 202
[ "$rc" -eq 8 ] || fail "B: overlapping base move returned rc=$rc, want 8: $(cat "$WORK/out.202")"
grep -q '^RESULT=base_moved$' "$WORK/out.202" || fail "B: base_moved not named"
grep -q 'during preparation; refusing stale-base push' "$WORK/out.202" || fail "B: the refusal message changed"
grep -q "touches the branch's own path(s): web/shared.txt" "$WORK/out.202" || fail "B: the overlap was not the reason: $(cat "$WORK/out.202")"
[ "$(git --git-dir="$ORIGIN" rev-parse refs/heads/bb)" = "$BB_HEAD" ] || fail "B: the branch was pushed"

# C. disjoint paths (web/dfx vs web/dfx/y.md) whose rebase conflicts: exit 8, worktree restored.
mk_branch bc; printf 'file\n' > "$SEED/web/dfx"; commit_push bc 'branch c'
BC_HEAD=$(git --git-dir="$ORIGIN" rev-parse refs/heads/bc)
hook web/dfx/y.md 'dir\n' 'main adds a directory'
run bc 203
[ "$rc" -eq 8 ] || fail "C: conflicting rebase returned rc=$rc, want 8: $(cat "$WORK/out.203")"
grep -q '^RESULT=base_moved$' "$WORK/out.203" || fail "C: base_moved not named"
grep -q 'conflicts; aborted, worktree back at' "$WORK/out.203" || fail "C: the rebase conflict was not the reason: $(cat "$WORK/out.203")"
C_WT="$WORK/wt-203"
[ -z "$(git -C "$C_WT" status --porcelain)" ] || fail "C: worktree left dirty: $(git -C "$C_WT" status --short)"
[ ! -d "$(git -C "$C_WT" rev-parse --git-path rebase-merge)" ] || fail "C: rebase left in progress"
[ "$(git --git-dir="$ORIGIN" rev-parse refs/heads/bc)" = "$BC_HEAD" ] || fail "C: the branch was pushed"
# HEAD is still the prepared head on the recorded base (one commit below the moved main).
[ "$(git -C "$C_WT" rev-parse HEAD^)" = "$(git -C "$C_WT" rev-parse "$(origin_main)^")" ] || fail "C: worktree HEAD is not the prepared head"

# D. --skip-rebase re-entry after a disjoint move: rebased, gates run, prepared.
mk_branch bd; printf 'd\n' > "$SEED/web/d.txt"; commit_push bd 'branch d'
run bd 204 --no-push
[ "$rc" -eq 0 ] || fail "D: initial preparation failed, rc=$rc: $(cat "$WORK/out.204")"
git -C "$SEED" switch -q main; mkdir -p "$SEED/prds"; printf 'x\n' > "$SEED/prds/other.md"; git -C "$SEED" add -A; git -C "$SEED" commit -qm 'other prd'; git -C "$SEED" push -q origin main
: > "$TASK_LOG"
run bd 204 --skip-rebase --no-push
[ "$rc" -eq 0 ] || fail "D: disjoint move at re-entry not tolerated, rc=$rc: $(cat "$WORK/out.204")"
grep -q '^RESULT=prepared ' "$WORK/out.204" || fail "D: not prepared"
grep -q '^gate:web$' "$TASK_LOG" || fail "D: gates did not run after the re-entry rebase"
git -C "$WORK/wt-204" merge-base --is-ancestor "$(origin_main)" HEAD || fail "D: not rebased onto the moved base"
[ "$(cat "$(git -C "$WORK/wt-204" rev-parse --git-path uzi-lander-base-204)")" = "$(origin_main)" ] || fail "D: recorded base not updated"

# E. CHANGELOG-only conflict: auto-resolved as a union, rebase continued.
mk_branch be
cl_append() { awk -v add="$2" '{print} $0=="- **four**"{print add}' "$1" > "$1.tmp" && mv "$1.tmp" "$1"; }
cl_append "$SEED/CHANGELOG.md" '- **branch e**'; printf 'e\n' > "$SEED/base-e.txt"; commit_push be 'branch e'
git -C "$SEED" switch -q main; cl_append "$SEED/CHANGELOG.md" '- **main e**'; git -C "$SEED" commit -qam 'main e'; git -C "$SEED" push -q origin main
run be 205 --no-push --gate none
[ "$rc" -eq 0 ] || fail "E: CHANGELOG-only conflict not auto-resolved, rc=$rc: $(cat "$WORK/out.205")"
grep -q 'auto-resolved the CHANGELOG.md conflict' "$WORK/out.205" || fail "E: auto-resolution not logged"
grep -qF -- '- **main e**' "$WORK/wt-205/CHANGELOG.md" && grep -qF -- '- **branch e**' "$WORK/wt-205/CHANGELOG.md" || fail "E: a side was lost"
grep -q '^<<<<<<<' "$WORK/wt-205/CHANGELOG.md" && fail "E: markers left"
[ -z "$(git -C "$WORK/wt-205" status --porcelain)" ] || fail "E: worktree dirty after the auto-resolution"

# F. CHANGELOG plus another conflicted path: exit 5, left mid-rebase.
mk_branch bf
cl_append "$SEED/CHANGELOG.md" '- **branch f**'; printf 'branch\n' > "$SEED/base.txt"; commit_push bf 'branch f'
git -C "$SEED" switch -q main; cl_append "$SEED/CHANGELOG.md" '- **main f**'; printf 'main\n' > "$SEED/base.txt"; git -C "$SEED" commit -qam 'main f'; git -C "$SEED" push -q origin main
run bf 206 --no-push --gate none
[ "$rc" -eq 5 ] || fail "F: a mixed conflict returned rc=$rc, want 5: $(cat "$WORK/out.206")"
grep -q 'auto-resolved' "$WORK/out.206" && fail "F: a mixed conflict was auto-resolved"
[ -d "$(git -C "$WORK/wt-206" rev-parse --git-path rebase-merge)" ] || fail "F: worktree not left mid-rebase"

# G. auto-resolution, then the branch's removal of a base line still stops with exit 9.
mk_branch bg
grep -vF -- '- **one**' "$SEED/CHANGELOG.md" > "$SEED/CHANGELOG.md.tmp"; mv "$SEED/CHANGELOG.md.tmp" "$SEED/CHANGELOG.md"
cl_append "$SEED/CHANGELOG.md" '- **branch g**'; commit_push bg 'branch g'
git -C "$SEED" switch -q main; cl_append "$SEED/CHANGELOG.md" '- **main g**'; git -C "$SEED" commit -qam 'main g'; git -C "$SEED" push -q origin main
run bg 207 --no-push --gate none
[ "$rc" -eq 9 ] || fail "G: removal after an auto-resolution returned rc=$rc, want 9: $(cat "$WORK/out.207")"
grep -q 'auto-resolved the CHANGELOG.md conflict' "$WORK/out.207" || fail "G: the conflict was not auto-resolved first"
grep -qF -- '-- **one**' "$WORK/out.207" || fail "G: the removed line was not printed"

echo "PASS land-prep base move: disjoint tolerated without re-gate, overlap/conflict refused, CHANGELOG union auto-resolve"
