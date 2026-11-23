#!/usr/bin/env bash
# Shared branch-writer predicate. Inputs are read-only uzi run-list JSON, repo id,
# exact pipeline_ref, and optional MR number. Callers own lookup failures;
# malformed data fails closed. Without an MR number only ci_fix is selected.

branch_helper_active_runs() {
  [ -n "$2" ] && [ -n "$3" ] || return 1
  printf '%s' "$1" | jq -sce --arg repo "$2" --arg br "$3" --argjson pr "${4:-0}" '
    # Exactly one JSON document: concatenated or empty input is malformed, never "no writers".
    if length != 1 then error("expected one run list document") else .[0] end
    | def matching:
      .repo_id==$repo and
      ((.kind=="ci_fix" and .pipeline_ref==$br) or
       (.kind=="mr_rework" and $pr>0 and .mr_iid==$pr));
    def terminal: .status | IN("completed", "failed", "cancelled");
    def ident: type == "string" and test("^[A-Za-z0-9_-]+$");
    def nullable($t): . == null or type == $t;
    # Validate EVERY row (matching or not) before filtering: a malformed row could hide a writer.
    if type != "array" or any(.[]; type != "object") then error("invalid run list") else . end
    | if any(.[];
        (.id | ident | not) or (.kind | type != "string") or
        (.status | type != "string" or (test("^[a-z_]+$") | not)) or
        (.repo_id | nullable("string") | not) or (.pipeline_ref | nullable("string") | not) or
        (.mr_iid | (. == null or (type == "number" and . == floor)) | not))
      then error("invalid run row") else . end
    | if any(.[] | select(matching);
        (.id | type != "string") or (.id | test("^[A-Za-z0-9_-]+$") | not) or
        (.status | type != "string") or (.status | test("^[a-z_]+$") | not))
      then error("invalid helper identity/status") else . end
    | [.[] | select(matching and (terminal|not)) | {id,kind,status}] | sort_by(.kind,.id)'
}

ci_fix_active_runs() {
  local active
  active=$(branch_helper_active_runs "$1" "$2" "$3") || return 1
  printf '%s' "$active" | jq -ce 'map({id,status})'
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
    [.[] | select(.kind=="ci_fix" and .repo_id==$repo and .pipeline_ref==$br and (.status|IN("completed", "failed", "cancelled")))
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

# Authoritative branch-writer read. Sets WRITERS (sorted JSON array of {id,kind,status}),
# WRITERS_VISIBILITY (all_owners | owner_only), WRITERS_RUNS (the owner run list when the
# fallback read it) and WRITERS_ERR (reason on refusal). Non-zero means "cannot rule out
# writers". Only an absent verb or HTTP 401/403/404 (CLI rc 3/4) falls back to the owner's own
# run list; every other failure refuses and never downgrades to owner-only.
# shellcheck disable=SC2034 # Sourced results (WRITERS_ERR etc.), read by the calling scripts.
branch_writers_read() {
  local repo=$1 br=$2 mr=${3:-0} help body rc=0 args
  WRITERS='[]'; WRITERS_VISIBILITY=''; WRITERS_RUNS=''; WRITERS_ERR=''
  [ -n "$repo" ] && [ -n "$br" ] || { WRITERS_ERR='repo or branch unresolved'; return 1; }
  case "$mr" in ''|0) mr=0;; *[!0-9]*) WRITERS_ERR='invalid MR number'; return 1;; esac
  help=$(uzi admin --help 2>&1 || true)
  if printf '%s\n' "$help" | grep -Eq '^[[:space:]]+run-writers([[:space:]]|$)'; then
    args=(--repo "$repo" --branch "$br"); [ "$mr" -eq 0 ] || args+=(--mr "$mr")
    body=$(uzi admin run-writers "${args[@]}" --json 2>/dev/null) || rc=$?
    case "$rc" in
      0)
        WRITERS=$(printf '%s' "$body" | jq -sce --arg br "$br" --argjson mr "$mr" '
          (if length == 1 then .[0] else error("documents") end)
          | if type != "object" or .complete != true or (.writers | type) != "array"
          then error("shape") else .writers end
          | if all(.[]; type == "object"
              and (.run_id | type == "string" and test("^[A-Za-z0-9_-]+$"))
              and (.kind | IN("ci_fix", "mr_rework"))
              and (.status | type == "string" and test("^[a-z_]+$") and (IN("completed", "failed", "cancelled") | not))
              and ((.kind == "ci_fix" and .pipeline_ref == $br) or (.kind == "mr_rework" and $mr > 0 and .mr_iid == $mr)))
            then map({id: .run_id, kind, status}) | sort_by(.kind, .id) else error("writer") end' 2>/dev/null) \
          || { WRITERS='[]'; WRITERS_ERR='uzi admin run-writers returned a malformed, incomplete or mismatched result'; return 1; }
        WRITERS_VISIBILITY=all_owners
        return 0 ;;
      3|4) ;;
      *) WRITERS_ERR="uzi admin run-writers failed (rc=$rc)"; return 1 ;;
    esac
  fi
  WRITERS_RUNS=$(uzi run list --json 2>/dev/null) \
    || { WRITERS_RUNS=''; WRITERS_ERR='uzi run list failed'; return 1; }
  WRITERS=$(branch_helper_active_runs "$WRITERS_RUNS" "$repo" "$br" "$mr" 2>/dev/null) \
    || { WRITERS='[]'; WRITERS_RUNS=''; WRITERS_ERR='could not parse uzi run list'; return 1; }
  WRITERS_VISIBILITY=owner_only
}

