# shellcheck shell=bash
# required-checks.sh — the required status-check contexts a PR must report before its CI
# counts as settled. Sourced by watch-pr.sh and merge.sh.
#
# `gh pr checks --required` lists only the required checks that have already REGISTERED on
# the head. Right after a push a few fast ones register and pass before the rest are queued,
# so a non-empty, all-green list is not proof CI is done. The authority for "which checks
# are required" is the base branch's rulesets; a required context missing from the list is
# pending. Contexts match check names, so required job names must be unique across
# workflows (the rules carry no workflow identity to tell two same-named jobs apart).
# Classic branch protection is not read: this repo gates `main` with rulesets only.

# required_contexts OWNER/REPO BRANCH — prints the required contexts as a sorted JSON array
# (`[]` when the branch's rules require none). Exit 1 when any page cannot be read or a
# required-check rule is malformed: callers treat that as unknown, never as "none required".
required_contexts() {
  local out
  out=$(gh api --paginate --slurp "repos/$1/rules/branches/$2" 2>/dev/null) || return 1
  printf '%s' "$out" | jq -ce '
    if type == "array" and length > 0 and all(.[]; type == "array") then add // [] else error("pages") end
    | [ .[] | select(.type == "required_status_checks")
        | .parameters.required_status_checks
        | if type == "array"
             and all(.[]; (.context | type) == "string" and (.context | length) > 0)
          then .[].context else error("rule") end ]
    | unique' 2>/dev/null || return 1
}

# missing_required REQUIRED_JSON CHECKS_JSON — prints how many required contexts are absent
# from a `gh pr checks --json name,...` array (matched by name).
missing_required() {
  jq -n --argjson req "$1" --argjson got "$2" '($got | map(.name)) as $n | [$req[] | select(. as $c | $n | index($c) | not)] | length'
}
