#!/usr/bin/env bash
# Hermetic regressions for land-prep's merge mode (#1835): a branch that already holds merge
# commits (an uzi rework merges main in) is brought up to date by merging the base in, not by
# replaying every commit.
#   A. auto mode: the base is merged in (branch head stays first parent, nothing rewritten),
#      and the push is a plain fast-forward, never a force;
#   B. a CHANGELOG.md-only merge conflict is union-resolved, base bullets first;
#   C. a CHANGELOG.md conflict the union refuses stops with exit 5, git's own merge conflict
#      (index stages and file bytes) left as git wrote it;
#   D. any other conflict stops with exit 5 mid-merge on the branch; a clean but unconcluded
#      merge is refused; once committed, --skip-rebase continues;
#   E. the CHANGELOG-removal guard (exit 9) still runs after a merge;
#   F. a migration number collision after a merge is renumbered and committed;
#   G. --merge / --rebase force the mode, which is recorded for the landing: a conflicting
#      flag on re-entry is refused, no flag reuses it;
#   H. a remote that advances between the lease check and the push is rejected, not overwritten;
#   I. a base that moves during the landing is merged in again; when that merge conflicts the
#      worktree is restored (HEAD, clean index and tree), and a failed `git merge --abort` is
#      exit 3, named.
set -eu
export NET_RETRY_BASE_SLEEP=0
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/land-prep.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

REAL_GIT=$(command -v git)
ORIGIN="$WORK/origin.git"
SEED="$WORK/seed"
ROOT="$WORK/root"
MIG=api/internal/store/migrations
mkdir -p "$WORK/bin"
git init -q --bare "$ORIGIN"
git init -q -b main "$SEED"
git -C "$SEED" config user.name test
git -C "$SEED" config user.email test@example.com
mkdir -p "$SEED/web" "$SEED/$MIG"
printf 'l1\nl2\nl3\n' > "$SEED/web/shared.txt"
printf -- '-- base\n' > "$SEED/$MIG/00001_base.sql"
# cl EXTRA: the CHANGELOG with EXTRA (whole lines) appended to [Unreleased]'s ### Fixed list.
cl() { printf '## [Unreleased]\n\n### Fixed\n\n- **one**\n- **two**\n- **three**\n- **four**\n%s\n## [0.1.0] - 2026-01-01\n\n- **old**\n' "$1"; }
cl '' > "$SEED/CHANGELOG.md"
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
# task: records every call; migration:renumber moves the branch's colliding migration up one.
cat > "$WORK/bin/task" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$TASK_LOG"
case "${1:-}" in
  migration:renumber) git mv "$RENUMBER_FROM" "$RENUMBER_TO"; echo "renamed $RENUMBER_FROM -> $RENUMBER_TO";;
  gate:*) ;;
  *) echo "unexpected task: $*" >&2; exit 1;;
esac
STUB
# git: records every push; runs $PUSH_HOOK once just before a push (a remote advancing in the
# window after the lease check); fails `merge --abort` while $FAIL_MERGE_ABORT is set.
cat > "$WORK/bin/git" <<STUB
#!/usr/bin/env bash
case " \$* " in
  *" push "*)
    printf '%s\n' "\$*" >> "\$PUSH_LOG"
    if [ -f "\$PUSH_HOOK" ]; then bash "\$PUSH_HOOK" > /dev/null 2>&1; rm -f "\$PUSH_HOOK"; fi;;
  *" merge --abort "*)
    if [ -n "\${FAIL_MERGE_ABORT:-}" ]; then echo "fatal: simulated abort failure" >&2; exit 128; fi;;
esac
exec "$REAL_GIT" "\$@"
STUB
chmod +x "$WORK/bin/"*
export TASK_LOG="$WORK/task.log" PUSH_LOG="$WORK/push.log" PUSH_HOOK="$WORK/push-hook.sh" SEED REAL_GIT
export RENUMBER_FROM="" RENUMBER_TO=""
: > "$TASK_LOG"; : > "$PUSH_LOG"

