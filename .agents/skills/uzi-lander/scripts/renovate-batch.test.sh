#!/usr/bin/env bash
# Hermetic regression: renovate-batch.sh cherry-picks each Renovate PR's exact head onto a new
# branch at a pinned BASE with -x, skips a conflicting PR (multi-commit included) only after a
# verified rollback while later ones still apply, stops on an unverified rollback or a failed
# git read, keeps its ranges fixed when origin/main moves mid-run, resolves a relative --dir
# against the caller, owns and cleans its temporary refs, and refuses (creating no branch or
# worktree) on a non-renovate head, a non-open PR or a head that moved between gh and fetch.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/renovate-batch.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
REAL_GIT=$(command -v git)

g() { git -C "$WORK/seed" "$@"; }
git init -q --bare "$WORK/origin.git"
git init -q -b main "$WORK/seed"
for f in deps:a ci:x lock:c lock2:d; do printf '%s 1\n' "${f#*:}" > "$WORK/seed/${f%%:*}.txt"; done
printf 'b 1\n' >> "$WORK/seed/deps.txt"
g add -A; g commit -qm base; g remote add origin "$WORK/origin.git"; g push -q origin main
edit() { sed -i.bak "s/$2/$3/" "$WORK/seed/$1"; rm "$WORK/seed/$1.bak"; g commit -qam "chore(deps): update $3"; }
mkpr() { # mkpr N FILE OLD NEW [FILE OLD NEW]...
  local n=$1; shift
  g checkout -q -b "renovate/pr-$n" main
  while [ $# -gt 0 ]; do edit "$1" "$2" "$3"; shift 3; done
  g push -q origin "HEAD:refs/pull/$n/head"
  g checkout -q main
}
mkpr 11 deps.txt 'a 1' 'a 2'
mkpr 12 ci.txt 'x 1' 'x 2'
mkpr 13 deps.txt 'a 1' 'a 9'                         # conflicts with #11 once #11 is picked
mkpr 14 lock.txt 'c 1' 'c 2'                         # still applies after the skipped #13
mkpr 18 lock2.txt 'd 1' 'd 2' deps.txt 'a 1' 'a 7'   # first commit picks, second conflicts
oid() { git -C "$WORK/origin.git" rev-parse "refs/pull/$1/head"; }
git -C "$WORK/origin.git" update-ref refs/pull/17/head "$(oid 11)"   # fetchable; gh lies about it

git clone -q "$WORK/origin.git" "$WORK/repo" 2>/dev/null
mkdir -p "$WORK/repo/sub" "$WORK/bin"
cat > "$WORK/bin/gh" <<STUB
#!/usr/bin/env bash
n="\$3"; head="renovate/pr-\$n"; state=OPEN
oid=\$("$REAL_GIT" -C "$WORK/origin.git" rev-parse "refs/pull/\$n/head")
[ "\$n" = 15 ] && head=feature/x && oid=\$("$REAL_GIT" -C "$WORK/origin.git" rev-parse refs/pull/11/head)
[ "\$n" = 16 ] && state=MERGED && oid=\$("$REAL_GIT" -C "$WORK/origin.git" rev-parse refs/pull/11/head)
[ "\$n" = 17 ] && oid=0000000000000000000000000000000000000000
title=\$("$REAL_GIT" -C "$WORK/origin.git" log -1 --format=%s "refs/pull/\$( [ "\$n" -ge 15 ] && [ "\$n" -le 17 ] && echo 11 || echo "\$n")/head")
case "\$*" in
  "pr view \$3 --repo test/repo --json number,title,headRefName,headRefOid,baseRefName,state")
    printf '{"number":%s,"title":"%s","headRefName":"%s","headRefOid":"%s","baseRefName":"main","state":"%s"}\n' "\$3" "\$title" "\$head" "\$oid" "\$state" ;;
  *) echo "unexpected gh call: \$*" >&2; exit 1 ;;
