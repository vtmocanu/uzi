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
# Usage: watch-run.sh <run-id> [stop-csv] [interval-secs] [max-polls]
#   stop-csv default covers a plan gate, a question park, and every terminal
#   state: completed,failed,cancelled,awaiting_approval,awaiting_input
#   For "watch to the end only" (after you have approved), pass:
#     watch-run.sh <id> completed,failed,cancelled 60
set -u
RID="${1:?usage: watch-run.sh <run-id> [stop-csv] [interval] [max]}"
STOP="${2:-completed,failed,cancelled,awaiting_approval,awaiting_input}"
INT="${3:-45}"
MAX="${4:-160}"
last=""
i=0
while [ "$i" -lt "$MAX" ]; do
  s="$(uzi run get "$RID" --field status 2>/dev/null)"
  if [ -n "$s" ] && [ "$s" != "$last" ]; then
    printf '%s status=%s\n' "$(date +%H:%M:%S)" "$s"
    last="$s"
  fi
  case ",$STOP," in
    *",$s,"*) printf 'STOP=%s\n' "$s"; exit 0 ;;
  esac
  i=$((i + 1))
  sleep "$INT"
done
printf 'ELAPSED last=%s (raise max-polls or re-launch)\n' "$last"
