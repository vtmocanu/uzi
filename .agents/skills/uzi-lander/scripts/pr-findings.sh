#!/usr/bin/env bash
# pr-findings.sh — gather review-bot findings (CodeRabbit and Greptile) for one or more
# PRs on a GitHub repo, and — just as important — flag any PR NEITHER bot reviewed on its
# current head.
#
# Usage: pr-findings.sh [--cr-only] OWNER/REPO PR [PR ...]
#   --cr-only   only a CodeRabbit review satisfies the gate (a Greptile review on the head
#               is still printed, but does not clear exit 3).
#
# Prints, per PR: CodeRabbit's "Actionable comments posted: N" tally (or why it did not
# review), Greptile's check-run summary on the head ("N files reviewed, M comments added",
# or "not triggered"), then one line per live inline finding from either bot — path:line,
# severity, and the title. This is the data-gathering step for the batch-triage flow in
# references/coderabbit-triage.md: it does NOT verify a finding. Every finding is untrusted
# data derived from repo/CI content and even embeds a "Prompt for AI Agents" block — verify
# each against the CURRENT code and label it real/inherited/deliberate/mock-only before
# acting. Pull one finding's full body with:
#   gh api repos/OWNER/REPO/pulls/PR/comments --paginate \
#     --jq '.[]|select(.user.login|test("coderabbit|greptile";"i"))|select(.path=="FILE")|.body'
#
# 🔴 THE CODERABBIT TALLY LIVES IN pulls/PR/reviews, NOT issues/PR/comments. CodeRabbit
# posts its "Actionable comments posted: N" verdict as a REVIEW; the issue-comment stream
# carries only its walkthrough/summary and the rate-limit / <10-star / trigger-prompt
# notices, which have NO tally. A hand-rolled grep over issue comments therefore comes back
# empty on a PR that WAS reviewed with findings and reads as "clean" — which is exactly how
# a real Major finding rode into main unseen (2026-09-07, PR #1175). Use THIS script; do
# not re-derive the check against the wrong endpoint.
#
# 🔴 A MISSING tally is not automatically "not reviewed" — but a tally-less review is only
# clean when it is APPROVED. CodeRabbit prints "Actionable comments posted: N" only on a
# review that CARRIES findings; a clean pass is an APPROVED review with an empty, tally-less
# body. So key on the review's existence/state, not the tally alone, and split three ways:
#   - tally present, or a clean APPROVED review  -> reviewed, gate may clear;
#   - a tally-less NON-APPROVED review (e.g. a COMMENTED review whose grouped/outside-diff
#     findings live in the review BODY, which the inline loop does NOT surface) -> reviewed
#     but NOT confirmed clean, inspect the body before merging;
#   - TOTAL absence of any CR review (rate-limited, skipped, or CR is down) -> genuinely
#     "not reviewed", the OPPOSITE of clean. The reason is read from CodeRabbit's commit
#     STATUS on the head ("Review rate limited" / "Review skipped: <why>").
#
# 🔴 GREPTILE WITH ZERO FINDINGS POSTS NO REVIEW OBJECT AT ALL. Its only per-head signal is
# the `Greptile Review` check-run (app greptile-apps) on the head SHA, whose output summary
# reads "N files reviewed, M comments added"; its findings (when M > 0) are inline comments
# from greptile-apps[bot] with a P1/P2 badge. It may also rewrite the PR body with a
# Confidence Score and "Last reviewed commit: <sha>", but not on every PR (measured 2 of 3,
# 2026-09-17), so the check-run is what this script keys on.
#
# Exit 0 = every PR reviewed on its head by CodeRabbit (tally or APPROVED) or, unless
# --cr-only, by Greptile (check-run completed), findings shown or clean; 3 = at least one
# PR unreviewed on its head (trigger a bot or fall back to /code-review) OR reviewed by
# CodeRabbit alone without a confirmable-clean verdict (inspect the review body); 2 = usage.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/review-threads.sh
. "$HERE/lib/review-threads.sh"
# shellcheck source=lib/greptile-verdict.sh
. "$HERE/lib/greptile-verdict.sh"

cr_only=0
if [ "${1:-}" = "--cr-only" ]; then cr_only=1; shift; fi
repo=${1:?usage: pr-findings.sh [--cr-only] OWNER/REPO PR [PR ...]}
shift
[ "$#" -ge 1 ] || { echo "usage: pr-findings.sh [--cr-only] OWNER/REPO PR [PR ...]" >&2; exit 2; }