esac
STUB
cat > "$WORK/bin/git" <<STUB
#!/usr/bin/env bash
case " \$* " in
  *" cherry-pick --abort "*) [ -n "\${INJECT_ABORT_FAIL:-}" ] && exit 1; [ -n "\${INJECT_ABORT_NOOP:-}" ] && exit 0 ;;
  *" rev-list --reverse "*) [ -n "\${INJECT_REVLIST_FAIL:-}" ] && case " \$* " in *"\$INJECT_REVLIST_FAIL"*) exit 1;; esac ;;
  *" worktree add "*)
    "$REAL_GIT" "\$@" || exit \$?
    [ -n "\${INJECT_MOVE_MAIN:-}" ] && "$REAL_GIT" -C "$WORK/repo" update-ref refs/remotes/origin/main "\$INJECT_MOVE_MAIN"
    exit 0 ;;
esac
exec "$REAL_GIT" "\$@"
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/git"
export PATH="$WORK/bin:$PATH"
run() { (cd "$WORK/repo" && "$SCRIPT" --repo test/repo "$@"); }
reset_main() { "$REAL_GIT" -C "$WORK/repo" fetch -q origin main; }
batch_refs() { "$REAL_GIT" -C "$WORK/repo" for-each-ref --format='%(refname)' refs/renovate-batch/; }
# Another invocation's ref must survive every run below.
"$REAL_GIT" -C "$WORK/repo" fetch -q origin refs/pull/11/head:refs/renovate-batch/other-1/pr-11
refs_ok() { [ "$(batch_refs)" = refs/renovate-batch/other-1/pr-11 ] || fail "$1: refs left: $(batch_refs)"; }

# 1. Mixed batch: #13 and multi-commit #18 skipped after verified rollback; #11/#12/#14 applied with -x.
rc=0; out=$(run --branch chore/b1 --dir "$WORK/b1" 11 12 13 18 14 2>&1) || rc=$?
[ "$rc" = 1 ] || fail "case1 rc=$rc: $out"
grep -qx 'PICKED=11 12 14' <<<"$out" || fail "case1 PICKED: $out"
grep -qx 'SKIPPED=13 18' <<<"$out" || fail "case1 SKIPPED: $out"
grep -qx 'SKIP_REASON_13=cherry-pick conflict' <<<"$out" || fail "case1 reason 13: $out"
grep -qx 'SKIP_REASON_18=cherry-pick conflict' <<<"$out" || fail "case1 reason 18: $out"
grep -qx "BASE=$("$REAL_GIT" -C "$WORK/origin.git" rev-parse main)" <<<"$out" || fail "case1 BASE: $out"
# shellcheck disable=SC2016
grep -qF '| #11 | a 2 | `deps.txt` |' <<<"$out" || fail "case1 row 11: $out"
[ "$(git -C "$WORK/b1" rev-parse --abbrev-ref HEAD)" = chore/b1 ] || fail "case1 branch"
[ -z "$(git -C "$WORK/b1" status --porcelain)" ] || fail "case1 tree dirty after skip"
[ "$(cat "$WORK/b1/deps.txt")" = "$(printf 'a 2\nb 1')" ] || fail "case1 deps content"
[ "$(cat "$WORK/b1/lock.txt")" = 'c 2' ] || fail "case1 lock content"
[ "$(cat "$WORK/b1/lock2.txt")" = 'd 1' ] || fail "case1 #18's first commit survived its skip"
for n in 11 12 14; do
  git -C "$WORK/b1" log --format=%B | grep -qF "cherry picked from commit $(oid "$n")" || fail "case1 -x trailer for #$n"
done
refs_ok case1

# 2. Refusals create no worktree and no branch, and clean their own refs.
for bad in 15 16 17; do
  rc=0; out=$(run --branch "chore/bad-$bad" --dir "$WORK/bad-$bad" 11 "$bad" 2>&1) || rc=$?
  [ "$rc" = 3 ] || fail "#$bad rc=$rc: $out"
  [ -e "$WORK/bad-$bad" ] && fail "#$bad created a worktree"
  git -C "$WORK/repo" show-ref --verify --quiet "refs/heads/chore/bad-$bad" && fail "#$bad created a branch"
  refs_ok "#$bad"
