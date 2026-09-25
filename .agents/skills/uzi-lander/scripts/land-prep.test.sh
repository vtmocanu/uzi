#!/usr/bin/env bash
# Hermetic regression: land-prep must not push after the PR base moves during gates in a way
# that touches the branch's own files (a disjoint move is tolerated: land-prep-basemove.test.sh).
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/land-prep.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

ORIGIN="$WORK/origin.git"
SEED="$WORK/seed"
ROOT="$WORK/root"
WT="$WORK/land"
mkdir -p "$WORK/bin"
git init -q --bare "$ORIGIN"
git init -q -b main "$SEED"
git -C "$SEED" config user.name test
git -C "$SEED" config user.email test@example.com
printf 'base\none\ntwo\nthree\nfour\n' > "$SEED/base.txt"
git -C "$SEED" add base.txt
git -C "$SEED" commit -qm base
git -C "$SEED" remote add origin "$ORIGIN"
git -C "$SEED" push -q -u origin main
BASE_HEAD=$(git -C "$SEED" rev-parse HEAD)
git --git-dir="$ORIGIN" symbolic-ref HEAD refs/heads/main
git -C "$SEED" switch -qc feature
mkdir -p "$SEED/web"
printf 'feature\n' > "$SEED/web/feature.txt"
# The branch also edits base.txt's first line, so the base move below touches a branch path
# (overlap) while still rebasing cleanly: the refusal comes from the overlap, not a conflict.
printf 'base-feature\none\ntwo\nthree\nfour\n' > "$SEED/base.txt"
git -C "$SEED" add web/feature.txt base.txt
git -C "$SEED" commit -qm feature
FEATURE_HEAD=$(git -C "$SEED" rev-parse HEAD)
git -C "$SEED" push -q -u origin feature
git -C "$SEED" switch -q main
git clone -q "$ORIGIN" "$ROOT"
git -C "$ROOT" config user.name test
git -C "$ROOT" config user.email test@example.com

cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then
  printf '%s\n' "$PR_JSON"
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then echo '[]'; exit 0; fi
echo "unexpected uzi call: $*" >&2
exit 1
STUB
cat > "$WORK/bin/task" <<'STUB'
#!/usr/bin/env bash
set -eu
[ "${1:-}" = gate:web ] || { echo "unexpected task: $*" >&2; exit 1; }
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi" "$WORK/bin/task"

PR_JSON=$(jq -cn --arg h "$FEATURE_HEAD" '{
  state:"OPEN",
  headRefName:"feature",
  baseRefName:"main",
  headRefOid:$h,
  headRepository:{name:"uzi"},
  headRepositoryOwner:{login:"test"}
}')
export PR_JSON
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --no-push > "$WORK/prepared.out" 2>&1 \
  || fail "initial preparation failed: $(cat "$WORK/prepared.out")"
grep -q '^RESULT=prepared ' "$WORK/prepared.out" || fail "initial preparation did not stop before push"

# Move main after preparation, matching a long gate or delayed --skip-rebase re-entry. The
# move touches base.txt, which the branch also changes, so it is not tolerated.
printf 'advanced\n' >> "$SEED/base.txt"
git -C "$SEED" add base.txt
git -C "$SEED" commit -qm 'advance main after preparation'
git -C "$SEED" push -q origin main
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --skip-rebase --gate none > "$WORK/out" 2>&1
rc=$?
set -e
[ "$rc" -eq 8 ] || fail "base movement returned rc=$rc, want 8: $(cat "$WORK/out")"
grep -q '^RESULT=base_moved$' "$WORK/out" || fail "base movement was not named: $(cat "$WORK/out")"
remote_feature=$(git --git-dir="$ORIGIN" rev-parse refs/heads/feature)
[ "$remote_feature" = "$FEATURE_HEAD" ] || fail "stale-base branch was pushed"
remote_main=$(git --git-dir="$ORIGIN" rev-parse refs/heads/main)
[ "$remote_main" != "$BASE_HEAD" ] || fail "fixture did not move remote main"

# --fresh must not silently drop a local commit the remote lacks (a fix committed after the
# rebase, #1574): the old HEAD is kept under a backup ref and the commit is named.
printf 'local fix\n' > "$WT/web/fix.txt"
git -C "$WT" add web/fix.txt
git -C "$WT" commit -qm 'local fix after rebase'
LOCAL_FIX=$(git -C "$WT" rev-parse HEAD)
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --fresh --no-push --gate none > "$WORK/fresh.out" 2>&1 \
  || fail "--fresh restart failed: $(cat "$WORK/fresh.out")"
