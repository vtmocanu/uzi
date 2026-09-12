#!/usr/bin/env bash
# plan-gate-fixture.sh — capture the run-plan-gate shot deterministically by driving a
# throwaway uzi run to the plan-approval gate, shooting it, then cleaning up. Uses a
# PERMANENT, labelled fixture issue kept CLOSED (so sweeps ignore it): reopen -> run to
# the gate -> capture both themes -> cancel the run -> close the issue. A trap runs the
# cleanup even on failure, so nothing is left active. It never approves the plan, so no
# branch/MR is created. Spends one small (~1-3 min) billed planning run.
#
# Usage: plan-gate-fixture.sh --repo <uzi-repo-id> [--gh-repo vtmocanu/uzi] [--stage <dir>] [--url <url>]
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"

URL=""; REPO=""; GHREPO="vtmocanu/uzi"; STAGE=""; APPROVE=0; MS_WAIT="150"
while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="${2:-}"; shift 2 ;;
    --repo) REPO="${2:-}"; shift 2 ;;
    --gh-repo) GHREPO="${2:-}"; shift 2 ;;
    --stage) STAGE="${2:-}"; shift 2 ;;
    --approve) APPROVE=1; shift ;;             # also approve + capture the milestones checklist mid-progress
    --ms-wait) MS_WAIT="${2:-}"; shift 2 ;;    # seconds after approve before capturing (M1 done, M2 in progress)
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done
[ -n "$URL" ] || URL="$(uzi auth status 2>/dev/null | awk '/^URL/{print $2}')"
[ -n "$REPO" ] || { echo "--repo <uzi-repo-id> required (see 'uzi repo list')" >&2; exit 2; }
[ -n "$STAGE" ] || STAGE="${TMPDIR:-/tmp}/uzi-ui-shots"
LABEL="screenshot-fixture"

# 1) ensure the fixture label + a permanent fixture issue exist, with a 3-milestone
# body so the PROPOSED PLAN reads as a real multi-milestone plan (not a no-op). Each
# milestone is a ~2-minute no-op (`sleep 120`) so nothing is built even if approved.
BODYFILE="$(mktemp -t uzi-fixture-body.XXXXXX)"  # removed by cleanup() on EXIT
cat > "$BODYFILE" <<'BODY'
# Demo PRD — uzi UI screenshot fixture (do not merge)

Tiny demo PRD used only to capture uzi UI screenshots (the plan-approval gate).
Three trivial milestones, each a ~2-minute no-op (`sleep 120`), so the proposed plan
reads as a real 3-milestone plan. The screenshot tool runs this to the gate,
captures, then cancels and closes it. **Do not approve or merge.**

## Milestones

1. **M1 — warm-up:** run `sleep 120`, then report the milestone complete. Touch no files.
2. **M2 — steady-state:** run `sleep 120`, then report the milestone complete. Touch no files.
3. **M3 — cool-down:** run `sleep 120`, then report the milestone complete. Touch no files.

No code changes, no commits, no PRD files — each milestone is only `sleep 120`.
BODY

gh label create "$LABEL" --repo "$GHREPO" --color BFD4F2 --description "Permanent uzi UI screenshot fixture (kept closed; reopened only to capture)" >/dev/null 2>&1 || true
IID="$(gh issue list --repo "$GHREPO" --label "$LABEL" --state all --limit 1 --json number -q '.[0].number' 2>/dev/null || true)"
if [ -z "$IID" ]; then
  echo "creating the permanent fixture issue…"
  IID="$(gh issue create --repo "$GHREPO" --label "$LABEL" --label uzi \
    --title "Screenshot fixture: do not merge (uzi UI capture)" --body-file "$BODYFILE" \
    --json number -q .number 2>/dev/null || gh issue create --repo "$GHREPO" --label "$LABEL" --label uzi --title "Screenshot fixture: do not merge (uzi UI capture)" --body-file "$BODYFILE" | grep -oE '[0-9]+$')"
