#!/usr/bin/env bash
# Hermetic regression: land-prep must not push after the PR base moves during gates.
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
printf 'base\n' > "$SEED/base.txt"
git -C "$SEED" add base.txt
git -C "$SEED" commit -qm base
git -C "$SEED" remote add origin "$ORIGIN"
git -C "$SEED" push -q -u origin main
BASE_HEAD=$(git -C "$SEED" rev-parse HEAD)
git --git-dir="$ORIGIN" symbolic-ref HEAD refs/heads/main
git -C "$SEED" switch -qc feature
mkdir -p "$SEED/web"
printf 'feature\n' > "$SEED/web/feature.txt"
git -C "$SEED" add web/feature.txt
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

# Move main after preparation, matching a long gate or delayed --skip-rebase re-entry.
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

echo "PASS land-prep: base movement blocks stale push"