grep -q "^FRESH_BACKUP=refs/uzi-lander/fresh-backup/pr-99/$LOCAL_FIX\$" "$WORK/fresh.out" || fail "--fresh kept no backup of the local commit: $(cat "$WORK/fresh.out")"
[ "$(git -C "$WT" rev-parse "refs/uzi-lander/fresh-backup/pr-99/$LOCAL_FIX")" = "$LOCAL_FIX" ] || fail "backup ref does not hold the old HEAD"
grep -q 'local fix after rebase' "$WORK/fresh.out" || fail "the dropped local commit was not named: $(cat "$WORK/fresh.out")"
if grep -q 'feature$' "$WORK/fresh.out"; then fail "a commit the remote already carries was listed as local-only"; fi
grep -q '^RESULT=prepared ' "$WORK/fresh.out" || fail "--fresh did not re-prepare: $(cat "$WORK/fresh.out")"
# A second --fresh with a different local commit keeps BOTH backups: no overwrite.
printf 'second fix\n' > "$WT/web/fix2.txt"
git -C "$WT" add web/fix2.txt
git -C "$WT" commit -qm 'second local fix'
SECOND_FIX=$(git -C "$WT" rev-parse HEAD)
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --fresh --no-push --gate none > "$WORK/fresh2.out" 2>&1 \
  || fail "second --fresh failed: $(cat "$WORK/fresh2.out")"
[ "$(git -C "$WT" rev-parse "refs/uzi-lander/fresh-backup/pr-99/$SECOND_FIX")" = "$SECOND_FIX" ] || fail "second backup missing"
[ "$(git -C "$WT" rev-parse "refs/uzi-lander/fresh-backup/pr-99/$LOCAL_FIX")" = "$LOCAL_FIX" ] || fail "the first backup was overwritten by the second --fresh"
# A failed git cherry aborts before the reset: HEAD (with a local commit) must survive.
printf 'third fix\n' > "$WT/web/fix3.txt"
git -C "$WT" add web/fix3.txt
git -C "$WT" commit -qm 'third local fix'
THIRD_FIX=$(git -C "$WT" rev-parse HEAD)
REAL_GIT=$(command -v git)
cat > "$WORK/bin/git" <<STUB
#!/usr/bin/env bash
for a in "\$@"; do [ "\$a" = cherry ] && { echo "fatal: simulated cherry failure" >&2; exit 128; }; done
exec "$REAL_GIT" "\$@"
STUB
chmod +x "$WORK/bin/git"
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --fresh --no-push --gate none > "$WORK/fresh-cherry-fail.out" 2>&1
rc=$?
set -e
rm -f "$WORK/bin/git"
[ "$rc" -eq 3 ] || fail "a failed git cherry did not abort --fresh, rc=$rc: $(cat "$WORK/fresh-cherry-fail.out")"
[ "$(git -C "$WT" rev-parse HEAD)" = "$THIRD_FIX" ] || fail "--fresh reset the worktree after git cherry failed"
git -C "$WT" reset -q --hard "$THIRD_FIX~1" # back to the prepared head of the second --fresh

# A REBASE_HEAD left behind by a COMPLETED rebase is not a rebase in progress (#1574).
git -C "$WT" update-ref REBASE_HEAD HEAD
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --skip-rebase --no-push --gate none > "$WORK/stale-rebase-head.out" 2>&1 \
  || fail "a stale REBASE_HEAD blocked a finished landing: $(cat "$WORK/stale-rebase-head.out")"
git -C "$WT" update-ref -d REBASE_HEAD
# ...but a real in-progress rebase in a LINKED worktree (where .git is a file) still stops it.
RM_DIR="$(git -C "$WT" rev-parse --absolute-git-dir)/rebase-merge"
mkdir "$RM_DIR"
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --skip-rebase --no-push --gate none > "$WORK/in-progress.out" 2>&1
rc=$?
set -e
rmdir "$RM_DIR"
[ "$rc" -eq 5 ] || fail "an in-progress rebase in a linked worktree was not detected, rc=$rc: $(cat "$WORK/in-progress.out")"

echo "PASS land-prep: base movement blocks stale push; --fresh backs up local commits; rebase-state detection"
