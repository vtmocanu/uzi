#!/usr/bin/env bash
# Hermetic regression: merge.sh --confirm-only reconciles a merge that landed OUT OF BAND.
# On PR #1510 the classifier refused the in-script `gh pr merge --admin`; the user ran it via
# a `!` line, so merge.sh's own confirm/trail block never executed and NO MERGE_SHA or trail
# was ever produced. --confirm-only writes that owed terminal evidence from the one place that
# knows the true merge SHA. The merge releases the live claim but preserves the trail through
# post-merge CI, so the final status line still contains the complete landing history.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/merge.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
MSHA=abcabcabcabcabcabcabcabcabcabcabcabcabca

mkdir -p "$WORK/bin" "$WORK/state"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat > "$WORK/bin/stat" <<'STUB'
#!/usr/bin/env bash
set -eu
# Model GNU stat: `-c %Y` is the numeric mtime interface, while BSD-first `-f %m`
# succeeds with filesystem-formatted text and therefore is not a portable feature probe.
if [ "${1:-}" = -c ]; then
  last="${!#}"
  if [ "$(uname -s)" = Darwin ]; then /usr/bin/stat -f %m "$last"; else /usr/bin/stat -c %Y "$last"; fi
  exit 0
fi
if [ "${1:-}" = -f ]; then
  echo '  File: "%m"'
  echo '    ID: deadbeef Namelen: 255 Type: ext2/ext3'
  exit 0
fi
exec /usr/bin/stat "$@"
STUB
cat > "$WORK/bin/gh" <<STUB
#!/usr/bin/env bash
set -eu
if [ "\${1:-}" = pr ] && [ "\${2:-}" = view ]; then
  # Resolve (state, oid) for this call. merged_lagging returns state=MERGED but a null oid on
  # the FIRST view, then the real oid — GitHub's eventual-consistency lag. merged_no_oid never
  # populates it.
  st=MERGED; oid='$MSHA'
  case "\$MERGE_STATE" in
    OPEN) st=OPEN; oid= ;;
    merged_no_oid) oid= ;;
    merged_lagging)
      c=0; [ -f "$WORK/calls" ] && c=\$(cat "$WORK/calls"); c=\$((c+1)); echo "\$c" > "$WORK/calls"
      [ "\$c" -ge 2 ] || oid= ;;
  esac
  case "\$*" in
    *'-q .state'*) printf '%s\n' "\$st" ;;
    *-q*)  printf '%s\n' "\$oid" ;;   # the retry: gh -q '.mergeCommit.oid // empty' prints the oid
    *)     if [ -n "\$oid" ]; then
             echo '{"state":"'"\$st"'","headRefOid":"$HEAD","mergeStateStatus":"CLEAN","mergeable":"MERGEABLE","mergeCommit":{"oid":"'"\$oid"'"}}'
           else
             echo '{"state":"'"\$st"'","headRefOid":"$HEAD","mergeStateStatus":"BLOCKED","mergeable":"MERGEABLE","mergeCommit":null}'
           fi ;;
  esac
  exit 0
fi
if [ "\${1:-}" = pr ] && [ "\${2:-}" = checks ]; then printf '%s\n' "\${CHECKS_JSON:-}"; exit "\${CHECKS_RC:-0}"; fi
if [ "\${1:-}" = pr ] && [ "\${2:-}" = merge ]; then echo "\$*" >> "$WORK/merge.log"; exit 0; fi
echo "unexpected gh call: \$*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/sleep" "$WORK/bin/stat"
export PATH="$WORK/bin:$PATH"
export UZI_LANDER_STATE_DIR="$WORK/state"

CL="$WORK/state/claims"; TR="$WORK/state/trail"
mkdir -p "$CL" "$TR"
seed_state() {  # a live claim + a pre-merge trail for #42, so the terminal side effects are observable
  printf '{"key":"#42","repo":"test/repo","pr":42,"owner":"tester","owner_uuid":"unknown-test","state":"pushed"}\n' > "$CL/#42.json"
  printf 'pr opened\nci green\n' > "$TR/#42.trail"
}