done
grep -qF 'gh reports head 0000000000000000000000000000000000000000' <<<"$out" || fail "#17 not refused for a moved head: $out"
rc=0; run --branch renovate/x --dir "$WORK/r" 11 >/dev/null 2>&1 || rc=$?
[ "$rc" = 3 ] || fail "renovate/* branch name accepted (rc=$rc)"
rc=0; run --dir "$WORK/b1" 11 >/dev/null 2>&1 || rc=$?
[ "$rc" = 3 ] || fail "existing dir accepted (rc=$rc)"
rc=0; run abc >/dev/null 2>&1 || rc=$?
[ "$rc" = 2 ] || fail "non-numeric PR accepted (rc=$rc)"

# 3. All clean, relative --dir from a subdirectory resolves against the caller's cwd.
rc=0; out=$(cd "$WORK/repo/sub" && "$SCRIPT" --repo test/repo --branch chore/b2 --dir ../../rel 12 14 2>&1) || rc=$?
[ "$rc" = 0 ] || fail "case3 rc=$rc: $out"
grep -qx "DIR=$WORK/repo/sub/../../rel" <<<"$out" || fail "case3 DIR: $out"
[ "$(cat "$WORK/rel/ci.txt")" = 'x 2' ] || fail "case3 worktree not at the caller-relative path"
refs_ok case3

# 4. origin/main moving to #11's head mid-run must not change #11's range (still picked).
rc=0; out=$(INJECT_MOVE_MAIN="$(oid 11)" run --branch chore/b3 --dir "$WORK/b3" 11 2>&1) || rc=$?
[ "$rc" = 0 ] || fail "case4 rc=$rc: $out"
grep -qx 'PICKED=11' <<<"$out" || fail "case4 range followed the moved origin/main: $out"
reset_main

# 5. An unverified rollback stops the batch: #14 after the conflict is never attempted.
rc=0; out=$(INJECT_ABORT_FAIL=1 run --branch chore/b4 --dir "$WORK/b4" 11 13 14 2>&1) || rc=$?
[ "$rc" = 4 ] || fail "case5 rc=$rc: $out"
grep -qx 'PICKED=11' <<<"$out" || fail "case5 PICKED: $out"
grep -qF 'stopped at #13' <<<"$out" || fail "case5 reason: $out"
[ "$(cat "$WORK/b4/lock.txt")" = 'c 1' ] || fail "case5 #14 attempted on uncertain state"
refs_ok case5

# 6. A failed commit enumeration stops the batch before any later PR.
rc=0; out=$(INJECT_REVLIST_FAIL="$(oid 12)" run --branch chore/b5 --dir "$WORK/b5" 11 12 14 2>&1) || rc=$?
[ "$rc" = 4 ] || fail "case6 rc=$rc: $out"
grep -qF 'stopped at #12: rev-list failed' <<<"$out" || fail "case6 reason: $out"
[ "$(cat "$WORK/b5/lock.txt")" = 'c 1' ] || fail "case6 #14 attempted after a failed read"
refs_ok case6

# 7. An abort that reports success but leaves the pick in progress is not a verified rollback.
rc=0; out=$(INJECT_ABORT_NOOP=1 run --branch chore/b6 --dir "$WORK/b6" 11 13 14 2>&1) || rc=$?
[ "$rc" = 4 ] || fail "case7 rc=$rc: $out"
grep -qF 'stopped at #13: cherry-pick conflict, rollback not verified' <<<"$out" || fail "case7 reason: $out"
[ "$(cat "$WORK/b6/lock.txt")" = 'c 1' ] || fail "case7 #14 attempted on uncertain state"
refs_ok case7

echo "renovate-batch.test.sh: ok"
