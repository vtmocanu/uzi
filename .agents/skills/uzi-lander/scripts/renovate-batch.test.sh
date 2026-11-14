#!/usr/bin/env bash
# Hermetic regression: renovate-batch.sh cherry-picks each Renovate PR's exact head onto a new
# branch off origin/main with -x, skips a conflicting PR cleanly while later ones still apply,
# prints the PR-body table, and refuses (creating nothing) on a non-renovate head, a non-open
# PR or a head that moved between gh and fetch.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/renovate-batch.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

g() { git -C "$WORK/seed" "$@"; }
git init -q --bare "$WORK/origin.git"
git init -q -b main "$WORK/seed"
printf 'a 1\nb 1\n' > "$WORK/seed/deps.txt"; printf 'x 1\n' > "$WORK/seed/ci.yml"; printf 'c 1\n' > "$WORK/seed/lock.txt"
g add -A; g commit -qm base; g remote add origin "$WORK/origin.git"; g push -q origin main
mkpr() { # mkpr N FILE OLD NEW
  g checkout -q -b "renovate/pr-$1" main
  sed -i.bak "s/$3/$4/" "$WORK/seed/$2"; rm "$WORK/seed/$2.bak"
  g commit -qam "chore(deps): update $4"; g push -q origin "HEAD:refs/pull/$1/head"
  g checkout -q main
}
mkpr 11 deps.txt 'a 1' 'a 2'
mkpr 12 ci.yml 'x 1' 'x 2'
mkpr 13 deps.txt 'a 1' 'a 9'   # conflicts with #11 once #11 is picked
mkpr 14 lock.txt 'c 1' 'c 2'   # still applies after the skipped #13
oid() { git -C "$WORK/origin.git" rev-parse "refs/pull/$1/head"; }

git clone -q "$WORK/origin.git" "$WORK/repo"
mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<STUB
#!/usr/bin/env bash
n="\$3"; head="renovate/pr-\$n"; state=OPEN; oid=\$(git -C "$WORK/origin.git" rev-parse "refs/pull/\$n/head")
[ "\$n" = 15 ] && head=feature/x
[ "\$n" = 16 ] && state=MERGED
[ "\$n" = 17 ] && oid=0000000000000000000000000000000000000000
[ "\$n" = 17 ] && n=11
title=\$(git -C "$WORK/origin.git" log -1 --format=%s "refs/pull/\$n/head")
case "\$*" in
  "pr view \$3 --repo test/repo --json number,title,headRefName,headRefOid,baseRefName,state")
    printf '{"number":%s,"title":"%s","headRefName":"%s","headRefOid":"%s","baseRefName":"main","state":"%s"}\n' "\$3" "\$title" "\$head" "\$oid" "\$state" ;;
  *) echo "unexpected gh call: \$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH"
run() { (cd "$WORK/repo" && "$SCRIPT" --repo test/repo "$@"); }

# 1. Clean batch plus one conflict: #13 skipped, #11/#12/#14 applied with -x, exit 1.
rc=0; out=$(run --branch chore/b1 --dir "$WORK/b1" 11 12 13 14 2>&1) || rc=$?
[ "$rc" = 1 ] || fail "case1 rc=$rc: $out"
grep -qx 'PICKED=11 12 14' <<<"$out" || fail "case1 PICKED: $out"
grep -qx 'SKIPPED=13' <<<"$out" || fail "case1 SKIPPED: $out"
grep -qx 'SKIP_REASON_13=cherry-pick conflict' <<<"$out" || fail "case1 reason: $out"
grep -qF '| #11 | a 2 | `deps.txt` |' <<<"$out" || fail "case1 row 11: $out"
grep -qF '| #12 | x 2 | `ci.yml` |' <<<"$out" || fail "case1 row 12: $out"
[ "$(git -C "$WORK/b1" rev-parse --abbrev-ref HEAD)" = chore/b1 ] || fail "case1 branch"
[ -z "$(git -C "$WORK/b1" status --porcelain)" ] || fail "case1 tree dirty after skip"
[ "$(cat "$WORK/b1/deps.txt")" = "$(printf 'a 2\nb 1')" ] && [ "$(cat "$WORK/b1/lock.txt")" = 'c 2' ] || fail "case1 content"
for n in 11 12 14; do
  git -C "$WORK/b1" log --format=%B | grep -qF "cherry picked from commit $(oid "$n")" || fail "case1 -x trailer for #$n"
done
git -C "$WORK/repo" for-each-ref refs/renovate-batch | grep -q . && fail "case1 temp refs left behind"

# 2. Refusals create no worktree and no branch.
for bad in 15 16 17; do
  rc=0; out=$(run --branch "chore/bad-$bad" --dir "$WORK/bad-$bad" 11 "$bad" 2>&1) || rc=$?
  [ "$rc" = 3 ] || fail "#$bad rc=$rc: $out"
  [ -e "$WORK/bad-$bad" ] && fail "#$bad created a worktree"
  git -C "$WORK/repo" show-ref --verify --quiet "refs/heads/chore/bad-$bad" && fail "#$bad created a branch"
done
rc=0; run --branch renovate/x --dir "$WORK/r" 11 >/dev/null 2>&1 || rc=$?
[ "$rc" = 3 ] || fail "renovate/* branch name accepted (rc=$rc)"
rc=0; run --dir "$WORK/b1" 11 >/dev/null 2>&1 || rc=$?
[ "$rc" = 3 ] || fail "existing dir accepted (rc=$rc)"
rc=0; run abc >/dev/null 2>&1 || rc=$?
[ "$rc" = 2 ] || fail "non-numeric PR accepted (rc=$rc)"

# 3. All clean: exit 0.
rc=0; out=$(run --branch chore/b2 --dir "$WORK/b2" 12 14 2>&1) || rc=$?
[ "$rc" = 0 ] || fail "case3 rc=$rc: $out"
grep -qx 'SKIPPED=' <<<"$out" || fail "case3 SKIPPED: $out"

echo "renovate-batch.test.sh: ok"