# 1. Already MERGED out of band → --confirm-only emits the owed terminal evidence (MERGED, the
#    true MERGE_SHA, the trail's admin-merged line) THEN releases the claim but preserves the
#    trail until the post-merge result is appended, printed and explicitly purged.
MERGE_STATE=MERGED; export MERGE_STATE
seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/confirm.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--confirm-only on a merged PR returned rc=$rc: $(cat "$WORK/confirm.out")"
grep -q '^MERGED #42$' "$WORK/confirm.out" || fail "--confirm-only did not confirm MERGED: $(cat "$WORK/confirm.out")"
grep -q "^MERGE_SHA=$MSHA$" "$WORK/confirm.out" || fail "--confirm-only did not print the true MERGE_SHA: $(cat "$WORK/confirm.out")"
# trail.sh prints the whole trail to stdout and preserves it for post-merge CI, so a deleted
# trail.sh call (the #1510 regression) would drop this line.
grep -q "admin-merged ${MSHA:0:8}" "$WORK/confirm.out" || fail "--confirm-only did not write the terminal trail line: $(cat "$WORK/confirm.out")"
# The live claim is gone, but the trail survives for the post-merge CI result.
[ ! -e "$CL/#42.json" ] || fail "--confirm-only did not release the claim"
[ -e "$TR/#42.trail" ] || fail "--confirm-only purged the trail before post-merge CI"
final=$(bash "$HERE/trail.sh" '#42' 'main ci green')
[ "$final" = "#42: pr opened → ci green → admin-merged ${MSHA:0:8} → main ci green" ] \
  || fail "post-merge CI lost the earlier trail: $final"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null
[ ! -e "$TR/#42.trail" ] || fail "final cleanup did not purge the completed trail"

# If the landing session dies before explicit cleanup, a fresh orphan trail survives reap while
# a stale one is collected after the shared stale-owner TTL. This prevents both callback-time
# history loss and permanent state leakage.
printf 'admin-merged deadbeef\n' > "$TR/#fresh.trail"
printf 'admin-merged deadbeef\n' > "$TR/#stale.trail"
touch -t 200001010000 "$TR/#stale.trail"
bash "$HERE/claims.sh" reap > "$WORK/reap.out"
[ -e "$TR/#fresh.trail" ] || fail "reap removed a fresh post-merge trail"
[ ! -e "$TR/#stale.trail" ] || fail "reap left a stale orphan trail behind"
grep -q 'reaped #stale (orphan trail stale ' "$WORK/reap.out" || fail "reap did not report stale orphan cleanup: $(cat "$WORK/reap.out")"
rm -f "$TR/#fresh.trail"

# A concurrent reap can observe MERGED before merge.sh releases the claim. It must remove the
# terminal claim without deleting the fresh trail the post-merge watcher still needs.
seed_state
bash "$HERE/claims.sh" reap > "$WORK/reap-merged.out"
[ ! -e "$CL/#42.json" ] || fail "reap left a merged claim on the live board"
[ -e "$TR/#42.trail" ] || fail "reap deleted the merged PR trail before post-merge CI"
grep -q 'reaped #42 (PR MERGED; trail preserved)' "$WORK/reap-merged.out" \
  || fail "reap did not report terminal claim-only cleanup: $(cat "$WORK/reap-merged.out")"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null

# A terminal claim can carry a trail older than the orphan TTL after a long review wait.
# Terminal cleanup refreshes its mtime, so neither that reap nor the next immediate reap
# can delete the history before post-merge CI appends its result.
seed_state
touch -t 200001010000 "$TR/#42.trail"
bash "$HERE/claims.sh" reap > "$WORK/reap-stale-terminal.out"
[ ! -e "$CL/#42.json" ] || fail "reap left the stale terminal claim on the live board"
[ -e "$TR/#42.trail" ] || fail "terminal reap deleted an old trail instead of refreshing it"
bash "$HERE/claims.sh" reap > "$WORK/reap-stale-terminal-again.out"
[ -e "$TR/#42.trail" ] || fail "the next immediate reap deleted the refreshed terminal trail"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null