sg() { "$REAL_GIT" -C "$SEED" "$@"; }
# main_commit PATH CONTENT MSG: main gains PATH=CONTENT (printf format), pushed.
main_commit() {
  sg switch -q main; mkdir -p "$(dirname "$SEED/$1")"; printf -- "$2" > "$SEED/$1"
  sg add -A; sg commit -qm "$3"; sg push -q origin main
}
# branch_commit NAME PATH CONTENT MSG: the branch gains PATH=CONTENT, pushed.
branch_commit() {
  sg switch -q "$1"; mkdir -p "$(dirname "$SEED/$2")"; printf -- "$3" > "$SEED/$2"
  sg add -A; sg commit -qm "$4"; sg push -q origin "$1"; sg switch -q main
}
# merge_branch NAME: an uzi-rework shape. NAME branches off main with its own commit, main
# moves on, and main is merged INTO NAME; pushed. The caller then moves main again.
merge_branch() {
  sg switch -q main; sg switch -qc "$1"
  printf '%s\n' "$1" > "$SEED/web/$1.txt"; sg add -A; sg commit -qm "branch $1"
  main_commit "docs/$1-before.md" 'before\n' "main before $1's rework"
  sg switch -q "$1"; sg merge -q --no-ff --no-edit main; sg push -q -u origin "$1"; sg switch -q main
}
linear_branch() {
  sg switch -q main; sg switch -qc "$1"
  printf '%s\n' "$1" > "$SEED/web/$1.txt"; sg add -A; sg commit -qm "branch $1"
  sg push -q -u origin "$1"; sg switch -q main
}
run() { # branch pr [extra args...] -> sets rc, output in $WORK/out.<pr>
  local br="$1" pr="$2"; shift 2
  PR_JSON=$(jq -cn --arg b "$br" --arg h "$(git --git-dir="$ORIGIN" rev-parse "refs/heads/$br")" \
    '{state:"OPEN",headRefName:$b,baseRefName:"main",headRefOid:$h,headRepository:{name:"uzi"},headRepositoryOwner:{login:"test"}}')
  export PR_JSON
  set +e
  PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi "$pr" --repo-root "$ROOT" --worktree "$WORK/wt-$pr" --gate none "$@" > "$WORK/out.$pr" 2>&1
  rc=$?
  set -e
}
origin_ref() { git --git-dir="$ORIGIN" rev-parse "refs/heads/$1"; }
out() { cat "$WORK/out.$1"; }

# A. auto mode merges the base in and pushes a fast-forward.
merge_branch ma
MA_OLD=$(origin_ref ma)
main_commit docs/a.md 'a\n' 'main moves after ma'
run ma 401 --no-push
[ "$rc" -eq 0 ] || fail "A: preparation failed, rc=$rc: $(out 401)"
grep -qx 'INTEGRATION=merge' "$WORK/out.401" || fail "A: a branch holding a merge commit was not merged: $(out 401)"
WT="$WORK/wt-401"
[ "$(git -C "$WT" rev-parse HEAD^1)" = "$MA_OLD" ] || fail "A: the branch head is not the merge's first parent"
[ "$(git -C "$WT" rev-parse HEAD^2)" = "$(origin_ref main)" ] || fail "A: origin/main is not the merge's second parent"
[ -f "$(git -C "$WT" rev-parse --git-path uzi-lander-mode-401)" ] || fail "A: the mode was not recorded"
: > "$PUSH_LOG"
run ma 401 --skip-rebase
[ "$rc" -eq 0 ] || fail "A: push failed, rc=$rc: $(out 401)"
grep -q '^RESULT=pushed ' "$WORK/out.401" || fail "A: not pushed: $(out 401)"
[ "$(origin_ref ma)" = "$(git -C "$WT" rev-parse HEAD)" ] || fail "A: the remote is not at the merged head"
git --git-dir="$ORIGIN" merge-base --is-ancestor "$MA_OLD" "$(origin_ref ma)" || fail "A: the push rewrote history"
grep -q 'push' "$PUSH_LOG" || fail "A: no push recorded"
grep -q 'force' "$PUSH_LOG" && fail "A: merge mode pushed with a force flag: $(cat "$PUSH_LOG")"
[ ! -f "$(git -C "$WT" rev-parse --git-path uzi-lander-mode-401)" ] || fail "A: the recorded mode outlived the landing"

# B. CHANGELOG.md-only conflict: union-resolved, base bullet before the branch's.
merge_branch mb
branch_commit mb CHANGELOG.md "$(cl '- **branch fix**
')\n" 'branch changelog'
main_commit CHANGELOG.md "$(cl '- **main fix**
')\n" 'main changelog'
run mb 402 --no-push
[ "$rc" -eq 0 ] || fail "B: CHANGELOG conflict not resolved, rc=$rc: $(out 402)"
grep -q 'auto-resolved the CHANGELOG.md merge conflict' "$WORK/out.402" || fail "B: no auto-resolution logged: $(out 402)"
WT="$WORK/wt-402"
grep -qE '^(<<<<<<<|=======|>>>>>>>|\|\|\|\|\|\|\|)' "$WT/CHANGELOG.md" && fail "B: markers survived"
m=$(grep -n -F -- '- **main fix**' "$WT/CHANGELOG.md" | cut -d: -f1); b=$(grep -n -F -- '- **branch fix**' "$WT/CHANGELOG.md" | cut -d: -f1)
[ -n "$m" ] && [ -n "$b" ] && [ "$m" -lt "$b" ] || fail "B: want main's bullet before the branch's: $(cat "$WT/CHANGELOG.md")"
[ "$(git -C "$WT" rev-parse HEAD^2)" = "$(origin_ref main)" ] || fail "B: the merge was not committed"

