#!/usr/bin/env bash
# Wait for uzi's mr_rework run on a PR through its fire->terminal lifecycle.
#
# mr_rework firing LAGS a settled CodeRabbit review by 30-40+ min on a busy instance
# (see SKILL.md "uzi may fix the CodeRabbit findings ITSELF"). This poller lets a driver
# DEFER to it deterministically instead of eyeballing a too-short wait and then doing a
# redundant local fix. Poll for the run to APPEAR, then to reach terminal.
#
# Usage: wait-mrrework.sh OWNER/REPO PR [max_ticks] [interval_s]
#   default budget: 45 ticks x 60s = 45 min (covers the observed ~40 min fire lag).
#
# Exit codes:
#   0  an mr_rework run reached terminal; prints MRREWORK_ID / MRREWORK_STATUS
#   2  budget elapsed with NO mr_rework run seen AND the poll that ended the budget
#      SUCCEEDED (a confirmed empty result) — safe to fall back to a local fix
#   3  budget elapsed while a run was still non-terminal (still reworking; re-run me)
#   4  the OWNER/REPO could not be resolved to a uzi repo id
#   5  budget elapsed but the polls were UNRELIABLE (every poll, or the final one,
#      failed — `uzi run list` errored or returned non-JSON), so "never fired" is NOT
#      established: do NOT fall back to a local fix on this; re-run or investigate.
#
# The exit-2-vs-5 split is load-bearing: a failed listing must never masquerade as a
# confirmed-empty "no run", or a transient `uzi`/network blip would greenlight a local
# fix while mr_rework is in fact mid-flight — recreating the double-push collision this
# poller exists to prevent.
set -uo pipefail

REPO_SLUG=${1:?usage: wait-mrrework.sh OWNER/REPO PR [max_ticks] [interval_s]}
PR=${2:?usage: wait-mrrework.sh OWNER/REPO PR [max_ticks] [interval_s]}
MAX=${3:-45}
INT=${4:-60}

REPO_ID=$(uzi repo list --json 2>/dev/null \
  | jq -r --arg s "$REPO_SLUG" 'first(.[]|select(.path_with_namespace==$s)|.id) // empty')
if [ -z "$REPO_ID" ]; then
  echo "could not resolve repo id for $REPO_SLUG from 'uzi repo list --json'" >&2
  exit 4
fi

seen_id=""
last_status=""
ok_polls=0        # count of polls where `uzi run list` returned valid JSON
last_poll_ok=0    # was the MOST RECENT poll a successful (valid-JSON) listing?
for i in $(seq 1 "$MAX"); do
  # Separate a command/parse FAILURE from a successful-but-empty listing: pipefail alone
  # can't, because the jq at the tail masks `uzi run list`'s exit status. Capture raw
  # output + rc, then validate it is JSON before trusting an "empty" result.
  raw=$(uzi run list --json 2>/dev/null); rc=$?
  if [ "$rc" -ne 0 ] || ! printf '%s' "$raw" | jq -e 'type == "array"' >/dev/null 2>&1; then
    last_poll_ok=0
    echo "$(date +%H:%M:%S) tick $i/$MAX: poll FAILED (uzi run list rc=$rc or non-array JSON) — NOT counted as 'no run'"
    sleep "$INT"; continue
  fi
  ok_polls=$((ok_polls + 1)); last_poll_ok=1
  row=$(printf '%s' "$raw" | jq -r --arg repo "$REPO_ID" --argjson pr "$PR" \
    'first(.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr)) // {} | "\(.id // "")\t\(.status // "")"')
  id=$(printf '%s' "$row" | cut -f1)
  st=$(printf '%s' "$row" | cut -f2)
  if [ -z "$id" ]; then
    echo "$(date +%H:%M:%S) tick $i/$MAX: no mr_rework run yet (waiting to fire)"
  else
    seen_id="$id"; last_status="$st"
    echo "$(date +%H:%M:%S) tick $i/$MAX: mr_rework $id status=$st"
    case "$st" in
      completed|failed|cancelled)
        echo "MRREWORK_ID=$id"
        echo "MRREWORK_STATUS=$st"
        exit 0 ;;
    esac
  fi
  sleep "$INT"
done

if [ -n "$seen_id" ]; then
  echo "BUDGET_ELAPSED: mr_rework $seen_id still $last_status (re-run to keep waiting)"
  exit 3
fi
# No run ever seen. Only call it "never fired" if the LAST poll was a confirmed empty
# listing; if polls were failing (esp. at the end), "never fired" is not established.
if [ "$ok_polls" -eq 0 ] || [ "$last_poll_ok" -ne 1 ]; then
  echo "BUDGET_ELAPSED: polls UNRELIABLE ($ok_polls ok, last_poll_ok=$last_poll_ok) — 'never fired' NOT established; do NOT fall back to a local fix on this"
  exit 5
fi
echo "BUDGET_ELAPSED: no mr_rework run appeared for $REPO_SLUG#$PR across $ok_polls confirmed polls — fall back to a local fix"
exit 2
