#!/usr/bin/env bash
# Hermetic regression for CodeRabbit's "already reviewed; use full review" terminal reply.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/watch-pr.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then echo '[]'; exit 0; fi
echo "unexpected uzi call: $*" >&2
exit 1
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then
  case "$*" in
    *'--json headRefOid,state'*) echo '{"headRefOid":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"OPEN"}' ;;
    *'-q .headRefOid'*) echo deadbeefdeadbeefdeadbeefdeadbeefdeadbeef ;;
    *'-q .baseRefName'*) echo main ;;
    *) echo "unexpected pr view: $*" >&2; exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = pr ] && [ "${2:-}" = checks ]; then
  echo '[{"bucket":"pass"}]'
  exit 0
fi
if [ "${1:-}" = api ]; then
  case "$*" in
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/status'*)
      echo '{"statuses":[{"context":"CodeRabbit","description":"Review completed"}]}' ;;
    *'/pulls/42/reviews'*) echo '[]' ;;
    *'/issues/42/comments'*) cat "$COMMENTS" ;;
    *'/pulls/42/comments'*) echo '[]' ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*) echo '{"check_runs":[]}' ;;
    *) echo "unexpected gh api: $*" >&2; exit 1 ;;
  esac
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi" "$WORK/bin/sleep"
export PATH="$WORK/bin:$PATH"
export COMMENTS="$WORK/comments.json"

jq -n '[
  {user:{login:"tester"},body:"@coderabbitai review",created_at:"2026-09-18T11:12:00Z"},
  {user:{login:"coderabbitai[bot]"},body:"Action not completed: Already reviewed the last commit. Use @coderabbitai full review to rerun a review of the entire changeset.",created_at:"2026-09-18T11:12:56Z"}
]' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/required.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 7 ] || fail "missed full-review reply, rc=$rc: $(cat "$WORK/required.out")"
grep -q "^RESULT=cr_full_review_required command='@coderabbitai full review'$" "$WORK/required.out" \
  || fail "full-review action was not surfaced: $(cat "$WORK/required.out")"

# Once the prescribed command was posted, the same old reply must not request it again.
jq '. + [{user:{login:"tester"},body:"@coderabbitai full review",created_at:"2026-09-18T11:13:00Z"}]' "$COMMENTS" > "$WORK/comments.next"
mv "$WORK/comments.next" "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/already.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "posted full-review command was not suppressed, rc=$rc: $(cat "$WORK/already.out")"
if grep -q '^RESULT=cr_full_review_required' "$WORK/already.out"; then fail "full-review command would be duplicated"; fi

echo "PASS watch-pr: full-review reply surfaced once"