# Print the visibility line (and the owner-only caveat) after a successful read.
print_writers_visibility() {
  echo "VISIBILITY=$WRITERS_VISIBILITY"
  [ "$WRITERS_VISIBILITY" != owner_only ] \
    || echo 'note: owner-only visibility; foreign or older writers are not excluded'
}

# Derive CI_FIX_ACTIVE / CI_FIX_ACTIVE_COUNT / WRITERS_MRW_COUNT from WRITERS (after a
# successful branch_writers_read), in the same one-line shape ci_fix_snapshot prints.
# shellcheck disable=SC2034 # Sourced results, read by takeover.sh and watch-pr.sh.
writers_summarize() {
  local doc_ok
  # One JSON array only: two concatenated documents print two lines and must not reach a numeric test.
  doc_ok=$(printf '%s' "$WRITERS" | jq -se 'length == 1 and (.[0] | type) == "array"' 2>/dev/null) && [ "$doc_ok" = true ] || return 1
  CI_FIX_ACTIVE=$(printf '%s' "$WRITERS" | jq -r '[.[] | select(.kind == "ci_fix")] | sort_by(.id)
    | if length == 0 then "none" else map(.id + " " + .status) | join("; ") end') || return 1
  CI_FIX_ACTIVE_COUNT=$(printf '%s' "$WRITERS" | jq -r '[.[] | select(.kind == "ci_fix")] | length') || return 1
  WRITERS_MRW_COUNT=$(printf '%s' "$WRITERS" | jq -r '[.[] | select(.kind == "mr_rework")] | length') || return 1
  case "$CI_FIX_ACTIVE_COUNT$WRITERS_MRW_COUNT" in ''|*[!0-9]*) return 1;; esac
}

# Neither observation can clear an active writer seen by the other. The owner
# read happens later, so prefer its status when the same run appears twice.
writers_include_owner_active() {
  local owner_active
  owner_active=$(branch_helper_active_runs "$@") || return 1
  WRITERS=$(printf '%s\n%s\n' "$WRITERS" "$owner_active" | jq -sce '
    if length != 2 or any(.[]; type != "array") then error("invalid writer observations")
    else add | group_by(.id) | map(last) | sort_by(.kind, .id) end') || return 1
  writers_summarize
}