# C. a CHANGELOG.md conflict the union refuses (the branch rewords a base line next to main's
#    addition): exit 5, git's conflict untouched (stage 2 = the branch, git's own markers).
merge_branch mc
branch_commit mc CHANGELOG.md "$(cl '' | sed 's/- \*\*four\*\*/- **four, reworded**/')\n" 'branch rewords'
main_commit CHANGELOG.md "$(cl '- **main c**
')\n" 'main changelog c'
run mc 403 --no-push
[ "$rc" -eq 5 ] || fail "C: refused union returned rc=$rc, want 5: $(out 403)"
WT="$WORK/wt-403"
git -C "$WT" rev-parse -q --verify MERGE_HEAD >/dev/null || fail "C: the merge is not left in progress"
[ "$(git -C "$WT" rev-parse --abbrev-ref HEAD)" = mc ] || fail "C: not on the branch"
s2=$(git -C "$WT" ls-files -u -- CHANGELOG.md | awk '$3 == 2 { print $2 }')
s3=$(git -C "$WT" ls-files -u -- CHANGELOG.md | awk '$3 == 3 { print $2 }')
[ "$s2" = "$(git -C "$WT" rev-parse HEAD:CHANGELOG.md)" ] || fail "C: index stage 2 is no longer the branch"
[ "$s3" = "$(git -C "$WT" rev-parse "$(origin_ref main):CHANGELOG.md")" ] || fail "C: index stage 3 is no longer the base"
grep -q '^<<<<<<< HEAD' "$WT/CHANGELOG.md" || fail "C: git's own conflict text was not restored: $(cat "$WT/CHANGELOG.md")"
grep -q '^<<<<<<< origin/main' "$WT/CHANGELOG.md" && fail "C: the regenerated orientation leaked into the worktree"
git -C "$WT" merge --abort
# C2. ...and when putting git's conflict text back fails, the saved copy is kept and named,
#     exit 3 (never a quiet exit 5 over a CHANGELOG.md that is not git's).
GIT_CONFLICT=$(git -C "$WT" -c merge.conflictStyle=diff3 merge -q --no-ff --no-edit origin/main >/dev/null 2>&1; cat "$WT/CHANGELOG.md")
git -C "$WT" merge --abort
REAL_CP=$(command -v cp)
cat > "$WORK/bin/cp" <<STUB
#!/usr/bin/env bash
if [ "\${2:-}" = CHANGELOG.md ]; then echo "cp: simulated restore failure" >&2; exit 1; fi
exec "$REAL_CP" "\$@"
STUB
chmod +x "$WORK/bin/cp"
run mc 403 --no-push
rm -f "$WORK/bin/cp"
[ "$rc" -eq 3 ] || fail "C2: a failed restore returned rc=$rc, want 3: $(out 403)"
grep -q "ERROR: could not restore git's CHANGELOG.md conflict text" "$WORK/out.403" || fail "C2: the failed restore was not named: $(out 403)"
backup=$(sed -n 's/^RESULT=restore_failed BACKUP=\([^ ]*\) .*/\1/p' "$WORK/out.403")
[ -n "$backup" ] && [ -f "$backup" ] || fail "C2: the saved conflict text was not kept: $(out 403)"
[ "$(cat "$backup")" = "$GIT_CONFLICT" ] || fail "C2: the kept copy is not git's conflict text"
rm -rf "$(dirname "$backup")"
git -C "$WT" merge --abort

