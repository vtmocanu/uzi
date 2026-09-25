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
# LIVENESS ONLY for greptile_prior_verdict / greptile_scope_live. A verdict on an earlier
# commit says nothing about the commits after it, so whether the current head was reviewed
# stays an exact-head question. greptile_paired_verdict is the one exception, and still an
# exact-head answer: it proves a review OF the head whose check-run landed on an older commit.

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

# greptile_body_sha — stdin: `gh api repos/O/R/pulls/N` (one JSON object). Prints the full
#   40-hex SHA of the "Last reviewed commit" `/commit/<sha>` link inside Greptile's PR-body
#   block, or nothing. The block is the text between exactly one `<!-- greptile_comment -->`
#   and one later `<!-- /greptile_comment -->`; the SHA must be the only one named on
#   "Last reviewed commit" lines there. A missing block, a short or ambiguous SHA, or a
#   malformed payload prints nothing (no evidence, never clean); rc 1 only when stdin is not
#   a JSON object.
greptile_body_sha() {
  jq -r 'if type!="object" then error("pull is not an object") else
    (.body // "") as $b
    | if ($b|type)!="string" then "" else
        ($b|gsub("\r";"")) as $s
        | if ([$s|match("<!-- greptile_comment -->";"g")]|length)!=1
             or ([$s|match("<!-- /greptile_comment -->";"g")]|length)!=1 then ""
          else ($s|split("<!-- greptile_comment -->")[1]|split("<!-- /greptile_comment -->")) as $p
            | if ($p|length)!=2 then ""
              else [$p[0]|split("\n")[]|select(contains("Last reviewed commit"))
                     |scan("/commit/([0-9a-f]+)")[0]]|unique
                | if length==1 and (.[0]|test("^[0-9a-f]{40}$")) then .[0] else "" end
              end
          end
      end
  end' 2>/dev/null
}

# greptile_paired_verdict REPO PR HEAD HEAD_REVIEW_ID
#   Greptile's verdict on HEAD when the head carries no completed `Greptile Review` run of its
#   own. A push that races `@greptileai review` leaves the run on the OLDER commit while
#   Greptile reviews the new head (PR #1698, 2026-09-25), and a clean pass posts no review
#   object, so the head reads unreviewed although it was reviewed clean.
#   Marker, in order: a greptile-apps[bot] review whose commit_id is HEAD (HEAD_REVIEW_ID,
#   passed by the caller; exists only when Greptile added inline comments), else the PR-body
#   "Last reviewed commit" SHA (greptile_body_sha) equal to HEAD. The PR body is user-editable
#   and can be replaced wholesale, so the marker alone is never enough: it must pair with a
#   `Greptile Review` run (app greptile-apps) on some commit among the newest
#   GREPTILE_PRIOR_MAX (default 20) of the PR, completed, conclusion success, summary
#   "N files reviewed, M comments added", and completed_at at or after HEAD's committer date.
#   The committer date is a lower bound on the push time (a commit cannot be pushed before it
#   is made), so a run that finished earlier cannot have seen HEAD; it is client-set, so a
#   skewed or rewritten date weakens, and never tightens, that bound. The newest completed_at
#   among the qualifying runs gives M. Sets:
#     GRP_SHA      the commit the paired run sits on; empty when there is no evidence
#     GRP_SUMMARY  its "N files reviewed, M comments added"
#     GRP_ADDED    M
#     GRP_NOTE     `<review|body>→<head8> via run on <sha8>`, for the poll log
#   rc 0 read (GRP_SHA empty = no evidence: no marker, no pairing run, or HEAD's committer
#   date missing); rc 1 a lookup failed or the commit list does not end at HEAD. Never cached:
#   a newer run can still complete.
# shellcheck disable=SC2034  # GRP_* are this function's outputs, read by the sourcing scripts.
greptile_paired_verdict() {
  local repo="$1" pr="$2" head="$3" head_rid="$4" max="${GREPTILE_PRIOR_MAX:-20}"
  local kind pull body_sha pages commits head_date sha run best="" best_at="" best_sha=""
  GRP_SHA=""; GRP_SUMMARY=""; GRP_ADDED=""; GRP_NOTE=""
  if [ -n "$head_rid" ]; then
    kind=review
  else
    pull=$(gh api "repos/$repo/pulls/$pr" 2>/dev/null) || return 1
    body_sha=$(printf '%s' "$pull" | greptile_body_sha) || return 1
    [ "$body_sha" = "$head" ] || return 0
    kind=body
  fi

  pages=$(gh api --paginate "repos/$repo/pulls/$pr/commits" 2>/dev/null) || return 1
  commits=$(printf '%s' "$pages" | jq -cs --arg h "$head" \
    'if length>0 and all(.[]; type=="array") then
       [.[][]] | if (last.sha // "") != $h then error("commit list does not end at the head") else . end
     else error("commit pages are not arrays") end' 2>/dev/null) || return 1
  head_date=$(printf '%s' "$commits" | jq -r 'last.commit.committer.date // ""' 2>/dev/null) || return 1
  [ -n "$head_date" ] || return 0

  while IFS= read -r sha; do
    [ -n "$sha" ] || continue
    pages=$(gh api --paginate "repos/$repo/commits/$sha/check-runs" 2>/dev/null) || return 1
    # The newest qualifying run on this commit, as {at, summary}, or empty.
    run=$(printf '%s' "$pages" | jq -cs --arg d "$head_date" '
      def ts: (sub("\\.[0-9]+";"")|fromdateiso8601)? // null;
      if length>0 and all(.[]; type=="object" and has("check_runs")) then
        ($d|ts) as $floor
        | if $floor==null then error("head committer date unparseable") else
            [.[].check_runs[]
             | select(.app.slug=="greptile-apps" and .name=="Greptile Review"
                      and .status=="completed" and .conclusion=="success")
             | ((.output.summary // "")|[match("[0-9]+ files reviewed, [0-9]+ comments added").string]|first) as $sum
             | select($sum!=null)
             | ((.completed_at // "")|ts) as $at
             | select($at!=null and $at >= $floor)
             | {at:$at, summary:$sum}]
            | max_by(.at) // empty
          end
      else error("check-run pages unreadable") end' 2>/dev/null) || return 1
    [ -n "$run" ] || continue
    if [ -z "$best" ] || [ "$(printf '%s' "$run" | jq -r .at)" -gt "$best_at" ]; then
      best="$run"; best_at=$(printf '%s' "$run" | jq -r .at); best_sha="$sha"
    fi
  done < <(printf '%s' "$commits" | jq -r --argjson max "$max" 'reverse|.[0:$max]|.[].sha')

  [ -n "$best" ] || return 0
  GRP_SHA="$best_sha"
  GRP_SUMMARY=$(printf '%s' "$best" | jq -r .summary)
  GRP_ADDED=$(printf '%s' "$GRP_SUMMARY" | grep -oE '[0-9]+ comments added' | grep -oE '^[0-9]+')
  GRP_NOTE="${kind}→${head:0:8} via run on ${GRP_SHA:0:8}"
  return 0
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

# greptile_outside_diff HEAD — stdin: `issues/N/comments` as ONE flat JSON array.
#   Greptile posts findings on lines the diff does not cover as a single ISSUE comment
#   marked `<!-- greptile_outside_diff -->` (one `- ` bullet per finding, each linking
#   `/blob/<sha>/path#L<n>`), not as review comments, and edits it in place: a bullet leaves
#   once its file changes. Its check-run tally ("M comments added") counts these bullets, so
#   a pass whose findings are all outside the diff posts no review object at all.
#   Reads the NEWEST such comment. Sets:
#     GOD_TOTAL  bullets still listed (all live)
#     GOD_HEAD   bullets linking HEAD, i.e. added by the pass on HEAD
#     GOD_LINES  one rendered `  GR  path:line  [Pn] title (outside diff)` row per bullet
#   rc 0 read (zero when there is no such comment); rc 1 unreadable input.
# shellcheck disable=SC2034  # GOD_* are this function's outputs, read by the sourcing scripts.
greptile_outside_diff() {
  local head="$1" json
  json=$(jq --arg h "$head" '
    if type!="array" then error("issue comments are not an array") else
      ([.[]|select(.user.login=="greptile-apps[bot]"
              and ((.body // "")|contains("<!-- greptile_outside_diff -->")))]
       | if length==0 then [] else (max_by(.id).body|split("\n")|map(select(startswith("- ")))) end) as $b
      | {total: ($b|length),
         head: ([$b[]|select(contains("/blob/" + $h + "/"))]|length),
         lines: [$b[]
           | ((match("alt=\"(P[0-9])\"").captures[0].string)? // "?") as $p
           | ((match("\\*\\*([^*]+)\\*\\*").captures[0].string)? // "-") as $t
           | ((match("`([^`]+)`").captures[0].string)? // "-") as $loc
           | "  GR  \($loc)  [\($p)] \($t) (outside diff)"]}
    end' 2>/dev/null) || return 1
  GOD_TOTAL=$(printf '%s' "$json" | jq -r '.total')
  GOD_HEAD=$(printf '%s' "$json" | jq -r '.head')
  GOD_LINES=$(printf '%s' "$json" | jq -r '.lines[]')
  return 0
}
