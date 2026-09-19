#!/usr/bin/env bash
# Hermetic regression: Greptile's newest EARLIER verdict scopes finding liveness on a head
# it never reviewed, and every unreadable or unscopable answer fails closed.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=greptile-verdict.sh
. "$HERE/greptile-verdict.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

HEAD_SHA=cccccccccccccccccccccccccccccccccccccccc
PREV_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
OLD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
HEAD_SHA=cccccccccccccccccccccccccccccccccccccccc
PREV_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
OLD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
review() { printf '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"success","output":{"summary":"Greptile has reviewed the Pull Request.\\n\\n90 files reviewed, %s comments added"}}]}\n' "$1"; }
notreview() { echo '{"check_runs":[{"app":{"slug":"greptile-apps"},"name":"Greptile Review","status":"completed","conclusion":"failure","output":{"summary":"90 files reviewed, 0 comments added"}}]}'; }
nothing() { echo '{"check_runs":[{"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","output":{"summary":""}}]}'; }
[ "${1:-}" = api ] || { echo "unexpected gh call: $*" >&2; exit 1; }
case "$*" in
  *'/pulls/42/commits'*)
    [ "$MODE" = commits_fail ] && exit 1
    # Oldest first, as the API returns them; the head is last.
    printf '[{"sha":"%s"},{"sha":"%s"},{"sha":"%s"}]\n' "$OLD_SHA" "$PREV_SHA" "$HEAD_SHA" ;;
  *"/commits/$HEAD_SHA/check-runs"*)
    # The helper answers for EARLIER commits only; the callers own the head.
    echo "helper queried the head's check-runs" >&2; exit 1 ;;
  *"/commits/$PREV_SHA/check-runs"*)
    case "$MODE" in
      prior_clean) review 0 ;;
      prior_findings) review 1 ;;
      unscoped) review 2 ;;
      skips_nonreview) notreview ;;
      garbage) echo '[]' ;;
      *) nothing ;;
    esac ;;
  *"/commits/$OLD_SHA/check-runs"*)
    case "$MODE" in
      skips_nonreview|window) review 0 ;;
      *) nothing ;;
    esac ;;
  *'/pulls/42/reviews'*)
    case "$MODE" in
      prior_findings) printf '[{"id":44,"user":{"login":"greptile-apps[bot]"},"commit_id":"%s"},{"id":55,"user":{"login":"greptile-apps[bot]"},"commit_id":"%s"}]\n' "$OLD_SHA" "$PREV_SHA" ;;
      *) echo '[]' ;;
    esac ;;
  *) echo "unexpected gh api: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH"

# want RC SHA ADDED REVIEW_ID — runs the helper under $MODE and compares all four.
want() {
  local rc=0
  greptile_prior_verdict test/repo 42 "$HEAD_SHA" || rc=$?
  [ "$rc" -eq "$1" ] || fail "$MODE: rc=$rc, want $1"
  [ "$GRV_SHA" = "$2" ] || fail "$MODE: GRV_SHA='$GRV_SHA', want '$2'"
  [ "$GRV_ADDED" = "$3" ] || fail "$MODE: GRV_ADDED='$GRV_ADDED', want '$3'"
  [ "$GRV_REVIEW_ID" = "$4" ] || fail "$MODE: GRV_REVIEW_ID='$GRV_REVIEW_ID', want '$4'"
}

# The PR #1449 shape: fixed finding, clean re-review on PREV, then a push Greptile never saw.
MODE=prior_clean; export MODE
want 0 "$PREV_SHA" 0 ""

# The earlier verdict added comments: they are scoped to ITS review id, not an older one.
MODE=prior_findings; export MODE
want 0 "$PREV_SHA" 1 55

# A completed check that is not a review (conclusion failure, even carrying a summary) is
# skipped, not trusted.
MODE=skips_nonreview; export MODE
want 0 "$OLD_SHA" 0 ""

# No earlier verdict at all: a trustworthy "none", so callers keep their raw count.
MODE=none; export MODE
want 0 "" "" ""

# The walk is bounded: a verdict outside the window is not found, and that is rc 0.
MODE=window; export MODE
GREPTILE_PRIOR_MAX=1 want 0 "" "" ""
want 0 "$OLD_SHA" 0 ""

# Fail closed: an unreadable commit list, an unreadable check-run page, and a verdict that
# added comments with no review object to scope them by.
MODE=commits_fail; export MODE
want 1 "" "" ""
MODE=garbage; export MODE
want 1 "" "" ""
MODE=unscoped; export MODE
want 1 "$PREV_SHA" 2 ""

echo "PASS greptile-verdict: earlier verdict scoped, non-reviews skipped, unreadable fails closed"
