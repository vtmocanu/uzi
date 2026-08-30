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
#   2  budget elapsed with NO mr_rework run ever seen (fall back to a local fix)
#   3  budget elapsed while a run was still non-terminal (still reworking; re-run me)
#   4  the OWNER/REPO could not be resolved to a uzi repo id
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
for i in $(seq 1 "$MAX"); do
  row=$(uzi run list --json 2>/dev/null | jq -r \
    --arg repo "$REPO_ID" --argjson pr "$PR" \
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
echo "BUDGET_ELAPSED: no mr_rework run appeared for $REPO_SLUG#$PR — fall back to a local fix"
exit 2
