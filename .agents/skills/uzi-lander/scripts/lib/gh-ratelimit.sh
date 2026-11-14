#!/usr/bin/env bash
# shellcheck shell=bash
# GitHub primary quota diagnosis after a failed/unknown read. Source; no traps or set.
# GET /rate_limit costs no REST core quota. Secondary limits and an unreadable quota
# response remain ordinary unknowns; neither is evidence of primary exhaustion.

# gh_rate_limit_report: 0 + metadata when core/graphql has exhausted a positive
# allocation; 1 + no output otherwise (including malformed/unavailable telemetry).
# An allocation of zero is unavailable, not a resettable exhausted allowance.
gh_rate_limit_report() {
  local payload row now
  payload=$(command gh api rate_limit 2>/dev/null) || return 1
  now=$(date +%s) || return 1
  row=$(printf '%s' "$payload" | jq -er --argjson now "$now" '
    [.resources | to_entries[] | select(.key == "core" or .key == "graphql")
      | select((.value.limit | type) == "number" and .value.limit > 0
        and .value.remaining == 0 and (.value.reset | type) == "number"
        and .value.reset > 0 and .value.reset == (.value.reset | floor))]
    | select(length > 0)
    | (map(.key) | sort | join(",")) as $resources
    | (map(.value.reset) | max) as $reset
    | "GH_RATE_LIMITED=1\nGH_RATE_LIMIT_RESOURCE=\($resources)\nGH_RATE_LIMIT_RESET=\($reset)\nGH_RATE_LIMIT_RESET_AT=\($reset | todateiso8601)\nGH_RETRY_AT=\(([$reset, $now] | max) + 5)"
  ' 2>/dev/null) || return 1
  printf '%s\n' "$row"
}

# Call in the poller's MAIN shell on failed/unknown reads, never inside $(...).
# Exit 12 is a bounded handback, not readiness or a retry. The owner re-arms after
# GH_RETRY_AT (latest exhausted reset + 5s, or now + 5s for stale reset telemetry).
# No unknown streak is charged and no poll/sleep budget is silently extended.
gh_rate_limit_stop() {
  local report
  if report=$(gh_rate_limit_report); then
    printf '%s\n' "$report"
    printf '%s\n' "$report" | awk -F= '
      /^GH_RATE_LIMIT_RESOURCE=/ {resource=$2}
      /^GH_RATE_LIMIT_RESET_AT=/ {reset=$2}
      /^GH_RETRY_AT=/ {retry=$2}
      END {printf "RESULT=gh_rate_limited resource=%s reset=%s retry_at=%s\n", resource, reset, retry}'
    exit 12
  fi
}
