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
# PREMISE: a Greptile pass reviews the WHOLE PR diff, not the delta since its last pass, so
# "0 comments added" speaks for every older comment. Measured on PR #1449: both passes
# reported the PR's full changed-file count (249, then 247) while the delta between them
# was 170 files. If Greptile ever reviews incrementally this rule is wrong; the current-head
# zeroing in the callers rests on the same premise.
#
# LIVENESS ONLY. A verdict on an earlier commit says nothing about the commits after it, so
# whether the current head was reviewed stays an exact-head question the callers answer
# themselves; nothing here may satisfy a review gate.

# greptile_newest_run — stdin: `gh api --paginate .../check-runs` pages. Prints the NEWEST
# `Greptile Review` run as JSON, or {} when there is none; fails on an unreadable payload.
# The listing is newest first and a re-trigger adds a second same-named run on one commit,
# so the newest is max id, never `last`.
greptile_newest_run() {
  jq -s 'if length>0 and all(.[]; type=="object" and has("check_runs")) then
           [.[].check_runs[]|select(.app.slug=="greptile-apps" and .name=="Greptile Review")]
           | if length==0 then {} else max_by(.id) end
         else error("check-run pages unreadable") end' 2>/dev/null
}

# greptile_prior_verdict REPO PR HEAD
#   Walks the PR's commits newest first, skipping HEAD, at most GREPTILE_PRIOR_MAX of them
#   (default 20), and stops at the first carrying a `Greptile Review` check-run that is a
#   REVIEW by the callers' own definition: completed, conclusion success, and the
#   "N files reviewed, M comments added" summary. Sets:
#     GRV_SHA        that commit, or empty when no earlier verdict exists in the window
#     GRV_ADDED      M
#     GRV_STARTED    that run's started_at (empty when the API omits it)
#     GRV_REVIEW_ID  the greptile review object on GRV_SHA (only when M > 0; a clean pass
#                    posts no review object)
#   rc 0  the answer is trustworthy, including "none found" (GRV_SHA empty).
#   rc 1  a lookup failed; the commit list does not end at HEAD (truncated past the API's
#         250-commit cap, or the head moved); or M > 0 with no review object to scope by.
#   rc 2  a Greptile run on a commit NEWER than any verdict is still queued / in progress.
#         An older verdict must not speak over a review that is running: defer.
#   Callers fail closed on 1 and 2. A definitive answer (rc 0) is memoised per REPO/PR/HEAD,
#   since a completed verdict on an earlier commit cannot change; 1 and 2 are never cached.
greptile_prior_verdict() {
  local repo="$1" pr="$2" head="$3" max="${GREPTILE_PRIOR_MAX:-20}"
  local key="$1/$2/$3" pages shas sha run status concl sum
  if [ "${GRV_CACHE_KEY:-}" = "$key" ]; then
    GRV_SHA="$GRV_CACHE_SHA"; GRV_ADDED="$GRV_CACHE_ADDED"; GRV_REVIEW_ID="$GRV_CACHE_REVIEW_ID"; GRV_STARTED="$GRV_CACHE_STARTED"
    return 0
  fi
  GRV_SHA=""; GRV_ADDED=""; GRV_REVIEW_ID=""; GRV_STARTED=""

  pages=$(gh api --paginate "repos/$repo/pulls/$pr/commits" 2>/dev/null) || return 1
  shas=$(printf '%s' "$pages" | jq -rs --arg h "$head" --argjson max "$max" \
    'if length>0 and all(.[]; type=="array") then
       [.[][]|.sha|select(type=="string")]
       | if (last // "") != $h then error("commit list does not end at the head") else . end
       | .[0:-1] | reverse | .[0:$max] | .[]
     else error("commit pages are not arrays") end' 2>/dev/null) || return 1

  while IFS= read -r sha; do
    [ -n "$sha" ] || continue
    pages=$(gh api --paginate "repos/$repo/commits/$sha/check-runs" 2>/dev/null) || return 1
    run=$(printf '%s' "$pages" | greptile_newest_run) || return 1
    status=$(printf '%s' "$run" | jq -r '.status // ""' 2>/dev/null) || return 1
    concl=$(printf '%s' "$run" | jq -r '.conclusion // ""' 2>/dev/null) || return 1
    case "$status" in queued|in_progress) return 2 ;; esac
    sum=$(printf '%s' "$run" | jq -r '.output.summary // ""' 2>/dev/null \
      | grep -oE '[0-9]+ files reviewed, [0-9]+ comments added' || true)
    if [ "$status" = "completed" ] && [ "$concl" = "success" ] && [ -n "$sum" ]; then
      GRV_SHA="$sha"
      GRV_ADDED=$(printf '%s' "$sum" | grep -oE '[0-9]+ comments added' | grep -oE '^[0-9]+' || true)
      GRV_STARTED=$(printf '%s' "$run" | jq -r '.started_at // ""' 2>/dev/null) || return 1
      break
    fi
  done <<< "$shas"

  if [ -n "$GRV_SHA" ]; then
    [ -n "$GRV_ADDED" ] || return 1
    if [ "$GRV_ADDED" != "0" ]; then
      pages=$(gh api --paginate "repos/$repo/pulls/$pr/reviews" 2>/dev/null) || return 1
      # Reviews list oldest first, so `last` IS the newest here (unlike check-runs).
      GRV_REVIEW_ID=$(printf '%s' "$pages" | jq -rs --arg s "$GRV_SHA" \
        'if length>0 and all(.[]; type=="array") then
           [.[][]|select(.user.login=="greptile-apps[bot]" and .commit_id==$s)]|last|.id // empty
         else error("review pages are not arrays") end' 2>/dev/null) || return 1
      [ -n "$GRV_REVIEW_ID" ] || return 1
    fi
  fi
  GRV_CACHE_KEY="$key"; GRV_CACHE_SHA="$GRV_SHA"; GRV_CACHE_ADDED="$GRV_ADDED"; GRV_CACHE_REVIEW_ID="$GRV_REVIEW_ID"; GRV_CACHE_STARTED="$GRV_STARTED"
  return 0
}

# greptile_scope_live REPO PR HEAD HEAD_STATE HEAD_REVIEW_ID RAW_LIVE COMMENTS ISSUE_COMMENTS
#   The ONE liveness decision for a head Greptile has not reviewed, shared by watch-pr.sh,
#   pr-findings.sh and takeover.sh so they decide the same way about the same PR.
#     HEAD_STATE      the head's Greptile check-run status; `absent` ONLY when the listing was
#                     read and holds no Greptile run. A failed or unreadable listing is not
#                     `absent`: pass any other word (the callers use `unreadable`).
#     HEAD_REVIEW_ID  the greptile review object whose commit_id is the head, or empty
#     RAW_LIVE        how many Greptile comments are still anchored (`line != null`)
#     COMMENTS        `pulls/N/comments` as ONE flat JSON array
#     ISSUE_COMMENTS  `issues/N/comments` as ONE flat JSON array
#   Sets GRL_LIVE and GRL_NOTE (`<sha8>/<added>` when an earlier verdict scoped the count).
#   The earlier verdict applies ONLY when the head carries no Greptile evidence at all: no
#   check-run in any state and no review object. Anything Greptile said or is saying about
#   THIS head outranks what it said about an older one, so every other case keeps RAW_LIVE.
#   A review REQUESTED after that verdict also outranks it: `@greptileai review` shows up as
#   a check-run only ~12 s later, and until then the head reads `absent`. A trigger comment
#   newer than the verdict defers for GREPTILE_TRIGGER_GRACE seconds (default 600); past
#   that Greptile is evidently not coming (out of credits, app down) and the verdict stands.
#   rc 0 decided; rc 1 unknown; rc 2 a newer Greptile review is running or was just requested.
# shellcheck disable=SC2034  # GRL_LIVE / GRL_NOTE are this function's outputs: the scripts that source this file read them, which shellcheck cannot see when linting the lib alone.
greptile_scope_live() {
  local repo="$1" pr="$2" head="$3" state="$4" head_rid="$5" raw="$6" comments="$7" issue_comments="$8"
  local rc=0 requested grace="${GREPTILE_TRIGGER_GRACE:-600}"
  GRL_LIVE="$raw"; GRL_NOTE=""
  [ "$raw" -gt 0 ] || return 0
  if [ "$state" != "absent" ] || [ -n "$head_rid" ]; then return 0; fi
  greptile_prior_verdict "$repo" "$pr" "$head" || rc=$?
  [ "$rc" -eq 0 ] || return "$rc"
  [ -n "$GRV_SHA" ] || return 0
  requested=$(printf '%s' "$issue_comments" | jq --arg since "$GRV_STARTED" --argjson grace "$grace" \
    'if type=="array" then
       # Bot comments are skipped so a bot QUOTING the phrase cannot defer a lander. This assumes
       # the trigger is posted by a user account; a lander posting through a GitHub App or
       # GITHUB_TOKEN would be typed Bot and must be allowed here by login instead.
       [.[]|select((.user.type // "") != "Bot")
           |select((.body // "")|test("@greptile(ai)?\\s+review"; "i"))
           |(.created_at|fromdateiso8601) as $t
           |select($since == "" or $t > ($since|fromdateiso8601))
           |select((now - $t) < $grace)]|length
     else error("issue comments are not an array") end' 2>/dev/null) || return 1
  [ "$requested" -eq 0 ] || return 2
  GRL_NOTE="${GRV_SHA:0:8}/${GRV_ADDED}"
  if [ "$GRV_ADDED" = "0" ]; then
    GRL_LIVE=0
  else
    GRL_LIVE=$(printf '%s' "$comments" | jq --argjson rid "$GRV_REVIEW_ID" \
      'if type=="array" then
         [.[]|select(.user.login=="greptile-apps[bot]" and .pull_request_review_id==$rid and .line!=null)]|length
       else error("comments are not an array") end' 2>/dev/null) || { GRL_LIVE="$raw"; return 1; }
  fi
  return 0
}
