#!/usr/bin/env bash
# Shared branch-writer predicate. Inputs are read-only uzi run-list JSON, repo id,
# and exact pipeline_ref. Callers own lookup failures; malformed data fails closed.

ci_fix_active_runs() {
  [ -n "$2" ] && [ -n "$3" ] || return 1
  printf '%s' "$1" | jq -ce --arg repo "$2" --arg br "$3" '
    if type != "array" or any(.[]; type != "object") then error("invalid run list") else . end
    | if any(.[] | select(.kind=="ci_fix" and .repo_id==$repo and .pipeline_ref==$br);
        (.id | type != "string") or (.id | test("^[A-Za-z0-9_-]+$") | not) or
        (.status | type != "string") or (.status | test("^[a-z_]+$") | not))
      then error("invalid ci_fix identity/status") else . end
    | [.[]|select(.kind=="ci_fix" and .repo_id==$repo and .pipeline_ref==$br and ((.status|test("completed|failed|cancelled"))|not))
        | {id,status}]'
}

ci_fix_active_ids() {
  local active
  active=$(ci_fix_active_runs "$@") || return 1
  printf '%s' "$active" | jq -r 'map(.id)|join(" ")'
}

# Populate one-line CI_FIX_ACTIVE/RECENT summaries; unknown is never none. Recent
# means the newest terminal run finished within 24h (including partial failed work).
# Optional epoch argument makes the boundary hermetic in unit tests.
ci_fix_snapshot() {
  local active snapshot now_epoch
  CI_FIX_ACTIVE=unknown; CI_FIX_RECENT=unknown; CI_FIX_ACTIVE_COUNT=0
  active=$(ci_fix_active_runs "$1" "$2" "$3") || return 1
  now_epoch=${4:-$(date +%s)}
  snapshot=$(printf '%s' "$1" | jq -ce --arg repo "$2" --arg br "$3" \
    --argjson active "$active" --argjson now "$now_epoch" '
    def epoch:
      "^(?<clock>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<zone>Z|[+-][0-2][0-9]:[0-5][0-9])$" as $format
      | if type != "string" or (test($format)|not) then error("invalid ci_fix finish") else . end
      | capture($format) as $t
      | ($t.clock + "Z" | fromdateiso8601) -
        (if $t.zone == "Z" then 0 else
          (($t.zone[1:3]|tonumber)*3600 + ($t.zone[4:6]|tonumber)*60) *
          (if $t.zone[0:1] == "+" then 1 else -1 end) end);
    [.[] | select(.kind=="ci_fix" and .repo_id==$repo and .pipeline_ref==$br and (.status|test("completed|failed|cancelled")))
      | . + {finished_epoch: (.finished_at | epoch)}
      | if .finished_epoch > ($now + 300) then error("future ci_fix finish") else . end
      | select(.finished_epoch >= ($now - 86400))] | sort_by(.finished_epoch, .id) | last as $recent
    | {active: (if ($active|length)==0 then "none" else $active|sort_by(.id)|map(.id+" "+.status)|join("; ") end),
       count: ($active|length),
       recent: (if $recent == null then "none" else $recent.id+" "+$recent.status+" "+$recent.finished_at end)}') || return 1
  # shellcheck disable=SC2034 # Sourced result, read by takeover.sh and watch-pr.sh.
  CI_FIX_ACTIVE=$(printf '%s' "$snapshot" | jq -r '.active') || return 1
  # shellcheck disable=SC2034 # Sourced result, read by takeover.sh and watch-pr.sh.
  CI_FIX_RECENT=$(printf '%s' "$snapshot" | jq -r '.recent') || return 1
  # shellcheck disable=SC2034 # Sourced result, read by takeover.sh and watch-pr.sh.
  CI_FIX_ACTIVE_COUNT=$(printf '%s' "$snapshot" | jq -r '.count') || return 1
}
