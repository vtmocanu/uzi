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
      case "$MODE" in
        pending_findings) echo '{"statuses":[{"context":"CodeRabbit","description":"Review in progress"}]}' ;;
        greptile_clean) echo '{"statuses":[]}' ;;
        *) echo '{"statuses":[{"context":"CodeRabbit","description":"Review completed"}]}' ;;
      esac ;;
    *'/pulls/42/reviews'*)
      if [ "$MODE" = pending_findings ]; then
        echo '[{"id":1,"user":{"login":"coderabbitai[bot]"},"commit_id":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","state":"APPROVED","body":""}]'
      else echo '[]'; fi ;;
    *'/issues/42/comments'*) cat "$COMMENTS" ;;
    *'/pulls/42/comments'*)
      case "$MODE" in
        pending_findings) echo '[{"user":{"login":"coderabbitai[bot]"},"line":7,"body":"🟡 **partial finding**","pull_request_review_id":1}]' ;;
        greptile_clean) echo '[{"user":{"login":"greptile-apps[bot]"},"line":8,"body":"<img alt=\"P1\"> old addressed finding","pull_request_review_id":99}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/commits/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/check-runs'*)
      if [ "$MODE" = greptile_clean ]; then
        echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\n\n90 files reviewed, 0 comments added"}}]}'
      else echo '{"check_runs":[]}'; fi ;;
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
MODE="full"; export MODE

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

# Findings are incomplete while a selected reviewer is still in progress; wait, do not edit.
MODE="pending_findings"; export MODE
printf '[]\n' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer coderabbit --reviewer-grace 0 > "$WORK/pending.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "in-progress review surfaced partial findings, rc=$rc: $(cat "$WORK/pending.out")"
if grep -q '^RESULT=findings' "$WORK/pending.out"; then fail "partial findings became actionable"; fi

# A current-head clean Greptile check is authoritative over old still-anchored comments.
MODE="greptile_clean"; export MODE
set +e
bash "$SCRIPT" test/repo 42 0 1 --reviewer greptile --reviewer-grace 0 > "$WORK/greptile.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "clean current Greptile review counted an old finding, rc=$rc: $(cat "$WORK/greptile.out")"
grep -q '^RESULT=ready$' "$WORK/greptile.out" || fail "clean Greptile review did not reach ready"

echo "PASS watch-pr: review replies, in-progress findings, current Greptile scope"
