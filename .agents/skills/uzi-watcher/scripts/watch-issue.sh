#!/usr/bin/env bash
# Discover the first new run for a queued issue, then delegate to watch-run.sh.
# Arm with a verified completion route: this script neither notifies nor re-arms itself.
# Usage: watch-issue.sh --repo PATH|UUID [--since EPOCH] ISSUE [stop-csv] [interval] [max-polls]
# Capture --since with `jq -nr now` BEFORE queueing; reuse the printed SINCE on re-arm.
# Without --since the baseline is this invocation's start, not the issue's filing time.
# Discovery sees only the CLI's 200 newest runs, not unbounded history. Terminal runs
# newer than the baseline count too: a fast sweep can finish between polls.
# Unsuccessful discovery polls consume the shared budget. The matching poll hands its
# remaining slots to watch-run, with no plan/question watermark; any gate is real.
# Exit/result contracts are watch-run's; invalid/ambiguous selectors exit 2, elapsed 3.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO=""
SINCE=""
args=()
usage() {
  printf 'usage: watch-issue.sh --repo PATH|UUID [--since EPOCH] ISSUE [stop-csv] [interval] [max-polls]\n' >&2
  exit 2
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo|--since)
      [ "$#" -ge 2 ] && [ -n "$2" ] || usage
      if [ "$1" = --repo ]; then REPO="$2"; else SINCE="$2"; fi
      shift 2 ;;
    --*) usage ;;
    *) args+=("$1"); shift ;;
  esac
done
[ -n "$REPO" ] && [ "${#args[@]}" -ge 1 ] && [ "${#args[@]}" -le 4 ] || usage
ISSUE="${args[0]}"
STOP="${args[1]:-completed,failed,cancelled,awaiting_approval,awaiting_input,paused}"
INT="${args[2]:-30}"
MAX="${args[3]:-240}"
[[ "$ISSUE" =~ ^[1-9][0-9]*$ ]] && [[ "$INT" =~ ^[0-9]+([.][0-9]+)?$ ]] &&
  [[ "$MAX" =~ ^(0|[1-9][0-9]*)$ ]] || usage
if [ -z "$SINCE" ]; then SINCE="$(jq -nr now)" || exit 2; fi
jq -en --arg s "$SINCE" '$s | tonumber | isfinite and . >= 0' >/dev/null 2>&1 || usage
printf 'ISSUE=%s REPO=%q SINCE=%s\n' "$ISSUE" "$REPO" "$SINCE"

polls=0
while [ "$polls" -lt "$MAX" ]; do
  # A nonzero CLI exit, skew diagnostic on stderr, or malformed stdout cannot bind a run.
  if rows="$(uzi run list --json 2>/dev/null)" &&
     selection="$(printf '%s' "$rows" | jq -c --arg repo "$REPO" --argjson issue "$ISSUE" --argjson since "$SINCE" '
       def epoch:
         capture("^(?<time>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<fraction>\\.[0-9]+)?(?<zone>Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$") as $t
         | ($t.time + "Z" | fromdateiso8601)
           + ("0" + ($t.fraction // "") | tonumber)
           - (if $t.zone == "Z" then 0 else
               (($t.zone[1:3] | tonumber) * 3600 + ($t.zone[4:6] | tonumber) * 60)
               * (if $t.zone[0:1] == "+" then 1 else -1 end) end);
       if type != "array" then error("expected run array") else
         [.[] | select(type == "object")
          | select(.issue_iid == $issue and (.repo_id == $repo or .repo_path == $repo))
          | select((.id | type) == "string" and (.repo_id | type) == "string"
                   and (.status | type) == "string" and .status != "")
          | select(.id | test("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"))
          | (try (.created_at | epoch) catch null) as $at
          | select($at != null and $at >= $since)
          | {id, repo_id, at: $at}] | sort_by(.at, .id)
         | if (map(.repo_id) | unique | length) > 1 then {ambiguous: true}
           else {run: (.[0].id // null)} end
       end' 2>/dev/null)"; then
    if [ "$(printf '%s' "$selection" | jq -r '.ambiguous // false')" = true ]; then
      printf 'ERROR repo path matches multiple repositories; use --repo UUID\n' >&2
      printf 'RESULT=error\n'
      exit 2
    fi
    rid="$(printf '%s' "$selection" | jq -r '.run // ""')"
    if [[ "$rid" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
      printf 'RUN=%s\n' "$rid"
      exec bash "$HERE/watch-run.sh" "$rid" "$STOP" "$INT" "$((MAX - polls))"
    fi
  fi
  polls=$((polls + 1))
  if [ "$polls" -lt "$MAX" ]; then sleep "$INT"; fi
done
printf 'ELAPSED issue=%s: no matching run observed; coverage ended\n' "$ISSUE"
printf 'REARM='
printf '%q ' "$HERE/watch-issue.sh" --repo "$REPO" --since "$SINCE" "$ISSUE" "$STOP" "$INT" "$MAX"
printf '\nRESULT=elapsed\n'
exit 3
