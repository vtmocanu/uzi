#!/usr/bin/env bash
# lib/cr-signal.sh — why CodeRabbit did or did not review a head. Source it; do not execute.
#
# CodeRabbit reports on a head through two channels: the legacy commit STATUS (context
# "CodeRabbit") and a CHECK-RUN from the `coderabbitai` app. A file-limit skip arrived only
# as a check-run (PR #4260, 2026-11-13: conclusion skipped, title "Review skipped: 133 files
# exceed the limit of 100"), so a status-only reader saw "absent" and never routed to Greptile.

# cr_head_signal REPO HEAD — prints "<at>\t<desc>": CodeRabbit's newest description for HEAD
# ("Review in progress", "Review rate limited", "Review skipped: …", another title such as
# "Finished") and when it was set, or an empty line when CR never touched the head.
# Precedence is conservative, so a pending review is never hidden:
#   1. "in progress" on either channel wins (a check-run not completed reads "Review in
#      progress"), stamped with the newest timestamp seen;
#   2. otherwise the newer timestamp wins; a missing timestamp counts as newest, and a tie
#      goes to the check-run (CR's current channel).
# Returns 1 when either channel is unreadable; the caller decides what unknown means.
cr_head_signal() {
  local repo=$1 head=$2 st runs
  st=$(gh api "repos/$repo/commits/$head/status" 2>/dev/null) || return 1
  printf '%s' "$st" | jq -e 'has("statuses")' >/dev/null 2>&1 || return 1
  runs=$(gh api --paginate "repos/$repo/commits/$head/check-runs?check_name=CodeRabbit" 2>/dev/null) || return 1
  printf '%s' "$runs" | jq -es 'length>0 and all(.[]; type=="object" and has("check_runs"))' >/dev/null 2>&1 || return 1
  jq -rn --argjson st "$st" --argjson runs "$(printf '%s' "$runs" | jq -s '[.[].check_runs[]]')" '
    ([$st.statuses[] | select(.context=="CodeRabbit")] | last) as $s0
    | ([$runs[] | select((.app.slug // "") == "coderabbitai")] | max_by(.id)) as $c0
    | [ (if $s0 == null then empty else {at: ($s0.updated_at // ""), desc: ($s0.description // "")} end),
        (if $c0 == null then empty else
           {at: ($c0.completed_at // $c0.started_at // ""),
            desc: (if $c0.status != "completed" then "Review in progress" else ($c0.output.title // "") end)} end) ]
    | (map(.at) | max // "") as $newest
    | (map(select(.desc | test("in progress"; "i"))) | first) as $pending
    | if length == 0 then ""
      elif $pending != null then "\($newest)\t\($pending.desc)"
      else (to_entries
            | max_by([(.value.at == ""), .value.at, .key]) | .value
            | "\(.at)\t\(.desc)")
      end
  ' 2>/dev/null || return 1
}

# cr_head_desc REPO HEAD — the description alone ("" = absent); same return contract.
cr_head_desc() {
  local sig
  sig=$(cr_head_signal "$1" "$2") || return 1
  printf '%s\n' "${sig#*$'\t'}" | sed '/^$/d'
}
