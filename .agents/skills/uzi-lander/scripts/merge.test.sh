#!/usr/bin/env bash
# Hermetic regression: merge.sh --confirm-only reconciles a merge that landed OUT OF BAND.
# On PR #1510 the classifier refused the in-script `gh pr merge --admin`; the user ran it via
# a `!` line, so merge.sh's own confirm/trail block never executed and NO MERGE_SHA or trail
# was ever produced. --confirm-only writes that owed terminal evidence from the one place that
# knows the true merge SHA.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/merge.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
MSHA=abcabcabcabcabcabcabcabcabcabcabcabcabca

mkdir -p "$WORK/bin" "$WORK/state"
cat > "$WORK/bin/gh" <<STUB
#!/usr/bin/env bash
set -eu
if [ "\${1:-}" = pr ] && [ "\${2:-}" = view ]; then
  if [ "\$MERGE_STATE" = MERGED ]; then
    echo '{"state":"MERGED","headRefOid":"$HEAD","mergeStateStatus":"CLEAN","mergeable":"MERGEABLE","mergeCommit":{"oid":"$MSHA"}}'
  else
    echo '{"state":"OPEN","headRefOid":"$HEAD","mergeStateStatus":"BLOCKED","mergeable":"MERGEABLE","mergeCommit":null}'
  fi
  exit 0
fi
echo "unexpected gh call: \$*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH"
export UZI_LANDER_STATE_DIR="$WORK/state"

# 1. Already MERGED out of band → --confirm-only emits the owed evidence and exits 0.
MERGE_STATE=MERGED; export MERGE_STATE
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/confirm.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--confirm-only on a merged PR returned rc=$rc: $(cat "$WORK/confirm.out")"
grep -q '^MERGED #42$' "$WORK/confirm.out" || fail "--confirm-only did not confirm MERGED: $(cat "$WORK/confirm.out")"
grep -q "^MERGE_SHA=$MSHA$" "$WORK/confirm.out" || fail "--confirm-only did not print the true MERGE_SHA: $(cat "$WORK/confirm.out")"

# 2. Still OPEN → --confirm-only refuses (exit 9): never a merge, never a purge of a live claim.
MERGE_STATE=OPEN; export MERGE_STATE
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/open.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 9 ] || fail "--confirm-only on an OPEN PR returned rc=$rc, want 9: $(cat "$WORK/open.out")"
grep -q 'not MERGED' "$WORK/open.out" || fail "--confirm-only OPEN did not report not-merged: $(cat "$WORK/open.out")"

echo "PASS merge: --confirm-only reconciles an out-of-band merge"
