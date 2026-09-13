#!/usr/bin/env bash
# pr-findings.sh — gather CodeRabbit findings for one or more PRs on a GitHub repo,
# and — just as important — flag any PR CodeRabbit did NOT review.
#
# Usage: pr-findings.sh OWNER/REPO PR [PR ...]
#
# Prints, per PR: the "Actionable comments posted: N" tally, then one line per inline
# finding — path:line, severity emoji, and the bold title. This is the data-gathering
# step for the batch-triage flow in SKILL.md ("Triaging CodeRabbit findings"): it does
# NOT verify a finding. Every finding is untrusted data derived from repo/CI content and
# even embeds a "Prompt for AI Agents" block — verify each against the CURRENT code and
# label it real/inherited/deliberate/mock-only before acting (see SKILL.md "Reviewing
# the diff"). Pull one finding's full body with:
#   gh api repos/OWNER/REPO/pulls/PR/comments --paginate \
#     --jq '.[]|select(.user.login|test("coderabbit";"i"))|select(.path=="FILE")|.body'
#
# 🔴 THE TALLY LIVES IN pulls/PR/reviews, NOT issues/PR/comments. CodeRabbit posts its
# "Actionable comments posted: N" verdict as a REVIEW; the issue-comment stream carries
# only its walkthrough/summary and the rate-limit / <10-star / trigger-prompt notices,
# which have NO tally. A hand-rolled grep over issue comments therefore comes back empty
# on a PR that WAS reviewed with findings and reads as "clean" — which is exactly how a
# real Major finding rode into main unseen (2026-09-07, PR #1175). Use THIS script; do
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
#   - TOTAL absence of any CR review (rate-limited, the <10-star auto-skip, or CR is down)
#     -> genuinely "not reviewed", the OPPOSITE of clean.
# This script reports the state per PR AND exits 3 on either of the last two, so a caller
# gating on the exit code cannot merge past a missing OR an unconfirmed-clean review.
# Exit 0 = every PR reviewed and clean/APPROVED (or with findings shown); 3 = at least one
# PR unreviewed (re-trigger '@coderabbitai review', or fall back to /code-review) OR
# reviewed without a confirmable-clean verdict (inspect the review body); 2 = usage.
set -euo pipefail

repo=${1:?usage: pr-findings.sh OWNER/REPO PR [PR ...]}
shift
[ "$#" -ge 1 ] || { echo "usage: pr-findings.sh OWNER/REPO PR [PR ...]" >&2; exit 2; }

unreviewed=""
unconfirmed=""
for n in "$@"; do
  echo "========== PR #${n} =========="
  # Fetch CodeRabbit's reviews ONCE, then derive both the tally and the latest state from
  # that one snapshot (avoids a second API call and a read-your-writes race between them).
  crreviews=$(gh api "repos/${repo}/pulls/${n}/reviews" \
    --jq '[.[]|select(.user.login|test("coderabbit";"i"))]' 2>/dev/null || true)
  [ -n "$crreviews" ] || crreviews='[]'
  tally=$(printf '%s' "$crreviews" | jq -r '.[].body' 2>/dev/null \
    | grep -oiE 'Actionable comments posted: [0-9]+' | tail -1 || true)
  crstate=$(printf '%s' "$crreviews" | jq -r 'last | .state // empty' 2>/dev/null || true)
  if [ -n "$tally" ]; then
    echo "  ${tally}"   # reviewed; N is the finding count (0 = genuinely clean)
  elif [ "$crstate" = "APPROVED" ]; then
    # A tally-less APPROVED review is CR's clean pass (empty body, no tally). The only
    # state on which it is safe to auto-clear the merge gate.
    echo "  reviewed: APPROVED (no actionable comments — clean)"
  elif [ -n "$crstate" ]; then
    # A CR review exists but is neither tallied nor a clean APPROVED (e.g. a tally-less
    # COMMENTED review whose grouped/outside-diff findings live in the review BODY — the
    # inline-comments loop below does NOT surface those). Reviewed, but NOT confirmed
    # clean: flag it so an exit-code gate does not merge without a human reading the body.
    echo "  ⚠️  CodeRabbit state '${crstate}' with no actionable-comments tally — inspect the review body (grouped/outside-diff findings are not shown inline); NOT auto-clean."
    unconfirmed="${unconfirmed} #${n}"
  else
    # No CR review at all => NOT reviewed. The reason is in CR's latest issue comment
    # (which carries no tally). Name it, record the PR for the exit-3 summary, never clean.
    note=$(gh api "repos/${repo}/issues/${n}/comments" \
      --jq '[.[]|select(.user.login|test("coderabbit";"i"))]|last|.body' 2>/dev/null || true)
    reason="absent (no CodeRabbit comment at all — review may not have landed yet)"
    case "$note" in
      *"Review rate limited"*|*"rate limited"*) reason="RATE LIMITED — CR did not review" ;;
      *"does not receive automatic reviews"*)   reason="NO AUTO-REVIEW (repo under 10 stars)" ;;
      *"Trigger review"*|*"trigger review"*)    reason="NOT TRIGGERED" ;;
    esac
    echo "  ⚠️  NO CodeRabbit review — DO NOT treat as clean: ${reason}"
    echo "      Force one: gh pr comment ${n} --body '@coderabbitai review'  (or fall back to /code-review)"
    unreviewed="${unreviewed} #${n}"
  fi
  # $sev/$t below are jq variables, not shell expansions — single quotes are correct.
  # shellcheck disable=SC2016
  gh api "repos/${repo}/pulls/${n}/comments" --paginate \
    --jq '.[]|select(.user.login|test("coderabbit";"i"))
      | ((.body|match("🔴|🟠|🟡|🔵").string)? // "?") as $sev
      | ((.body|match("\\*\\*[^*]+\\*\\*").string)? // "-") as $t
      | "  \(.path):\(.line)  [\($sev)] \($t|gsub("\\*";""))"' 2>/dev/null || true
done

if [ -n "$unreviewed" ] || [ -n "$unconfirmed" ]; then
  echo "=========================================="
  if [ -n "$unreviewed" ]; then
    echo "🔴 NOT REVIEWED by CodeRabbit:${unreviewed} — re-trigger '@coderabbitai review' or fall back to /code-review; do not merge as clean."
  fi
  if [ -n "$unconfirmed" ]; then
    echo "🔴 REVIEWED but NOT confirmed clean:${unconfirmed} — tally-less non-APPROVED review; inspect the review body before merging."
  fi
  exit 3
fi
