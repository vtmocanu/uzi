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
# 🔴 ABSENT TALLY != CLEAN. A PR with no review tally was NOT reviewed (rate-limited, the
# <10-star auto-skip, or CR is down) — the OPPOSITE of a clean "Actionable comments
# posted: 0". This script names the specific reason per PR AND exits 3 if ANY named PR was
# not reviewed, so a caller that gates on the exit code cannot merge past a missing review.
# Exit 0 = every PR reviewed (findings or clean); 3 = at least one PR unreviewed
# (re-trigger '@coderabbitai review', or fall back to /code-review); 2 = usage.
set -euo pipefail

repo=${1:?usage: pr-findings.sh OWNER/REPO PR [PR ...]}
shift
[ "$#" -ge 1 ] || { echo "usage: pr-findings.sh OWNER/REPO PR [PR ...]" >&2; exit 2; }

unreviewed=""
for n in "$@"; do
  echo "========== PR #${n} =========="
  tally=$(gh api "repos/${repo}/pulls/${n}/reviews" \
    --jq '.[]|select(.user.login|test("coderabbit";"i"))|.body' 2>/dev/null \
    | grep -oiE 'Actionable comments posted: [0-9]+' | tail -1 || true)
  if [ -n "$tally" ]; then
    echo "  ${tally}"   # reviewed; N is the finding count (0 = genuinely clean)
  else
    # No tally => NOT reviewed. The reason is in CR's latest issue comment (which carries
    # no tally). Name it, record the PR for the exit-3 summary, never read it as clean.
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

if [ -n "$unreviewed" ]; then
  echo "=========================================="
  echo "🔴 NOT REVIEWED by CodeRabbit:${unreviewed} — do not merge as clean (exit 3)."
  exit 3
fi