# D. a non-CHANGELOG conflict stops mid-merge on the branch; an unconcluded merge is refused
#    even with a clean tree; committing it lets --skip-rebase continue.
merge_branch md
branch_commit md web/shared.txt 'l1-branch\nl2\nl3\n' 'branch edits shared'
main_commit web/shared.txt 'l1-main\nl2\nl3\n' 'main edits shared'
run md 404 --no-push
[ "$rc" -eq 5 ] || fail "D: conflict returned rc=$rc, want 5: $(out 404)"
grep -q 'MERGE CONFLICT' "$WORK/out.404" || fail "D: the merge conflict was not named: $(out 404)"
grep -q '^RESULT=conflict ' "$WORK/out.404" || fail "D: no RESULT=conflict: $(out 404)"
WT="$WORK/wt-404"
[ "$(git -C "$WT" rev-parse --abbrev-ref HEAD)" = md ] || fail "D: not on the branch"
git -C "$WT" checkout -q --ours web/shared.txt
git -C "$WT" add web/shared.txt
[ -z "$(git -C "$WT" status --porcelain)" ] || fail "D: fixture: the resolution should leave a clean tree"
run md 404 --skip-rebase --no-push
[ "$rc" -eq 5 ] || fail "D: an unconcluded merge returned rc=$rc, want 5: $(out 404)"
grep -q 'merge is still in progress' "$WORK/out.404" || fail "D: the unconcluded merge was not named: $(out 404)"
GIT_EDITOR=true git -C "$WT" commit -q --no-edit
run md 404 --skip-rebase --no-push
[ "$rc" -eq 0 ] || fail "D: re-entry after the commit failed, rc=$rc: $(out 404)"
grep -q '^RESULT=prepared ' "$WORK/out.404" || fail "D: not prepared: $(out 404)"

# E. the CHANGELOG-removal guard still runs after a clean merge.
merge_branch me
branch_commit me CHANGELOG.md "$(cl '' | grep -v -F -- '- **two**')\n" 'branch drops a base line'
main_commit docs/e.md 'e\n' 'main moves after me'
run me 405 --no-push
[ "$rc" -eq 9 ] || fail "E: a deleted base line returned rc=$rc, want 9: $(out 405)"
grep -q '^RESULT=changelog_removal ' "$WORK/out.405" || fail "E: no RESULT=changelog_removal: $(out 405)"

# F. a migration number collision after the merge is renumbered and committed.
merge_branch mf
branch_commit mf "$MIG/00002_branch.sql" '-- branch\n' 'branch migration'
main_commit "$MIG/00002_main.sql" '-- main\n' 'main migration'
: > "$TASK_LOG"
RENUMBER_FROM="$MIG/00002_branch.sql" RENUMBER_TO="$MIG/00003_branch.sql" run mf 406 --no-push
[ "$rc" -eq 0 ] || fail "F: renumber after merge failed, rc=$rc: $(out 406)"
grep -qx 'migration:renumber' "$TASK_LOG" || fail "F: migration:renumber was not invoked"
WT="$WORK/wt-406"
[ -f "$WT/$MIG/00003_branch.sql" ] && [ ! -e "$WT/$MIG/00002_branch.sql" ] || fail "F: the migration was not renamed"
git -C "$WT" log -1 --format=%s | grep -q '^chore: renumber migrations' || fail "F: the renumber was not committed"
[ "$(git -C "$WT" rev-parse HEAD^^2)" = "$(origin_ref main)" ] || fail "F: the renumber does not sit on the merge"

# G. forced modes are recorded for the landing.
linear_branch lg
main_commit docs/g.md 'g\n' 'main moves after lg'
run lg 407 --merge --no-push
[ "$rc" -eq 0 ] || fail "G: --merge on a linear branch failed, rc=$rc: $(out 407)"
grep -qx 'INTEGRATION=merge' "$WORK/out.407" || fail "G: --merge was not honoured: $(out 407)"
[ "$(git -C "$WORK/wt-407" rev-parse HEAD^2)" = "$(origin_ref main)" ] || fail "G: no merge commit"
run lg 407 --rebase --skip-rebase --no-push
[ "$rc" -eq 3 ] || fail "G: a conflicting --rebase on re-entry returned rc=$rc, want 3: $(out 407)"
grep -q 'started in merge mode' "$WORK/out.407" || fail "G: the conflict was not named: $(out 407)"
run lg 407 --skip-rebase --no-push
[ "$rc" -eq 0 ] || fail "G: re-entry without a flag failed, rc=$rc: $(out 407)"
grep -q 'integration mode: merge (recorded for this landing)' "$WORK/out.407" || fail "G: the recorded mode was not reused: $(out 407)"
merge_branch mg
main_commit docs/g2.md 'g2\n' 'main moves after mg'
run mg 408 --rebase --no-push
[ "$rc" -eq 0 ] || fail "G: --rebase on a merge branch failed, rc=$rc: $(out 408)"
grep -qx 'INTEGRATION=rebase' "$WORK/out.408" || fail "G: --rebase was not honoured: $(out 408)"
[ -z "$(git -C "$WORK/wt-408" rev-list --merges "$(origin_ref main)..HEAD")" ] || fail "G: --rebase kept merge commits"
run mg 408 --merge --skip-rebase --no-push
[ "$rc" -eq 3 ] || fail "G: a conflicting --merge on re-entry returned rc=$rc, want 3: $(out 408)"