# 2. state=MERGED but mergeCommit not populated yet (GitHub lag) → re-read recovers the oid.
MERGE_STATE=merged_lagging; export MERGE_STATE
rm -f "$WORK/calls"; seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/lag.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--confirm-only did not recover a lagging mergeCommit, rc=$rc: $(cat "$WORK/lag.out")"
grep -q "^MERGE_SHA=$MSHA$" "$WORK/lag.out" || fail "--confirm-only did not re-read the populated oid: $(cat "$WORK/lag.out")"

# 3. state=MERGED but mergeCommit never populates → refuse (exit 9): NEVER an empty MERGE_SHA
#    or a bare admin-merged trail, and the claim stays.
MERGE_STATE=merged_no_oid; export MERGE_STATE
seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/nooid.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 9 ] || fail "--confirm-only emitted evidence with no merge commit, rc=$rc: $(cat "$WORK/nooid.out")"
if grep -qE '^MERGE_SHA=$|admin-merged $' "$WORK/nooid.out"; then fail "--confirm-only wrote empty terminal evidence: $(cat "$WORK/nooid.out")"; fi
[ -e "$CL/#42.json" ] || fail "--confirm-only purged the claim without confirming the merge"

# 4. Still OPEN → refuse (exit 9): never a merge, never a purge of a live claim/trail.
MERGE_STATE=OPEN; export MERGE_STATE
seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/open.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 9 ] || fail "--confirm-only on an OPEN PR returned rc=$rc, want 9: $(cat "$WORK/open.out")"
grep -q 'not MERGED' "$WORK/open.out" || fail "--confirm-only OPEN did not report not-merged: $(cat "$WORK/open.out")"
{ [ -e "$CL/#42.json" ] && [ -e "$TR/#42.trail" ]; } || fail "--confirm-only OPEN mutated the claim/trail: $(cat "$WORK/open.out")"

# 5. The guarded merge's required-checks gate is fail-closed. An EMPTY list (a head with no
#    check runs: CI skipped, a missed dispatch) and an unreadable reply both refuse with
#    exit 2 and no merge call; a failing check is read from the array gh prints alongside
#    its non-zero exit; a passing list reaches the merge (the positive control).
MERGE_STATE=OPEN; export MERGE_STATE
merge_run() { # label -> rc, output in $WORK/m.<label>
  rm -f "$WORK/merge.log"
  set +e
  bash "$SCRIPT" test/repo 42 --no-rework-check > "$WORK/m.$1" 2>&1
  rc=$?
  set -e
}
CHECKS_JSON='[]' CHECKS_RC=0; export CHECKS_JSON CHECKS_RC
merge_run empty
[ "$rc" -eq 2 ] || fail "an empty required-checks list returned rc=$rc, want 2: $(cat "$WORK/m.empty")"
grep -q "no required checks reported on ${HEAD:0:8}; not merging" "$WORK/m.empty" || fail "empty list not named: $(cat "$WORK/m.empty")"
[ ! -e "$WORK/merge.log" ] || fail "merged with no required checks reported"
CHECKS_JSON='' CHECKS_RC=1
merge_run unreadable
[ "$rc" -eq 2 ] || fail "a failed gh pr checks returned rc=$rc, want 2: $(cat "$WORK/m.unreadable")"
grep -q 'cannot read the required checks for #42 (gh exit 1)' "$WORK/m.unreadable" || fail "gh failure not named: $(cat "$WORK/m.unreadable")"
[ ! -e "$WORK/merge.log" ] || fail "merged with unreadable required checks"
CHECKS_JSON='[{"bucket":"pass"},{"bucket":"fail"}]' CHECKS_RC=1
merge_run failing
[ "$rc" -eq 1 ] || fail "a failing required check returned rc=$rc, want 1: $(cat "$WORK/m.failing")"
[ ! -e "$WORK/merge.log" ] || fail "merged with a failing required check"
CHECKS_JSON='[{"bucket":"pass"}]' CHECKS_RC=0
merge_run green
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "a green required-checks list did not reach the merge: $(cat "$WORK/m.green")"
unset CHECKS_JSON CHECKS_RC

echo "PASS merge: --confirm-only reconciles an out-of-band merge; empty/unreadable required checks refuse"
