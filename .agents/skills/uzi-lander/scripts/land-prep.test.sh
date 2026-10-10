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
  if [ -n "${GH_ALWAYS_FAIL:-}" ] || { [ -n "${GH_BLIP_FILE:-}" ] && [ -e "$GH_BLIP_FILE" ]; }; then
    [ -z "${GH_BLIP_FILE:-}" ] || rm -f "$GH_BLIP_FILE"
    echo "error connecting to api.github.com" >&2; exit 1
  fi
  printf '%s\n' "$PR_JSON"
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then
  if [ -n "${UZI_ALWAYS_FAIL:-}" ] || { [ -n "${UZI_BLIP_FILE:-}" ] && [ -e "$UZI_BLIP_FILE" ]; }; then
    [ -z "${UZI_BLIP_FILE:-}" ] || rm -f "$UZI_BLIP_FILE"
    echo "error connecting to uzi.example" >&2; exit 1
  fi
  echo '[]'; exit 0
fi
echo "unexpected uzi call: $*" >&2
exit 1
STUB
cat > "$WORK/bin/task" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$TASK_CALLS"
[ "${1:-}" = gate:web ] || { echo "unexpected task: $*" >&2; exit 1; }
STUB
# Recording sleep stub: net_retry backoff must cost no real time (NET_RETRY_BASE_SLEEP=0).
export NET_RETRY_BASE_SLEEP=0
export SLEEP_LOG="$WORK/sleeps"; : > "$SLEEP_LOG"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$1" >> "$SLEEP_LOG"
STUB
chmod +x "$WORK/bin/sleep"
export TASK_CALLS="$WORK/task-calls"
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
# The default runs NO local gate: CI on the pushed head is the gate, and the run says so.
: > "$TASK_CALLS"
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --no-push > "$WORK/prepared.out" 2>&1 \
  || fail "initial preparation failed: $(cat "$WORK/prepared.out")"
grep -q '^RESULT=prepared ' "$WORK/prepared.out" || fail "initial preparation did not stop before push"
[ ! -s "$TASK_CALLS" ] || fail "the default ran a local gate: $(cat "$TASK_CALLS")"
grep -q '^LOCAL_GATES=none ' "$WORK/prepared.out" || fail "the default did not report LOCAL_GATES=none: $(cat "$WORK/prepared.out")"
# --gate auto still runs the full component gate the changed paths select (web/ -> gate:web).
PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --skip-rebase --no-push --gate auto > "$WORK/auto.out" 2>&1 \
  || fail "--gate auto preparation failed: $(cat "$WORK/auto.out")"
grep -qx 'gate:web' "$TASK_CALLS" || fail "--gate auto did not run gate:web: $(cat "$TASK_CALLS")"
grep -q '^LOCAL_GATES=none' "$WORK/auto.out" && fail "--gate auto reported LOCAL_GATES=none"

# Transient transport errors on the read-only lookups are retried (lib/net-retry.sh).
: > "$SLEEP_LOG"
run_lp() { # extra env via caller; prints rc
  local rc=0
  PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --skip-rebase --no-push --gate none "$@" > "$WORK/blip.out" 2>&1 || rc=$?
  echo "$rc"
}
: > "$WORK/gh-blip"
rc=$(GH_BLIP_FILE="$WORK/gh-blip" run_lp --no-rework-check)
[ "$rc" -eq 0 ] || fail "a one-shot gh pr view blip failed land-prep (rc=$rc): $(cat "$WORK/blip.out")"
grep -q 'gh pr view .* failed' "$WORK/blip.out" && fail "gh pr view blip surfaced as a failure"
[ -s "$SLEEP_LOG" ] || fail "gh pr view blip was not retried through sleep"
: > "$WORK/uzi-blip"
rc=$(UZI_BLIP_FILE="$WORK/uzi-blip" run_lp)
[ "$rc" -eq 0 ] || fail "a one-shot uzi repo list blip failed land-prep (rc=$rc): $(cat "$WORK/blip.out")"
grep -q 'uzi repo list failed' "$WORK/blip.out" && fail "uzi repo list blip surfaced as a failure"
rc=$(GH_ALWAYS_FAIL=1 run_lp --no-rework-check)
[ "$rc" -eq 3 ] || fail "a persistent gh outage returned rc=$rc, want 3: $(cat "$WORK/blip.out")"
grep -q 'gh pr view 99 failed' "$WORK/blip.out" || fail "persistent gh outage not named"
rc=$(UZI_ALWAYS_FAIL=1 run_lp)
[ "$rc" -eq 4 ] || fail "a persistent uzi outage returned rc=$rc, want 4: $(cat "$WORK/blip.out")"
grep -q 'uzi repo list failed' "$WORK/blip.out" || fail "persistent uzi outage not named"
# Retries never cost real time, and a PATH without uzi still hits the absent-uzi refusal.
grep -qvx 0 "$SLEEP_LOG" && fail "non-zero backoff recorded: $(sort -u "$SLEEP_LOG" | tr '\n' ' ')"
mkdir -p "$WORK/nouzi"
for t in bash env git jq awk sed tr head cat rm mktemp grep date dirname basename uname sort cut tail wc mkdir sleep; do
  p=$(command -v "$t" 2>/dev/null) && ln -sf "$p" "$WORK/nouzi/$t"
done
ln -sf "$WORK/bin/gh" "$WORK/nouzi/gh"
rc=0
PATH="$WORK/nouzi" bash "$SCRIPT" test/uzi 99 --repo-root "$ROOT" --worktree "$WT" --skip-rebase --no-push --gate none > "$WORK/blip.out" 2>&1 || rc=$?
[ "$rc" -eq 4 ] || fail "no-uzi PATH returned rc=$rc, want 4: $(cat "$WORK/blip.out")"
grep -q 'uzi CLI absent' "$WORK/blip.out" || fail "no-uzi PATH did not hit the absent-uzi refusal: $(cat "$WORK/blip.out")"

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
