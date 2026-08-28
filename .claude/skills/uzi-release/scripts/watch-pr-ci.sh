#!/usr/bin/env bash
# watch-pr-ci.sh — poll a PR's checks at JOB level and surface a failed job the
# moment it appears, instead of waiting for the whole CI run to conclude.
#
# This is the executable form of the uzi-release skill's step 4 ("Watch CI at JOB
# level — act on a failed job immediately"). It is a STARTING POINT: keep improving
# it as the CI surface changes (new jobs, new terminal states, CodeRabbit behavior).
#
# Usage:
#   watch-pr-ci.sh <PR> [--interval SECS] [--max-ticks N] [--wait-cr]
#
#   <PR>            PR number (repo inferred by gh from the checkout).
#   --interval      seconds between polls (default 120; CI jobs are minutes-long,
#                   so a tighter cadence just burns API calls).
#   --max-ticks     give up after N polls (default 40 → ~80 min at 120s).
#   --wait-cr       also wait for the CodeRabbit check to reach a terminal state
#                   (by default CI-only; CodeRabbit is assessed separately).
#
# Run it in the BACKGROUND (the harness re-invokes you when it exits); do not block
# a foreground turn on it.
#
# Exit codes:
#   0  all checks terminal and none failed (green)
#   1  a failed/cancelled job was detected (its name + URL are printed) — react now
#   2  timed out: still pending after --max-ticks
#   3  usage / gh error
#
# Design notes:
#  - Parses `gh pr checks` with awk -F'\t' (this host's grep is ugrep, whose POSIX
#    modes mishandle negated classes and brace intervals — see repo CLAUDE.md — so
#    load-bearing field parsing uses awk, never a grep pattern).
#  - Fields: $1=name $2=state $3=elapsed $4=url. States seen: pass, fail, pending,
#    skipping. Treated as FAILURE: fail failure cancelled timed_out action_required.
#    NON-TERMINAL: pending in_progress queued waiting. OK-TERMINAL: pass skipping
#    neutral success.
#  - Early-exit on the first failure is the whole point: one red job means the run
#    cannot go green, so there is nothing to gain by waiting for the rest.
#  - A failure is CONFIRMED with one immediate re-query before exiting, because a
#    freshly-started run can briefly report a stale non-success conclusion on a job
#    that is actually still in_progress (skill step 4's stale-first-tick caveat).
set -uo pipefail

PR=""; INTERVAL=120; MAX_TICKS=40; WAIT_CR=0
while [ $# -gt 0 ]; do
  case "$1" in
    --interval) INTERVAL="${2:?}"; shift 2;;
    --max-ticks) MAX_TICKS="${2:?}"; shift 2;;
    --wait-cr) WAIT_CR=1; shift;;
    -h|--help) sed -n '2,30p' "$0"; exit 3;;
    *)
      if [ -z "$PR" ]; then PR="$1"; shift
      else echo "unexpected arg: $1" >&2; exit 3
      fi
      ;;
  esac
done
[ -n "$PR" ] || { echo "usage: watch-pr-ci.sh <PR> [--interval S] [--max-ticks N] [--wait-cr]" >&2; exit 3; }

# Classify one `gh pr checks` dump. Prints one of: FAIL / PENDING / GREEN, followed
# by the failing rows (name<TAB>url) when FAIL. Reads the dump on stdin.
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

tick=0
while [ "$tick" -lt "$MAX_TICKS" ]; do
  out="$(gh pr checks "$PR" 2>/dev/null)"
  if [ -z "$out" ]; then echo "[tick $tick] gh returned nothing (transient?); retrying"; sleep "$INTERVAL"; tick=$((tick+1)); continue; fi

  verdict="$(printf '%s\n' "$out" | classify)"
  head="$(printf '%s\n' "$verdict" | head -1)"

  case "$head" in
    FAIL)
      # Confirm with one immediate re-query to shake off a stale first-tick read.
      out2="$(gh pr checks "$PR" 2>/dev/null)"
      verdict2="$(printf '%s\n' "$out2" | classify)"
      if [ "$(printf '%s\n' "$verdict2" | head -1)" = "FAIL" ]; then
        echo "=== #$PR: FAILED JOB(S) after $((tick*INTERVAL))s — react now (read the log: gh run view --job <id> --log-failed) ==="
        printf '%s\n' "$verdict2" | tail -n +2 | awk -F'\t' '{printf "  FAIL  %-28s %s\n",$1,$2}'
        exit 1
      fi
      echo "[tick $tick] a fail cleared on re-query (stale read); continuing"
      ;;
    GREEN)
      echo "=== #$PR: all checks terminal, none failed after $((tick*INTERVAL))s ==="
      printf '%s\n' "$out" | awk -F'\t' '{c[$2]++} END{for(s in c) printf "  %-10s %d\n",s,c[s]}'
      exit 0
      ;;
    PENDING) : ;;  # keep waiting
  esac

  sleep "$INTERVAL"; tick=$((tick+1))
done

echo "=== #$PR: still pending after $((MAX_TICKS*INTERVAL))s (max-ticks reached) ==="
exit 2