else
  gh issue edit "$IID" --repo "$GHREPO" --body-file "$BODYFILE" >/dev/null 2>&1 || true # keep the 3-milestone body current
fi
echo "fixture issue: #$IID"

RUN_ID=""
cleanup() {
  echo "cleanup…"
  rm -f "$BODYFILE" 2>/dev/null || true
  [ -n "$RUN_ID" ] && uzi run cancel "$RUN_ID" -m "screenshot fixture done" >/dev/null 2>&1 || true
  # if the plan was approved, agents may have opened a branch/MR — close + delete it.
  for n in $(gh pr list --repo "$GHREPO" --head "agent/issue-$IID" --state all --json number -q '.[].number' 2>/dev/null); do
    gh pr close "$n" --repo "$GHREPO" --delete-branch >/dev/null 2>&1 || true
  done
  gh api -X DELETE "repos/$GHREPO/git/refs/heads/agent/issue-$IID" >/dev/null 2>&1 || true
  gh issue close "$IID" --repo "$GHREPO" >/dev/null 2>&1 || true
  echo "cleaned up: run cancelled, any branch/MR removed, issue #$IID closed."
}
trap cleanup EXIT

# 2) reopen + start a run; it plans, then parks at the approval gate.
gh issue reopen "$IID" --repo "$GHREPO" >/dev/null 2>&1 || true
echo "starting run…"
# A brand-new issue can lag the forge->uzi sync, so `create` may 404 the issue for a
# few seconds; retry. Parse the run id from the ID row of the (non-json) table.
RUN_ID=""
for attempt in $(seq 1 18); do
  OUT="$(uzi run create --repo "$REPO" --issue "$IID" --force 2>&1 || true)"
  RUN_ID="$(printf '%s\n' "$OUT" | awk '/^ID/{print $2; exit}')"
  [ -n "$RUN_ID" ] && break
  echo "  create not ready (attempt $attempt): $(printf '%s' "$OUT" | head -1)"
  sleep 5
done
[ -n "$RUN_ID" ] || { echo "could not start run after retries" >&2; exit 4; }
echo "run: $RUN_ID"

# 3) poll until the plan gate (awaiting_approval), up to ~6 min.
echo "waiting for the plan gate…"
for _ in $(seq 1 72); do
  ST="$(uzi run get "$RUN_ID" --json 2>/dev/null | grep -oE '"status"[: ]+"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"
  echo "  status: ${ST:-?}"
  [ "$ST" = "awaiting_approval" ] && break
  case "$ST" in failed|completed|cancelled) echo "run ended as '$ST' before parking" >&2; exit 5 ;; esac
  sleep 5
done
[ "$ST" = "awaiting_approval" ] || { echo "did not reach the plan gate in time (status=$ST)" >&2; exit 5; }

# 4) capture the plan-gate shot (both themes) against the parked run.
"$DIR/run.sh" --url "$URL" --stage "$STAGE" --run "$RUN_ID" --only run-plan-gate
echo "plan-gate captured to $STAGE."

# 5) optionally approve the plan + capture the milestones checklist MID-PROGRESS
# (M1 done/struck, M2 in progress, M3 pending). The sleep-120 milestones make the
# timing predictable; the cleanup trap still cancels the run + removes any branch/MR.
if [ "$APPROVE" = "1" ]; then
  echo "approving plan…"
  uzi run approve "$RUN_ID" >/dev/null 2>&1 || uzi run approve "$RUN_ID" || echo "approve failed" >&2
  echo "waiting ${MS_WAIT}s for M1 to finish (M1 done → M2 in progress)…"
  sleep "$MS_WAIT"
  ST2="$(uzi run get "$RUN_ID" --json 2>/dev/null | grep -oE '"status"[: ]+"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"
  echo "  status: ${ST2:-?}"
  "$DIR/run.sh" --url "$URL" --stage "$STAGE" --run "$RUN_ID" --only milestones
  echo "milestones captured to $STAGE."
fi
echo "(cleanup trap will cancel the run, remove any branch/MR, and close the issue)"