# H. the remote advances between the lease check and the push: the plain push is rejected and
#    the remote keeps the newer commit.
merge_branch mh
main_commit docs/h.md 'h\n' 'main moves after mh'
run mh 409 --no-push
[ "$rc" -eq 0 ] || fail "H: preparation failed, rc=$rc: $(out 409)"
cat > "$PUSH_HOOK" <<'EOF'
set -e
"$REAL_GIT" -C "$SEED" switch -q mh
printf 'racer\n' > "$SEED/web/racer.txt"
"$REAL_GIT" -C "$SEED" add -A
"$REAL_GIT" -C "$SEED" commit -qm 'a concurrent push'
"$REAL_GIT" -C "$SEED" push -q origin mh
"$REAL_GIT" -C "$SEED" switch -q main
EOF
run mh 409 --skip-rebase
[ "$rc" -eq 3 ] || fail "H: a concurrent advance returned rc=$rc, want 3: $(out 409)"
grep -q '^RESULT=push_failed$' "$WORK/out.409" || fail "H: no RESULT=push_failed: $(out 409)"
[ "$(git --git-dir="$ORIGIN" log -1 --format=%s refs/heads/mh)" = 'a concurrent push' ] || fail "H: the concurrent commit was overwritten"

# I. the base moves during the landing: merged again on re-entry; a conflicting move restores
#    the worktree; a failed abort is exit 3.
merge_branch mi
main_commit docs/i.md 'i\n' 'main moves after mi'
run mi 410 --no-push
[ "$rc" -eq 0 ] || fail "I: preparation failed, rc=$rc: $(out 410)"
main_commit docs/i2.md 'i2\n' 'main moves again (disjoint)'
run mi 410 --skip-rebase --no-push
[ "$rc" -eq 0 ] || fail "I: disjoint base move not tolerated, rc=$rc: $(out 410)"
grep -q 'merged; the gates below run on the new head' "$WORK/out.410" || fail "I: the move was not merged: $(out 410)"
[ "$(git -C "$WORK/wt-410" rev-parse HEAD^2)" = "$(origin_ref main)" ] || fail "I: the moved base is not merged in"
[ "$(git -C "$WORK/wt-410" rev-list --merges "$(origin_ref mi)..HEAD" | wc -l | tr -d ' ')" -eq 2 ] || fail "I: want two new merge commits"
merge_branch mi2
branch_commit mi2 web/dfx 'file\n' 'branch adds a file'
run mi2 411 --no-push
[ "$rc" -eq 0 ] || fail "I: second landing preparation failed, rc=$rc: $(out 411)"
WT="$WORK/wt-411"; PRE=$(git -C "$WT" rev-parse HEAD)
main_commit web/dfx/y.md 'dir\n' 'main adds a directory'
run mi2 411 --skip-rebase --no-push
[ "$rc" -eq 8 ] || fail "I: a conflicting base move returned rc=$rc, want 8: $(out 411)"
grep -q 'merge of the moved base .* conflicts; aborted, worktree back at' "$WORK/out.411" || fail "I: the conflict was not the reason: $(out 411)"
[ "$(git -C "$WT" rev-parse HEAD)" = "$PRE" ] || fail "I: HEAD was not restored"
[ -z "$(git -C "$WT" status --porcelain)" ] || fail "I: worktree left dirty: $(git -C "$WT" status --short)"
[ -z "$(git -C "$WT" ls-files -u)" ] || fail "I: the index holds unmerged entries"
git -C "$WT" rev-parse -q --verify MERGE_HEAD >/dev/null && fail "I: the merge was left in progress"
FAIL_MERGE_ABORT=1 run mi2 411 --skip-rebase --no-push
[ "$rc" -eq 3 ] || fail "I: a failed abort returned rc=$rc, want 3: $(out 411)"
grep -q 'ERROR: git merge --abort failed' "$WORK/out.411" || fail "I: the failed abort was not named: $(out 411)"
grep -q 'worktree back at' "$WORK/out.411" && fail "I: claimed the worktree was restored"
git -C "$WT" merge --abort

echo "PASS land-prep-merge: auto merge mode with a fast-forward push, CHANGELOG union under merge, refused union keeps git's conflict, other conflicts stop mid-merge, removal guard, renumber after merge, recorded forced modes, concurrent remote advance rejected, base-move merge and restore"
