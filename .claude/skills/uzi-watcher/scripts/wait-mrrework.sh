#!/usr/bin/env bash
# Wait for uzi's mr_rework run on a PR through its fire->terminal lifecycle.
#
# mr_rework firing LAGS a settled CodeRabbit review by 30-40+ min on a busy instance
# (see SKILL.md "uzi may fix the CodeRabbit findings ITSELF"). This poller lets a driver
# DEFER to it deterministically instead of eyeballing a too-short wait and then doing a
# redundant local fix. Poll for the run to APPEAR, then to reach terminal.
#
# Usage: wait-mrrework.sh OWNER/REPO PR [max_ticks] [interval_s] [since_utc]
#   default budget: 45 ticks x 60s = 45 min (covers the observed ~40 min fire lag).
#   since_utc: an RFC3339-UTC instant (`date -u +%Y-%m-%dT%H:%M:%SZ`) captured BEFORE this
#     wait began. When set, only mr_rework runs created AFTER it count as "the current
#     cycle" — see the cycle-anchoring note below. Omit it and the wait is legacy (newest
#     run of ANY cycle), which can report a STALE prior cycle's terminal run as done.
#
# Exit codes:
#   0  a CURRENT-cycle mr_rework run reached terminal; prints MRREWORK_ID / MRREWORK_STATUS
#   2  budget elapsed with NO current-cycle mr_rework run seen AND the poll that ended the
#      budget SUCCEEDED (a confirmed empty result) — the ONLY exit that authorizes a local fix
#   3  budget elapsed while a run was still non-terminal (still reworking; re-run me)
#   4  the OWNER/REPO could not be resolved to a uzi repo id
#   5  budget elapsed but the polls were UNRELIABLE (every poll, or the final one,
#      failed — `uzi run list` errored or returned non-JSON), so "never fired" is NOT
#      established: do NOT fall back to a local fix on this; re-run or investigate.
#
# The exit-2-vs-5 split is load-bearing: a failed listing must never masquerade as a
# confirmed-empty "no run", or a transient `uzi`/network blip would greenlight a local
# fix while mr_rework is in fact mid-flight — recreating the double-push collision this
# poller exists to prevent. Callers must treat ONLY exit 2 as "no rework, fix locally";
# 3/4/5 all mean "do NOT fix locally yet".
set -uo pipefail

REPO_SLUG=${1:?usage: wait-mrrework.sh OWNER/REPO PR [max_ticks] [interval_s] [since_utc]}
PR=${2:?usage: wait-mrrework.sh OWNER/REPO PR [max_ticks] [interval_s] [since_utc]}
MAX=${3:-45}
INT=${4:-60}
SINCE=${5:-}   # RFC3339-UTC baseline; empty = legacy (no cycle anchoring)

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
  # Anchor on the NEWEST mr_rework run for this MR, not an arbitrary first match: an MR can
  # be reworked over several cycles (each a distinct run, up to the per-MR cap), so `first`
  # could latch onto a stale TERMINAL earlier cycle and exit 0 while the current cycle is
  # still running. `max_by(.created_at)` tracks the latest cycle — but "latest that EXISTS
  # right now" is still a PRIOR cycle's terminal run until the current one is queued, so a
  # bare max_by can report an old completed run as done before this cycle fires. SINCE fixes
  # that: when set, only runs created after it are considered, so a prior cycle's run is
  # excluded and the wait holds for the run this review actually triggers. Fractional seconds
  # are stripped so a plain `YYYY-MM-DDTHH:MM:SSZ` baseline compares lexicographically (same
  # format + UTC 'Z' => string order == time order).
  row=$(printf '%s' "$raw" | jq -r --arg repo "$REPO_ID" --argjson pr "$PR" --arg since "$SINCE" \
    '([.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr
        and ($since=="" or ((.created_at|sub("\\.[0-9]+";"")) > $since)))]
      | max_by(.created_at)) // {} | "\(.id // "")\t\(.status // "")"')
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
