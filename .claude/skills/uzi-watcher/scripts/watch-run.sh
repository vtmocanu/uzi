#!/usr/bin/env bash
# Poll a uzi run's status until it reaches a stop-set state, printing each
# transition with a timestamp. Designed to be launched with Claude Code's
# run_in_background so the harness re-invokes the model when it exits.
#
# WHY a script and not `uzi run wait`: this harness reaps long-lived background
# processes, so a backgrounded `uzi run wait` gets killed mid-run and you never
# learn the outcome. A short poll loop that exits on the stop set survives,
# because each exit is a clean notification and a killed poll simply re-fires.
#
# uzi's benign "CLI is behind server" line goes to stderr and is discarded here.
#
# Usage: watch-run.sh <run-id> [stop-csv] [interval-secs] [max-polls] [min-plan-seq]
#   stop-csv (default) covers a plan gate, a question park, and every terminal
#   state: completed,failed,cancelled,awaiting_approval,awaiting_input
#   For "watch to the end only" (after you have approved), pass:
#     watch-run.sh <id> completed,failed,cancelled 60
#
#   min-plan-seq (optional 5th arg) — use it after `uzi run revise` to wait for
#   the REVISED plan. When set, an `awaiting_approval` state only stops the watch
#   once the run has a plan message with seq > min-plan-seq. This is immune to
#   whether the poll happened to catch the transient re-planning `running` window
#   (a "did I observe running?" heuristic loops forever if it starts too late).
#   Terminal and `awaiting_input` states still stop unconditionally. Capture the
#   baseline BEFORE revising:
#     SEQ=$(uzi run logs <id> --json | jq -rs '[.[]|select(.kind=="plan")|.seq]|max // 0')
#     uzi run revise <id> -m "…"
#     watch-run.sh <id> "" "" "" "$SEQ"
set -u
RID="${1:?usage: watch-run.sh <run-id> [stop-csv] [interval] [max] [min-plan-seq]}"
STOP="${2:-completed,failed,cancelled,awaiting_approval,awaiting_input}"
INT="${3:-45}"
MAX="${4:-160}"
MIN_PLAN_SEQ="${5:-}"

max_plan_seq() {
  uzi run logs "$RID" --json 2>/dev/null \
    | jq -rs '[.[] | select(.kind=="plan") | .seq] | max // 0'
}

last=""
i=0
while [ "$i" -lt "$MAX" ]; do
  s="$(uzi run get "$RID" --field status 2>/dev/null)"
  if [ -n "$s" ] && [ "$s" != "$last" ]; then
    printf '%s status=%s\n' "$(date +%H:%M:%S)" "$s"
    last="$s"
  fi
  case ",$STOP," in
    *",$s,"*)
      # A revised-plan watch (min-plan-seq set) must not stop at the OLD gate:
      # only stop at awaiting_approval once a newer plan message exists.
      if [ "$s" = "awaiting_approval" ] && [ -n "$MIN_PLAN_SEQ" ]; then
        cur="$(max_plan_seq)"
        if [ "${cur:-0}" -le "$MIN_PLAN_SEQ" ]; then
          i=$((i + 1))
          sleep "$INT"
          continue
        fi
        printf 'STOP=%s (revised plan seq=%s > %s)\n' "$s" "$cur" "$MIN_PLAN_SEQ"
        exit 0
      fi
      printf 'STOP=%s\n' "$s"
      exit 0
      ;;
  esac
  i=$((i + 1))
  sleep "$INT"
done
printf 'ELAPSED last=%s (raise max-polls or re-launch)\n' "$last"
