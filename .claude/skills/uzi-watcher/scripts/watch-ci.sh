#!/usr/bin/env bash
# Poll GitHub Actions runs for one commit until they all settle, then classify
# the result the way this skill's "Post-merge CI" section prescribes. Designed to
# be launched with Claude Code's run_in_background so the harness re-invokes the
# model when it exits (a killed poll simply re-fires; see watch-run.sh for why a
# short poll loop beats a long-lived `gh run watch`).
#
# It exists so a CI watch is one maintained tool, not an ad-hoc heredoc rewritten
# per merge (which is how a path typo crept in on 2026-08-23).
#
# Usage: watch-ci.sh <sha> [branch] [interval-secs] [max-polls]
#   <sha>      full or prefix commit SHA to match (headSha startswith)
#   branch     branch to list runs on (default: main)
#   interval   seconds between polls (default: 60)
#   max-polls  give up after this many (default: 60 → ~1h at the default interval)
#
# Exit codes (branch on these, not on the text):
#   0  every run for the SHA concluded green — `success`, or the values GitHub treats
#      as passing for a completed check (`skipped` / `neutral`)
#   1  at least one run concluded `failure` / `timed_out` / `startup_failure` (REAL red)
#   2  runs concluded only `cancelled` (concurrency supersession, NOT a failure):
#      a newer commit superseded this one — `git fetch origin <branch>` and
#      re-watch the CURRENT HEAD, whose run exercises this change plus the newer one
#   3  no run ever appeared, they never settled within max-polls, OR a run concluded a
#      value that is neither green, red, nor cancelled (`action_required` / `stale`):
#      fail closed and inspect, never merge on it. (Exit 0 means every run that
#      EXISTS is green, NOT that the full expected workflow set ran — a partial
#      dispatch, e.g. a docs-only or `[skip ci]`-adjacent push, can show one green
#      run; confirm the gate another way. See SKILL.md "Post-merge CI".)
#
# A repo push to main sometimes spawns NO run at all (measured 2026-08-23 on a
# docs-only commit with no [skip ci] marker); that surfaces here as exit 3 with a
# "no run appeared" line, so the caller re-points at a descendant commit's run
# rather than waiting forever.
set -u

SHA="${1:?usage: watch-ci.sh <sha> [branch] [interval] [max-polls]}"
BRANCH="${2:-main}"
INT="${3:-60}"
MAX="${4:-60}"

runs_json() {
  gh run list --branch "$BRANCH" --limit 20 \
    --json headSha,status,conclusion,workflowName \
    --jq "[.[] | select(.headSha | startswith(\"$SHA\")) | {w: .workflowName, s: .status, c: .conclusion}]" \
    2>/dev/null
}

i=0
while [ "$i" -lt "$MAX" ]; do
  runs="$(runs_json)"
  n="$(printf '%s' "$runs" | jq 'length' 2>/dev/null || echo 0)"
  pending="$(printf '%s' "$runs" | jq '[.[] | select(.s != "completed")] | length' 2>/dev/null || echo 1)"
  printf '%s %s\n' "$(date +%H:%M:%S)" "$runs"
  if [ "$n" -gt 0 ] && [ "$pending" = "0" ]; then
    fails="$(printf '%s' "$runs" | jq '[.[] | select(.c=="failure" or .c=="timed_out" or .c=="startup_failure")] | length')"
    cancels="$(printf '%s' "$runs" | jq '[.[] | select(.c=="cancelled")] | length')"
    # GitHub treats success/skipped/neutral as PASSING for a completed check; every
    # OTHER completed conclusion (action_required, stale, and any value GitHub adds
    # later) is NOT green. Count the green set explicitly and require EVERY run to be
    # in it — the old `else → success` branch reported success for anything that was
    # merely not-failure-and-not-cancelled, so an action_required/stale run read green.
    greens="$(printf '%s' "$runs" | jq '[.[] | select(.c=="success" or .c=="skipped" or .c=="neutral")] | length')"
    printf 'ALL-COMPLETED\n%s\n' "$runs"
    if [ "$fails" -gt 0 ]; then
      echo "RESULT=failure"
      exit 1
    fi
    if [ "$cancels" -gt 0 ]; then
      echo "RESULT=cancelled (supersession: re-point at current origin/$BRANCH HEAD)"
      exit 2
    fi
    if [ "$greens" -eq "$n" ]; then
      echo "RESULT=success"
      exit 0
    fi
    # A completed run concluded neither green (success/skipped/neutral), red
    # (failure/timed_out/startup_failure) nor cancelled — e.g. action_required or
    # stale. Fail closed: do NOT report success; surface it for inspection.
    echo "RESULT=indeterminate (a run's conclusion is not in success/skipped/neutral — inspect, do not merge on this: $runs)"
    exit 3
  fi
  i=$((i + 1))
  sleep "$INT"
done

if [ "$(runs_json | jq 'length' 2>/dev/null || echo 0)" = "0" ]; then
  echo "NO-RUN-APPEARED (push may have spawned no workflow run; re-point at a descendant commit)"
else
  echo "TIMEOUT (runs never settled within $MAX polls; raise max-polls or re-launch)"
fi
exit 3
