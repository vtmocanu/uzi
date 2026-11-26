#!/usr/bin/env bash
# owned-gates.sh — list uzi runs parked for their owner on anything THIS session has claimed.
#
# A claimed PR that is parked (waiting on a migration chain, a merge-lane hold) has no watcher,
# but uzi can still start a run on its branch: a ci_fix repair or an mr_rework that stops at a
# plan gate or a question. Nothing pages anyone; the run waits until someone looks. Run this
# on every pass over parked work and before treating a claimed PR as idle.
#
# Usage: owned-gates.sh OWNER/REPO
#
# Gate statuses: awaiting_approval, awaiting_input, paused, recovery_wait. Against this
# session's claims for OWNER/REPO (claims.sh list --json, owner_uuid = whoami):
#   run-<ID> claim  -> `uzi run get <ID>` (exact id, no list needed)
#   #<PR> claim     -> runs in `uzi run list` with this repo's exact repo_id whose mr_iid is the
#                      PR, whose branch is the PR's head branch, or a ci_fix whose issue_title
#                      is "Fix CI: <head branch> pipeline ..."
# `uzi run list` returns only the newest RUN_LIST_CAP (200) runs. A full page still covers a PR
# claim when its oldest run predates the earliest PR claim (runs started after you claimed are
# all visible; earlier ones were in takeover.sh's snapshot). Otherwise, with no PR-claim gate
# found, absence is not established: RESULT=unknown, exit 3.
# Prints one line per gate:
#   GATE key=<claim> run=<id> kind=<kind> status=<status> since=<created_at> title=<title>
# then RESULT=none|gates|unknown COUNT=<n>.
#
# Exit codes: 0 none, 1 at least one gate, 2 usage, 3 unknown (a failed or ambiguous lookup,
# or a full run page). Never reads a failed lookup as "none".
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAIMS="${OWNED_GATES_CLAIMS:-$HERE/claims.sh}"
CAP="${RUN_LIST_CAP:-200}"
GATES='["awaiting_approval","awaiting_input","paused","recovery_wait"]'

[ $# -eq 1 ] && [ -n "$1" ] || { sed -n '2,25p' "$0" >&2; exit 2; }
repo="$1"
broken() { echo "owned-gates: $*" >&2; echo "RESULT=unknown"; exit 3; }
gate_line() { # $1 key, stdin: one run JSON
  jq -r --arg k "$1" '"GATE key=\($k) run=\(.id) kind=\(.kind) status=\(.status) since=\(.created_at) title=\((.issue_title // "") | gsub("[\\r\\n\\t]"; " "))"'
}

me="$("$CLAIMS" whoami 2>/dev/null | sed -n 's/^SESSION_UUID=//p')"
[ -n "$me" ] || broken "claims.sh whoami gave no SESSION_UUID"
claims="$("$CLAIMS" list --json 2>/dev/null)" || broken "claims.sh list failed"
mine="$(jq -c --arg me "$me" --arg repo "$repo" \
  '[.[] | select(.owner_uuid == $me and ((.repo // $repo) == $repo))]' <<<"$claims")" \
  || broken "claims list is not JSON"
keys="$(jq -r '.[].key' <<<"$mine")"
# Earliest PR claim; a claim without claimed_at gives "" and never counts as covered.
horizon="$(jq -r '[.[] | select(.key | startswith("#")) | (.claimed_at // "")] | if length == 0 then "" elif any(. == "") then "" else min end' <<<"$mine")"
[ -n "$keys" ] || { echo "RESULT=none COUNT=0"; exit 0; }

lines=()
targets='[]'
while IFS= read -r key; do
  [ -n "$key" ] || continue
  case "$key" in
    run-*)
      run="$(uzi run get "${key#run-}" --json 2>/dev/null)" || broken "uzi run get ${key#run-} failed"
      jq -e --argjson g "$GATES" '.status as $s | $g | index($s) != null' >/dev/null <<<"$run" \
        && lines+=("$(gate_line "$key" <<<"$run")") ;;
    '#'*)
      pr="${key#\#}"
      head="$(gh pr view "$pr" --repo "$repo" --json headRefName --jq .headRefName 2>/dev/null)" \
        || broken "gh pr view $pr failed"
      targets="$(jq -c --arg k "$key" --argjson pr "$pr" --arg h "$head" '. + [{key:$k, pr:$pr, head:$h}]' <<<"$targets")" ;;
  esac
done <<<"$keys"

saturated=0
if [ "$(jq length <<<"$targets")" -gt 0 ]; then
  ids="$(uzi repo list --json 2>/dev/null | jq -r --arg r "$repo" '[.[] | select(.path_with_namespace == $r) | .id] | if length == 1 then .[0] else empty end' 2>/dev/null)"
  [ -n "$ids" ] || broken "repo $repo is missing or ambiguous in uzi repo list"
  runs="$(uzi run list --json 2>/dev/null)" || broken "uzi run list failed"
  n_runs="$(jq 'if type == "array" then length else error("not an array") end' <<<"$runs" 2>/dev/null)" \
    || broken "uzi run list is not a JSON array"
  if [ "$n_runs" -ge "$CAP" ]; then
    oldest="$(jq -r '[.[].created_at // empty] | min // ""' <<<"$runs")"
    # ISO-8601 UTC strings compare lexically.
    if [ -z "$horizon" ] || [ -z "$oldest" ] || [[ "$oldest" > "$horizon" ]]; then saturated=1; fi
  fi
  while IFS= read -r l; do [ -n "$l" ] && lines+=("$l"); done < <(jq -r --argjson t "$targets" --arg rid "$ids" --argjson g "$GATES" '
    [ .[] | select(.repo_id == $rid) | . as $r | select($g | index($r.status) != null)
      | ($t[] | . as $c | select(
            ($r.mr_iid // -1) == $c.pr
         or ($c.head != "" and (($r.branch // "") == $c.head
              or ($r.kind == "ci_fix" and (($r.issue_title // "") | startswith("Fix CI: " + $c.head + " "))))))
        ) as $m
      | "GATE key=\($m.key) run=\($r.id) kind=\($r.kind) status=\($r.status) since=\($r.created_at) title=\(($r.issue_title // "") | gsub("[\\r\\n\\t]"; " "))"
    ] | unique | .[]' <<<"$runs") || broken "match failed"
fi

n=${#lines[@]}
[ "$n" -gt 0 ] && printf '%s\n' "${lines[@]}"
if [ "$n" -gt 0 ]; then echo "RESULT=gates COUNT=$n"; exit 1; fi
if [ "$saturated" -eq 1 ]; then
  echo "owned-gates: uzi run list returned $CAP runs (its cap) and its oldest run is newer than the earliest PR claim; older runs are not visible" >&2
  echo "RESULT=unknown COUNT=0"; exit 3
fi
echo "RESULT=none COUNT=0"
