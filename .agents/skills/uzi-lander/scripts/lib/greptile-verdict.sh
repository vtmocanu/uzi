#!/usr/bin/env bash
# lib/greptile-verdict.sh — Greptile's newest EARLIER verdict on a PR, for finding LIVENESS.
# Source it; do not execute.
#
# Greptile is on demand here, so every push leaves the new head with no `Greptile Review`
# check-run, while GitHub re-anchors every older Greptile comment onto that head
# (`line != null`). Scoping liveness to the current head alone then counts, as live, a
# finding that a LATER Greptile pass already superseded: fix the finding, get a clean
# re-review, push a docs commit, and the fixed P1 is "live" again until a fresh credit is
# spent (PR #1449, 2026-09-19).
#
# greptile_prior_verdict REPO PR HEAD
#   Walks the PR's commits newest first, skipping HEAD, at most GREPTILE_PRIOR_MAX of them
#   (default 20), and stops at the first carrying a `Greptile Review` check-run that is a
#   REVIEW by the callers' own definition: completed, conclusion success, and the
#   "N files reviewed, M comments added" summary. Sets:
#     GRV_SHA        that commit, or empty when no earlier verdict exists in the window
#     GRV_ADDED      M
#     GRV_REVIEW_ID  the greptile review object on GRV_SHA (only when M > 0; a clean pass
#                    posts no review object)
#   rc 0  the answer is trustworthy, including "none found" (GRV_SHA empty).
#   rc 1  a lookup failed, or M > 0 with no review object to scope by. Callers fail closed.
#
# LIVENESS ONLY. A verdict on an earlier commit says nothing about the commits after it, so
# whether the current head was reviewed stays an exact-head question the callers answer
# themselves; this never satisfies a review gate.

greptile_prior_verdict() {
  local repo="$1" pr="$2" head="$3" max="${GREPTILE_PRIOR_MAX:-20}"
  local pages shas sha run status concl sum
  GRV_SHA=""; GRV_ADDED=""; GRV_REVIEW_ID=""

  pages=$(gh api --paginate "repos/$repo/pulls/$pr/commits" 2>/dev/null) || return 1
  shas=$(printf '%s' "$pages" | jq -rs --arg h "$head" --argjson max "$max" \
    'if all(.[]; type=="array") then
       [.[][]|.sha|select(type=="string" and . != $h)]|reverse|.[0:$max]|.[]
     else error("commit pages are not arrays") end' 2>/dev/null) || return 1

  while IFS= read -r sha; do
    [ -n "$sha" ] || continue
    pages=$(gh api --paginate "repos/$repo/commits/$sha/check-runs" 2>/dev/null) || return 1
    run=$(printf '%s' "$pages" | jq -s \
      'if length>0 and all(.[]; type=="object" and has("check_runs")) then
         [.[].check_runs[]|select(.app.slug=="greptile-apps" and .name=="Greptile Review")]|last // {}
       else error("check-run pages unreadable") end' 2>/dev/null) || return 1
    status=$(printf '%s' "$run" | jq -r '.status // ""' 2>/dev/null) || return 1
    concl=$(printf '%s' "$run" | jq -r '.conclusion // ""' 2>/dev/null) || return 1
    sum=$(printf '%s' "$run" | jq -r '.output.summary // ""' 2>/dev/null \
      | grep -oE '[0-9]+ files reviewed, [0-9]+ comments added' || true)
    if [ "$status" = "completed" ] && [ "$concl" = "success" ] && [ -n "$sum" ]; then
      GRV_SHA="$sha"
      GRV_ADDED=$(printf '%s' "$sum" | grep -oE '[0-9]+ comments added' | grep -oE '^[0-9]+' || true)
      break
    fi
  done <<< "$shas"

  [ -n "$GRV_SHA" ] || return 0
  [ -n "$GRV_ADDED" ] || return 1
  [ "$GRV_ADDED" = "0" ] && return 0

  pages=$(gh api --paginate "repos/$repo/pulls/$pr/reviews" 2>/dev/null) || return 1
  GRV_REVIEW_ID=$(printf '%s' "$pages" | jq -rs --arg s "$GRV_SHA" \
    'if all(.[]; type=="array") then
       [.[][]|select(.user.login=="greptile-apps[bot]" and .commit_id==$s)]|last|.id // empty
     else error("review pages are not arrays") end' 2>/dev/null) || return 1
  [ -n "$GRV_REVIEW_ID" ] || return 1
  return 0
}
