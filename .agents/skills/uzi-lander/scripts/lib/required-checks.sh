# shellcheck shell=bash
# required-checks.sh — the required status-check contexts a PR must report before its CI
# counts as settled. Sourced by watch-pr.sh, merge.sh and takeover.sh.
#
# `gh pr checks --required` lists only the required checks that have already REGISTERED on
# the head. Right after a push a few fast ones register and pass before the rest are queued,
# so a non-empty, all-green list is not proof CI is done. The authority for "which checks
# are required" is the base branch's rulesets and classic protection; a required context missing from the list is
# pending. Contexts match check names, so required job names must be unique across
# workflows (the rules carry no workflow identity to tell two same-named jobs apart).
# Empty rulesets authorize the all-checks fallback only after explicit classic absence.
# Nonempty rulesets retain the legacy required-check path if classic cannot be read.

# Classic 404s are absence only for the two explicit GitHub configuration messages.
# Read the HTTP status, not a message which a 403/500 body could also contain.
classic_required_contexts() {
  local raw rc=0 status body branch
  branch=$(jq -rn --arg name "$2" '$name | @uri') || return 1
  raw=$(gh api --include "repos/$1/branches/$branch/protection/required_status_checks" 2>/dev/null) || rc=$?
  status=$(printf '%s\n' "$raw" | awk 'NR==1 && $1 ~ /^HTTP\/[0-9.]+$/ && $2 ~ /^[0-9][0-9][0-9]$/ {print $2}')
  body=$(printf '%s\n' "$raw" | awk '{sub(/\r$/, "")} found {print; next} /^$/ {found=1}')
  case "$status" in
    200)
      [ "$rc" -eq 0 ] || return 1
      printf '%s' "$body" | jq -ce '
        if type=="object" and (has("contexts") or has("checks"))
          and (if has("contexts") then (.contexts | type=="array" and all(.[]; type=="string" and length>0)) else true end)
          and (if has("checks") then (.checks | type=="array" and all(.[]; type=="object" and (.context | type)=="string" and (.context | length)>0)) else true end)
        then ((.contexts // []) + [(.checks // [])[] | .context]) | unique
          | if length>0 then . else error("empty enabled checks") end
        else error("classic checks") end' 2>/dev/null || return 1 ;;
    404)
      [ "$rc" -ne 0 ] || return 1
      printf '%s' "$body" | jq -ce '
        if type=="object" and (.message=="Branch not protected" or .message=="Required status checks not enabled")
        then [] else error("unknown 404") end' 2>/dev/null || return 1 ;;
    *) return 1 ;;
  esac
}

# required_contexts OWNER/REPO BRANCH — prints the required contexts as a sorted JSON array
# (`[]` when the branch's rules require none). Exit 1 when any page cannot be read or a
# required-check rule is malformed: callers treat that as unknown, never as "none required".
required_contexts() {
  local out rules classic
  out=$(gh api --paginate --slurp "repos/$1/rules/branches/$2" 2>/dev/null) || return 1
  rules=$(printf '%s' "$out" | jq -ce '
    if type == "array" and length > 0 and all(.[]; type == "array") then add // [] else error("pages") end
    | if all(.[]; type == "object" and (.type | type) == "string" and (.type | length) > 0)
      then . else error("rules") end
    | [ .[] | select(.type == "required_status_checks")
        | .parameters.required_status_checks
        | if type == "array"
             and all(.[]; (.context | type) == "string" and (.context | length) > 0)
          then .[].context else error("rule") end ]
    | unique' 2>/dev/null) || return 1
  if ! classic=$(classic_required_contexts "$1" "$2"); then
    [ "$rules" != '[]' ] || return 1
    classic='[]'
  fi
  jq -nc --argjson rules "$rules" --argjson classic "$classic" '$rules + $classic | unique'
}

# pr_ci_checks REPO PR REQUIRED_JSON HEAD BASE — REQUIRED_JSON is required_contexts output.
# Select required checks, or all checks only when
# readable base rules definitively require none. Preserve gh's bucket/exit semantics.
# In the all-checks lane re-confirm the head and base before publishing the response.
pr_ci_checks() {
  local err out rc=0 pv current current_base
  local args=(pr checks "$2" --repo "$1")
  printf '%s' "$3" | jq -e 'type=="array" and all(.[]; type=="string" and length>0)' >/dev/null 2>&1 || return 1
  if [ "$3" != '[]' ]; then args+=(--required); fi
  err=$(mktemp) || return 1
  out=$(gh "${args[@]}" --json name,bucket 2>"$err") || rc=$?
  if [ "$3" = '[]' ]; then
    pv=$(gh pr view "$2" --repo "$1" --json headRefOid,state,baseRefName 2>/dev/null) || pv=""
    current=$(printf '%s' "$pv" | jq -er '.headRefOid | select(type=="string" and length>0)' 2>/dev/null) || current=""
    current_base=$(printf '%s' "$pv" | jq -er '.baseRefName | select(type=="string" and length>0)' 2>/dev/null) || current_base=""
    if [ -z "$4" ] || [ -z "$5" ] || [ "$current" != "$4" ] || [ "$current_base" != "$5" ]; then
      rm -f "$err"
      echo "cannot confirm CI checks on the expected head and base" >&2
      return 1
    fi
  fi
  cat "$err" >&2
  rm -f "$err"
  printf '%s' "$out"
  return "$rc"
}

# A missing/unknown bucket cannot silently count as a passing check.
ci_checks_valid() {
  printf '%s' "$1" | jq -e 'type=="array" and all(.[];
    type=="object" and (.bucket as $b | ["pass","skipping","pending","fail","cancel"] | index($b) != null))' >/dev/null 2>&1
}

# missing_required REQUIRED_JSON CHECKS_JSON — prints how many required contexts are absent
# from a `gh pr checks --json name,...` array (matched by name).
missing_required() {
  jq -n --argjson req "$1" --argjson got "$2" '($got | map(.name)) as $n | [$req[] | select(. as $c | $n | index($c) | not)] | length'
}
