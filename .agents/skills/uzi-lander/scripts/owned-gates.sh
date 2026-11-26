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
# Matches every nonterminal run in a gate status (awaiting_approval, awaiting_input, paused,
# recovery_wait) against this session's claims (claims.sh list --json, owner_uuid = whoami):
#   run-<ID> claim  -> that run id
#   #<PR> claim     -> runs whose mr_iid is the PR, whose branch is the PR's head branch, or a
#                      ci_fix whose issue_title is "Fix CI: <head branch> pipeline ..."
# Prints one line per gate:
#   GATE key=<claim> run=<id> kind=<kind> status=<status> since=<created_at> title=<title>
# then RESULT=none|gates COUNT=<n>.
#
# Exit codes: 0 none, 1 at least one gate, 2 usage, 3 lookup failure (uzi, gh, claims).
# A lookup failure never reads as "none".
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAIMS="${OWNED_GATES_CLAIMS:-$HERE/claims.sh}"

[ $# -eq 1 ] && [ -n "$1" ] || { sed -n '2,21p' "$0" >&2; exit 2; }
repo="$1"
broken() { echo "owned-gates: $*" >&2; echo "RESULT=unknown"; exit 3; }

me="$("$CLAIMS" whoami 2>/dev/null | sed -n 's/^SESSION_UUID=//p')"
[ -n "$me" ] || broken "claims.sh whoami gave no SESSION_UUID"
claims="$("$CLAIMS" list --json 2>/dev/null)" || broken "claims.sh list failed"
mine="$(jq -c --arg me "$me" --arg repo "$repo" \
  '[.[] | select(.owner_uuid == $me and ((.repo // $repo) == $repo))]' <<<"$claims")" \
  || broken "claims list is not JSON"

runs="$(uzi run list --json 2>/dev/null)" || broken "uzi run list failed"
jq -e 'type == "array"' >/dev/null 2>&1 <<<"$runs" || broken "uzi run list is not a JSON array"
repo_id="$(uzi repo list --json 2>/dev/null | jq -r --arg r "$repo" '.[] | select(.path_with_namespace == $r) | .id' 2>/dev/null)"
[ -n "$repo_id" ] || broken "repo $repo not found in uzi repo list"

# key -> head branch for each #PR claim (one gh call per PR).
targets='[]'
while IFS= read -r key; do
  [ -n "$key" ] || continue
  case "$key" in
    run-*) targets="$(jq -c --arg k "$key" --arg id "${key#run-}" '. + [{key:$k, run:$id}]' <<<"$targets")" ;;
    '#'*)
      pr="${key#\#}"
      head="$(gh pr view "$pr" --repo "$repo" --json headRefName --jq .headRefName 2>/dev/null)" \
        || broken "gh pr view $pr failed"
      targets="$(jq -c --arg k "$key" --argjson pr "$pr" --arg h "$head" '. + [{key:$k, pr:$pr, head:$h}]' <<<"$targets")" ;;
  esac
done < <(jq -r '.[].key' <<<"$mine")

out="$(jq -r --argjson t "$targets" --arg rid "$repo_id" '
  def gate: . as $s | ["awaiting_approval","awaiting_input","paused","recovery_wait"] | index($s) != null;
  [ .[] | select((.repo_id // $rid) == $rid) | select(.status | gate) | . as $r
    | ($t[] | . as $c | select(
          ($c.run != null and $r.id == $c.run)
       or ($c.pr != null and (($r.mr_iid // -1) == $c.pr
            or ($c.head != "" and (($r.branch // "") == $c.head
              or ($r.kind == "ci_fix" and (($r.issue_title // "") | startswith("Fix CI: " + $c.head + " "))))))))
      ) as $m
    | "GATE key=\($m.key) run=\($r.id) kind=\($r.kind) status=\($r.status) since=\($r.created_at) title=\(($r.issue_title // "") | gsub("[\\r\\n\\t]"; " "))"
  ] | unique | .[]' <<<"$runs")" || broken "match failed"

n=0
[ -n "$out" ] && n="$(printf '%s\n' "$out" | wc -l | tr -d ' ')" && printf '%s\n' "$out"
if [ "$n" -gt 0 ]; then echo "RESULT=gates COUNT=$n"; exit 1; fi
echo "RESULT=none COUNT=0"
