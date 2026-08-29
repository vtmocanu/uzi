#!/usr/bin/env bash
# watch-run-ci.sh — poll a workflow RUN (main's ci.yml after a merge or the release
# commit) at JOB level and surface a failed job the moment it appears, instead of
# waiting for the whole run to conclude.
#
# This is the run-id/branch analog of watch-pr-ci.sh — the executable form of the
# uzi-release skill's step 4 ("Watch CI at JOB level — act on a failed job
# immediately, do NOT wait for the whole run") for the case that watch-pr-ci.sh does
# NOT cover: a push to a branch (main) rather than a PR. Polling the whole-run
# `status` and reacting only at `completed` is the bug this replaces — a long job
# (test-api-store-it) keeps the run in_progress for minutes after a fast job
# (validate-api) has already gone red, so a whole-run wait learns of the failure late.
# It is a STARTING POINT: keep improving it as the CI surface changes (new jobs,
# terminal states).
#
# Usage:
#   watch-run-ci.sh <run-id> [--interval SECS] [--max-ticks N]
#   watch-run-ci.sh --branch main [--workflow ci.yml] [--interval SECS] [--max-ticks N]
#
#   <run-id>       numeric run id (repo inferred by gh from the checkout).
#   --branch       resolve the LATEST run on this branch (needs --workflow to be
#                  unambiguous; defaults to ci.yml). Re-resolves each tick so a
#                  newer run from a concurrent push is picked up.
#   --workflow     workflow file name for --branch resolution (default ci.yml).
#   --interval     seconds between polls (default 120; CI jobs are minutes-long, so a
#                  tighter cadence just burns API calls).
#   --max-ticks    give up after N polls (default 40 -> ~80 min at 120s).
#
# Run it in the BACKGROUND (the harness re-invokes you when it exits); do not block a
# foreground turn on it.
#
# Exit codes:
#   0  all jobs terminal and none failed (green)
#   1  a failed/cancelled job was detected (its name + url printed) — react now
#   2  timed out: still pending after --max-ticks
#   3  usage / gh error (no run found, gh failure)
#
# Design notes:
#  - Parses `gh run view --json jobs` with jq, then classifies with awk -F'\t' (this
#    host's grep is ugrep, whose POSIX modes mishandle negated classes and brace
#    intervals — see repo CLAUDE.md — so load-bearing field parsing uses awk, never a
#    grep pattern).
#  - A job is FAILURE only when status==completed AND conclusion is a failure state.
#    That is deliberate: `gh run view --json jobs` can briefly report an in_progress
#    job with a non-null (often failure/cancelled) conclusion right after a run starts
#    (skill step 4's stale-first-tick caveat), so gating on status==completed filters
#    that, and a failure is still CONFIRMED with one immediate re-query before exiting.
#  - FAILURE conclusions: failure cancelled timed_out action_required startup_failure.
#    OK-TERMINAL: success skipping neutral (path-filtered build-* jobs report skipped).
#    NON-TERMINAL: any job whose status != completed.
#  - Early-exit on the first failure is the whole point: one red job means the run
#    cannot go green, so there is nothing to gain by waiting for the rest.
set -uo pipefail

RUN=""; BRANCH=""; WORKFLOW="ci.yml"; INTERVAL=120; MAX_TICKS=40
while [ $# -gt 0 ]; do
  case "$1" in
    --branch) BRANCH="${2:?}"; shift 2;;
    --workflow) WORKFLOW="${2:?}"; shift 2;;
    --interval) INTERVAL="${2:?}"; shift 2;;
    --max-ticks) MAX_TICKS="${2:?}"; shift 2;;
    -h|--help) sed -n '2,40p' "$0"; exit 3;;
    *)
      if [ -z "$RUN" ]; then RUN="$1"; shift
      else echo "unexpected arg: $1" >&2; exit 3
      fi
      ;;
  esac
done
if [ -z "$RUN" ] && [ -z "$BRANCH" ]; then
  echo "usage: watch-run-ci.sh <run-id> | --branch <name> [--workflow ci.yml] [--interval S] [--max-ticks N]" >&2
  exit 3
fi

# Resolve the latest run id on a branch (used when --branch given and RUN not fixed).
resolve_run() {
  gh run list --branch "$BRANCH" --workflow "$WORKFLOW" --limit 1 \
    --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null
}

# Classify one run's jobs (jq TSV on stdin: status<TAB>conclusion<TAB>name<TAB>url).
# Prints FAIL / PENDING / GREEN, then the failing rows (name<TAB>url) when FAIL.
classify() {
  awk -F'\t' '
    function isfail(c){ return c=="failure"||c=="cancelled"||c=="timed_out"||c=="action_required"||c=="startup_failure" }
    {
      status=$1; concl=$2; name=$3; url=$4
      if (status!="completed") { pend++; next }        # non-terminal job
      if (isfail(concl)) { fails[nf++]=name "\t" url }  # completed AND failed
    }
    END {
      if (nf>0){ print "FAIL"; for(i=0;i<nf;i++) print fails[i] }
      else if (pend>0) print "PENDING"
      else print "GREEN"
    }'
}

# Dump one run as jq TSV; empty output signals a gh/run-not-found error to the caller.
dump() {
  gh run view "$1" --json jobs \
    --jq '.jobs[] | [.status, (.conclusion // ""), .name, .url] | @tsv' 2>/dev/null
}

tick=0
while [ "$tick" -lt "$MAX_TICKS" ]; do
  # With --branch and no fixed run id, re-resolve each tick (a concurrent push mints a
  # newer run; a rerun keeps the same id). With an explicit run id, keep it.
  cur="$RUN"
  if [ -z "$cur" ] || [ -n "$BRANCH" ]; then
    r="$(resolve_run)"; [ -n "$r" ] && cur="$r"
  fi
  if [ -z "$cur" ]; then echo "[tick $tick] no run found for branch=$BRANCH workflow=$WORKFLOW; retrying"; sleep "$INTERVAL"; tick=$((tick+1)); continue; fi

  out="$(dump "$cur")"
  if [ -z "$out" ]; then echo "[tick $tick] gh returned no jobs for run $cur (transient?); retrying"; sleep "$INTERVAL"; tick=$((tick+1)); continue; fi

  verdict="$(printf '%s\n' "$out" | classify)"
  case "$(printf '%s\n' "$verdict" | head -1)" in
    FAIL)
      # Confirm with one immediate re-query to shake off a stale first-tick read.
      out2="$(dump "$cur")"; verdict2="$(printf '%s\n' "$out2" | classify)"
      if [ "$(printf '%s\n' "$verdict2" | head -1)" = "FAIL" ]; then
        echo "=== run $cur: FAILED JOB(S) after $((tick*INTERVAL))s — react now (read the log: gh run view --job <id> --log-failed) ==="
        printf '%s\n' "$verdict2" | tail -n +2 | awk -F'\t' '{printf "  FAIL  %-28s %s\n",$1,$2}'
        exit 1
      fi
      echo "[tick $tick] a fail cleared on re-query (stale read); continuing"
      ;;
    GREEN)
      echo "=== run $cur: all jobs terminal, none failed after $((tick*INTERVAL))s ==="
      exit 0
      ;;
    PENDING) : ;;  # keep waiting
  esac

  sleep "$INTERVAL"; tick=$((tick+1))
done

echo "=== run ${RUN:-branch:$BRANCH}: still pending after $((MAX_TICKS*INTERVAL))s (max-ticks reached) ==="
exit 2