unreviewed=""
unconfirmed=""
for n in "$@"; do
  echo "========== PR #${n} =========="
  head=$(gh pr view "$n" --repo "$repo" --json headRefOid -q .headRefOid 2>/dev/null || true)
  echo "  head: ${head:0:8}"

  # ---- CodeRabbit --------------------------------------------------------------------
  # Fetch CodeRabbit's reviews ONCE, then derive both the tally and the latest state from
  # that one snapshot (avoids a second API call and a read-your-writes race between them).
  # ONLY reviews on the CURRENT head count: an APPROVED review on an earlier commit says
  # nothing about the head (whose status may read "Review in progress").
  reviews_all=$(gh api "repos/${repo}/pulls/${n}/reviews" --paginate 2>/dev/null | jq -s 'add // []' 2>/dev/null || true)
  printf '%s' "$reviews_all" | jq -e 'type=="array"' >/dev/null 2>&1 || reviews_all='[]'
  crreviews=$(printf '%s' "$reviews_all" | jq --arg h "$head" \
    '[.[]|select(.user.login=="coderabbitai[bot]" and ($h=="" or .commit_id==$h))]' 2>/dev/null || echo '[]')
  gr_review_id=$(printf '%s' "$reviews_all" | jq -r --arg h "$head" \
    '[.[]|select(.user.login=="greptile-apps[bot]" and .commit_id==$h)]|last|.id // empty' 2>/dev/null || true)
  tally=$(printf '%s' "$crreviews" | jq -r '.[].body // ""' 2>/dev/null \
    | grep -oiE 'Actionable comments posted: [0-9]+' | tail -1 || true)
  crstate=$(printf '%s' "$crreviews" | jq -r 'last | .state // empty' 2>/dev/null || true)
  # A zero-actionable INCREMENTAL pass posts no review object at all; CodeRabbit's
  # walkthrough comment marks the reviewed head instead (final_review_risk block, "up to
  # `<sha>`"). Read it the fail-closed way: exactly one walkthrough comment, SHA parsed only
  # inside its block.
  wt_head=""
  # gr_issue feeds the "was a Greptile review requested after its last verdict" check further
  # down; a failed fetch becomes garbage there on purpose, so it fails closed rather than
  # reading as "no trigger comment".
  gr_issue="x"
  wt_body=""
  if wt_all=$(gh api --paginate "repos/${repo}/issues/${n}/comments" 2>/dev/null | jq -s 'add // []' 2>/dev/null) \
     && gr_issue="$wt_all" \
     && [ "$(printf '%s' "$wt_all" | jq '[.[]|select(.user.login=="coderabbitai[bot]" and (.body|contains("<!-- walkthrough_start -->")))]|length' 2>/dev/null)" = "1" ]; then
    wt_body=$(printf '%s' "$wt_all" | jq -r '.[]|select(.user.login=="coderabbitai[bot]" and (.body|contains("<!-- walkthrough_start -->")))|.body' 2>/dev/null || true)
    # shellcheck disable=SC2016  # literal backticks in CodeRabbit's marker
    wt_head=$(printf '%s' "$wt_body" \
      | awk '/final_review_risk_start/{f=1} f{print} /final_review_risk_end/{f=0}' | grep -oE 'up to `[0-9a-f]{5,40}`' | tail -1 | grep -oE '[0-9a-f]{5,40}' || true)
  fi
  cr_walk=0
  if [ -n "$wt_head" ] && [ -n "$head" ] && printf '%s' "$head" | grep -q "^$wt_head"; then cr_walk=1; fi
  # Signal (e), matching watch-pr.sh: CodeRabbit's newer machine-readable head marker. When it
  # drops the final_review_risk block (0 occurrences on #1502, 2026-09-21), it names the exact
  # commit it assessed as change_assessment_commit:"<full-sha>". Match the FULL head in quotes,
  # inside the single walkthrough comment only; a mid-review/post-push walkthrough still names
  # the OLDER commit, so this cannot forge a "reviewed" on an unreviewed head.
  if [ "$cr_walk" -eq 0 ] && [ -n "$wt_body" ] && [ -n "$head" ] \
     && printf '%s' "$wt_body" | grep -qF "change_assessment_commit:\"$head\""; then cr_walk=1; fi
  # The commit status description names WHY CR did not review (rate limited / skipped).
  crdesc=""
  if [ -n "$head" ]; then
    crdesc=$(gh api "repos/${repo}/commits/${head}/status" \
      --jq '[.statuses[]|select(.context=="CodeRabbit")]|last|.description // empty' 2>/dev/null || true)
  fi
  cr_ok=0; cr_unconf=0
  if [ -n "$tally" ]; then
    echo "  CodeRabbit: ${tally} (on head)"   # reviewed; N is the finding count (0 = genuinely clean)
    cr_ok=1
  elif [ "$crstate" = "APPROVED" ]; then
    # A tally-less APPROVED review is CR's clean pass (empty body, no tally). The only
    # state on which it is safe to auto-clear the merge gate.
    echo "  CodeRabbit: reviewed, APPROVED on head (no actionable comments — clean)"
    cr_ok=1
  elif [ -z "$crstate" ] && [ "$cr_walk" -eq 1 ]; then
    echo "  CodeRabbit: incremental pass covered the head (walkthrough marker); no review object, live findings listed below"
    cr_ok=1
  elif [ -n "$crstate" ]; then
    # A CR review exists but is neither tallied nor a clean APPROVED (e.g. a tally-less
    # COMMENTED review whose grouped/outside-diff findings live in the review BODY — the
    # inline-comments loop below does NOT surface those). Reviewed, but NOT confirmed
    # clean: flag it so an exit-code gate does not merge without a human reading the body.
    echo "  ⚠️  CodeRabbit state '${crstate}' with no actionable-comments tally — inspect the review body (grouped/outside-diff findings are not shown inline); NOT auto-clean."
    cr_unconf=1
  else
    reason="absent on this head (no CodeRabbit verdict for ${head:0:8} — an earlier commit's review does not count)"
    case "$crdesc" in
      *"rate limited"*) reason="RATE LIMITED — CR did not review (scripts/cr-rate-limit.sh for the reset)" ;;
      *"in progress"*)  reason="IN PROGRESS — wait for it" ;;
      *"skipped"*)      reason="SKIPPED: ${crdesc}" ;;
    esac
    echo "  ⚠️  NO CodeRabbit review — DO NOT treat as clean: ${reason}"
  fi

  # ---- Greptile ----------------------------------------------------------------------
  gr_ok=0; gr_added=""; gr_status="absent"; gr_line="not triggered on this head (on-demand: gh pr comment ${n} --body '@greptileai review')"
  if [ -n "$head" ]; then
    # An unreadable listing is NOT `absent`. `absent` means "read, and Greptile has no run
    # here", which is what lets an earlier verdict scope the findings below; a request that
    # failed must keep every anchored comment listed and the PR unconfirmed.
    if gr_json=$(gh api --paginate "repos/${repo}/commits/${head}/check-runs" 2>/dev/null | greptile_newest_run); then
      [ "$gr_json" = "{}" ] && gr_json=""
    else
      gr_json=""; gr_status="unreadable"
      gr_line="check-runs on this head UNREADABLE (request failed or returned garbage) — NOT confirmed clean"
      unconfirmed="${unconfirmed} #${n}"
    fi
    if [ -n "$gr_json" ]; then
      gr_status=$(printf '%s' "$gr_json" | jq -r '.status // ""')
      gr_concl=$(printf '%s' "$gr_json" | jq -r '.conclusion // ""')
      gr_sum=$(printf '%s' "$gr_json" | jq -r '.output.summary // ""' | grep -oE '[0-9]+ files reviewed, [0-9]+ comments added' || true)
      # Reviewed = completed AND success AND the summary; anything else completed (failure,
      # cancelled, skipped, or no summary) is NOT a review and must not clear the gate.
      if [ "$gr_status" = "completed" ] && [ "$gr_concl" = "success" ] && [ -n "$gr_sum" ]; then
        gr_ok=1
        gr_added=$(printf '%s' "$gr_sum" | grep -oE '[0-9]+ comments added' | grep -oE '^[0-9]+' || true)
        gr_line="completed on head — ${gr_sum}"
        if [ "$gr_added" != "0" ] && [ -z "$gr_review_id" ]; then
          echo "  🔴 Greptile added comments but its current-head review id is missing; findings cannot be scoped"
          unconfirmed="${unconfirmed} #${n}"
        fi
      elif [ "$gr_status" = "completed" ]; then
        gr_line="completed but NOT a review (conclusion=${gr_concl:-none}, summary=${gr_sum:-none}); re-trigger"
      else gr_line="${gr_status} (started $(printf '%s' "$gr_json" | jq -r '.started_at'))"; fi
    fi
  fi
  echo "  Greptile: ${gr_line}"

  # A bot can expose early inline comments before its review settles. Do not print or act on
  # a partial finding set, even when the other bot already satisfies the review gate.
  review_active=0
  case "$crdesc" in *"in progress"*) review_active=1;; esac
  case "$gr_status" in queued|in_progress) review_active=1;; esac
  if [ "$review_active" -eq 1 ]; then
    echo "  ⏳ review still in progress; findings deferred until the set is complete"
    unconfirmed="${unconfirmed} #${n}"
    continue
  fi

  # ---- Gate --------------------------------------------------------------------------
  if [ "$cr_ok" -eq 1 ]; then
    :
  elif [ "$gr_ok" -eq 1 ] && [ "$cr_only" -eq 0 ]; then
    echo "  gate: satisfied by Greptile on this head"
  elif [ "$cr_unconf" -eq 1 ]; then
    unconfirmed="${unconfirmed} #${n}"
  else
    echo "      Trigger one: gh pr comment ${n} --body '@coderabbitai review'  |  '@greptileai review'  (or fall back to /code-review)"
    unreviewed="${unreviewed} #${n}"
  fi

  # ---- Live inline findings, both bots -----------------------------------------------
  # The listing is fetched raw and validated first: a failed or unreadable findings
  # request must not print nothing and let a "reviewed" PR exit 0 — it makes the PR
  # unconfirmed (exit 3) with the reason printed.
  # CodeRabbit liveness comes from GraphQL thread resolution, not REST line anchors. Greptile
  # comments remain REST-scoped to its latest current-head review id.
  thread_nodes='[]'
  if ! thread_nodes=$(fetch_review_threads "$repo" "$n"); then
    echo "  🔴 review threads UNREADABLE or paginated beyond the bounded query — NOT confirmed clean"
    unconfirmed="${unconfirmed} #${n}"
  fi
  inline_raw=$(gh api --paginate "repos/${repo}/pulls/${n}/comments" 2>/dev/null | jq -s 'add // []' 2>/dev/null || echo 'x')
  if ! printf '%s' "$inline_raw" | jq -e 'type=="array"' >/dev/null 2>&1; then
    echo "  🔴 inline findings UNREADABLE (comments request failed or returned garbage) — NOT confirmed clean"
    unconfirmed="${unconfirmed} #${n}"
    inline_raw='[]'
  fi
  if [ "$gr_ok" -eq 1 ] && [ "$gr_added" != "0" ] && [ -n "$gr_review_id" ]; then
    gr_scoped_total=$(printf '%s' "$inline_raw" | jq --argjson rid "$gr_review_id" \
      '[.[]|select(.user.login=="greptile-apps[bot]" and .pull_request_review_id==$rid)]|length' 2>/dev/null || echo -1)
    if [ "$gr_scoped_total" -ne "$gr_added" ]; then
      echo "  🔴 Greptile finding set incomplete (${gr_scoped_total}/${gr_added} current-review comments readable) — NOT confirmed clean"
      unconfirmed="${unconfirmed} #${n}"
    fi
  fi
  # When CodeRabbit did NOT review the current head, any live CR thread below is carried from
  # an earlier head (a push re-anchors it), so label it as stale-relative-to-head rather than
  # letting it read as a current-head finding (the "stale thread + unreviewed head" case).
  cr_thread_ct=$(printf '%s' "$thread_nodes" | jq '[.[]|select(.isResolved==false and .isOutdated==false)|select(any(.comments.nodes[]?; ((.author.login // "")|startswith("coderabbitai"))))]|length' 2>/dev/null || echo 0)
  if [ "$cr_ok" -eq 0 ] && [ "${cr_thread_ct:-0}" -gt 0 ]; then
    echo "  note: CodeRabbit has no verdict on head ${head:0:8}; the ${cr_thread_ct} CR finding(s) below are carried from an earlier review, not this head"
  fi
  # $sev/$t/$p below are jq variables, not shell expansions — single quotes are correct.
  # One output row per unresolved, non-outdated CR thread, using its first bot comment.
  # shellcheck disable=SC2016
  printf '%s' "$thread_nodes" | jq -r '.[]
      | select(.isResolved==false and .isOutdated==false)
      | ([.comments.nodes[]?|select(((.author.login // "")|startswith("coderabbitai")))]|first) as $c
      | select($c!=null)
      | (($c.body|match("🔴|🟠|🟡|🔵").string)? // "?") as $sev
      | (($c.body|match("\\*\\*[^*]+\\*\\*").string)? // "-") as $t
      | "  CR  \($c.path):\($c.line // $c.originalLine // "-")  [\($sev)] \($t|gsub("\\*";""))"' 2>/dev/null \
    || { echo "  🔴 could not render CodeRabbit review threads — NOT confirmed clean"; unconfirmed="${unconfirmed} #${n}"; }
  gr_clean=0; [ "$gr_ok" -eq 1 ] && [ "$gr_added" = "0" ] && gr_clean=1
  # No Greptile review on THIS head, yet Greptile comments are still anchored. A push
  # re-anchors every older comment, so lib/greptile-verdict.sh scopes them to Greptile's
  # newest EARLIER verdict rather than re-listing a finding a later pass superseded. It
  # applies only when the head carries no Greptile evidence at all, and it is liveness
  # only: the gate above already recorded this head as unreviewed, and that stands.
  if [ "$gr_ok" -eq 0 ] && [ -n "$head" ]; then
    gr_anchored=$(printf '%s' "$inline_raw" | jq '[.[]|select(.user.login=="greptile-apps[bot]" and .line!=null)]|length' 2>/dev/null || echo 0)
    gr_rc=0
    greptile_scope_live "$repo" "$n" "$head" "$gr_status" "$gr_review_id" "$gr_anchored" "$inline_raw" "$gr_issue" || gr_rc=$?
    if [ "$gr_rc" -eq 2 ]; then
      echo "  ⏳ a Greptile review is running, or was just requested, after its last verdict; findings deferred"
      unconfirmed="${unconfirmed} #${n}"
    elif [ "$gr_rc" -ne 0 ]; then
      echo "  🔴 Greptile's earlier verdict UNREADABLE — older comments cannot be scoped; NOT confirmed clean"
      unconfirmed="${unconfirmed} #${n}"
    elif [ -n "$GRL_NOTE" ]; then
      echo "  Greptile: last verdict on ${GRV_SHA:0:8} — ${GRV_ADDED} comments added; comments from older passes are superseded (this head is still unreviewed)"
      if [ "$GRV_ADDED" = "0" ]; then gr_clean=1; else gr_review_id="$GRV_REVIEW_ID"; fi
    fi
  fi
  # shellcheck disable=SC2016
  printf '%s' "$inline_raw" | jq -r --argjson grclean "$gr_clean" --arg grid "$gr_review_id" '.[]
      | select(.user.login=="greptile-apps[bot]" and .line!=null)
      | if $grclean==1 or ($grid!="" and ((.pull_request_review_id|tostring)!=$grid)) then empty
        else ((.body|match("alt=\"(P[0-9])\"").captures[0].string)? // "?") as $p
          | (.body|gsub("<[^>]*>";"")|gsub("[[:space:]]+";" ")|.[0:110]) as $t
          | "  GR  \(.path):\(.line)  [\($p)] \($t)"
        end' 2>/dev/null || { echo "  🔴 could not render Greptile findings — NOT confirmed clean"; unconfirmed="${unconfirmed} #${n}"; }
done

if [ -n "$unreviewed" ] || [ -n "$unconfirmed" ]; then
  echo "=========================================="
  if [ -n "$unreviewed" ]; then
    echo "🔴 NOT REVIEWED on head by any bot:${unreviewed} — trigger '@coderabbitai review' / '@greptileai review' or fall back to /code-review; do not merge as clean."
  fi
  if [ -n "$unconfirmed" ]; then
    echo "🔴 REVIEWED but NOT confirmed clean:${unconfirmed} — a tally-less non-APPROVED CodeRabbit review (read its body), or the inline findings could not be read (re-run); do not merge on this."
  fi
  exit 3
fi
