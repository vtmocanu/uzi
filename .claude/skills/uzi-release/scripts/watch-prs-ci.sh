#!/usr/bin/env bash
# watch-prs-ci.sh — poll SEVERAL PRs' checks at JOB level at once, surfacing a
# failed job the moment it appears on ANY of them, and exiting green only when they
# are ALL terminal and none failed.
#
# This is the batch companion to watch-pr-ci.sh: the uzi-release skill's step 1/3
# routinely has to watch a whole merge batch (e.g. several PRs re-running CI after a
# repo-wide-red fix on main), and a single-PR watcher forces either N background
# tasks or a hand-rolled loop — the latter is how a fragile poll loop gets written
# under time pressure. This wraps the exact same awk classification watch-pr-ci.sh
# uses (same field parsing, same fail/pending/terminal sets), extended across PRs.
# It is a STARTING POINT: keep improving it as the CI surface changes (new jobs, new
# terminal states, CodeRabbit behavior).
#
# Usage:
#   watch-prs-ci.sh <PR> [<PR>...] [--interval SECS] [--max-ticks N] [--wait-cr]
#
#   <PR>...         one or more PR numbers (repo inferred by gh from the checkout).
#   --interval      seconds between polls (default 120; CI jobs are minutes-long,
#                   so a tighter cadence just burns API calls).
#   --max-ticks     give up after N polls (default 40 → ~80 min at 120s).
#   --wait-cr       also wait for each PR's CodeRabbit check to reach a terminal
#                   state (by default CI-only; CodeRabbit is assessed separately).
#
# Run it in the BACKGROUND (the harness re-invokes you when it exits); do not block
# a foreground turn on it.
#
# Exit codes (same contract as watch-pr-ci.sh, aggregated over the PR set):
#   0  every PR terminal and none failed (whole batch green)
#   1  a failed/cancelled job was detected on some PR (that PR + its failing jobs are
#      printed) — react now; a batch cannot merge on a red member
#   2  timed out: at least one PR still pending after --max-ticks
#   3  usage / gh error
#
# Design notes:
#  - Parses `gh pr checks` with awk -F'\t' (this host's grep is ugrep, whose POSIX
#    modes mishandle negated classes and brace intervals — see repo CLAUDE.md — so
#    load-bearing field parsing uses awk, never a grep pattern). Aggregation across
#    PRs is likewise done in awk/shell without arithmetic on parsed strings and
#    without gawk-only extensions (no 3-arg match — BSD awk on macOS lacks it), the
#    two traps that bite a hand-rolled multi-PR loop.
#  - Fields: $1=name $2=state $3=elapsed $4=url. States seen: pass, fail, pending,
#    skipping. Treated as FAILURE: fail failure cancelled timed_out action_required.
#    NON-TERMINAL: pending in_progress queued waiting. OK-TERMINAL: pass skipping
#    neutral success.
#  - Early-exit on the FIRST confirmed failure on ANY PR is the whole point: one red
#    job means the batch cannot go green, so there is nothing to gain by waiting for
#    the rest. The failure is CONFIRMED with one immediate re-query of that PR before
#    exiting, because a freshly-started run can briefly report a stale non-success
#    conclusion on a job that is actually still in_progress (skill step 4's
#    stale-first-tick caveat).
set -uo pipefail

PRS=(); INTERVAL=120; MAX_TICKS=40; WAIT_CR=0
while [ $# -gt 0 ]; do
  case "$1" in
    --interval) INTERVAL="${2:?}"; shift 2;;
    --max-ticks) MAX_TICKS="${2:?}"; shift 2;;
    --wait-cr) WAIT_CR=1; shift;;
    -h|--help) sed -n '2,40p' "$0"; exit 3;;
    -*) echo "unknown flag: $1" >&2; exit 3;;
    *) PRS+=("$1"); shift;;
  esac
done
[ "${#PRS[@]}" -gt 0 ] || { echo "usage: watch-prs-ci.sh <PR> [<PR>...] [--interval S] [--max-ticks N] [--wait-cr]" >&2; exit 3; }

# Classify one `gh pr checks` dump. Prints one of: FAIL / PENDING / GREEN, followed
# by the failing rows (name<TAB>url) when FAIL. Reads the dump on stdin. Identical
# to watch-pr-ci.sh's classify so the two scripts cannot drift in what they count.
classify() {
  awk -F'\t' -v wait_cr="$WAIT_CR" '
    function isfail(s){ return s=="fail"||s=="failure"||s=="cancelled"||s=="timed_out"||s=="action_required" }
    function ispend(s){ return s=="pending"||s=="in_progress"||s=="queued"||s=="waiting" }
    {
      name=$1; state=$2; url=$4
      if (name=="CodeRabbit" && !wait_cr) next   # CR assessed separately unless --wait-cr
      if (isfail(state)) { fails[nf++]=name "\t" url }
      else if (ispend(state)) pend++
    }
    END {
      if (nf>0){ print "FAIL"; for(i=0;i<nf;i++) print fails[i] }
      else if (pend>0) print "PENDING"
      else print "GREEN"
    }'
}

# Verdict for one PR ("FAIL" | "PENDING" | "GREEN" | "EMPTY"), printed on line 1,
# any failing rows following. EMPTY = gh returned nothing (transient), treated as
# non-terminal for aggregation so a blip does not end the watch.
pr_verdict() {
  local out; out="$(gh pr checks "$1" 2>/dev/null)"
  if [ -z "$out" ]; then echo "EMPTY"; return; fi
  printf '%s\n' "$out" | classify
}

tick=0
while [ "$tick" -lt "$MAX_TICKS" ]; do
  all_green=1; line=""
  for pr in "${PRS[@]}"; do
    verdict="$(pr_verdict "$pr")"
    head="$(printf '%s\n' "$verdict" | head -1)"
    case "$head" in
      FAIL)
        # Confirm with one immediate re-query to shake off a stale first-tick read.
        verdict2="$(pr_verdict "$pr")"
        if [ "$(printf '%s\n' "$verdict2" | head -1)" = "FAIL" ]; then
          echo "=== #$pr: FAILED JOB(S) after ~$((tick*INTERVAL))s — react now (read the log: gh run view --job <id> --log-failed) ==="
          printf '%s\n' "$verdict2" | tail -n +2 | awk -F'\t' '{printf "  FAIL  #'"$pr"'  %-28s %s\n",$1,$2}'
          exit 1
        fi
        echo "[tick $tick] #$pr: a fail cleared on re-query (stale read); continuing"
        all_green=0; line="$line #$pr:pending(re-query)"
        ;;
      GREEN)   line="$line #$pr:green" ;;
      PENDING) all_green=0; line="$line #$pr:pending" ;;
      EMPTY)   all_green=0; line="$line #$pr:empty" ;;
    esac
  done

  if [ "$all_green" = "1" ]; then
    echo "=== batch GREEN: all of ${PRS[*]} terminal, none failed after ~$((tick*INTERVAL))s ==="
    exit 0
  fi
  echo "[tick $tick]$line"
  sleep "$INTERVAL"; tick=$((tick+1))
done

echo "=== batch still pending after ~$((MAX_TICKS*INTERVAL))s (max-ticks reached):$line ==="
exit 2
