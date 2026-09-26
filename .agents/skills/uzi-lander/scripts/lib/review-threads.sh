#!/usr/bin/env bash
# Shared fail-closed reader for GitHub review-thread resolution state.

fetch_review_threads() { # OWNER/REPO PR -> compact JSON array
  local repo=$1 pr=$2 owner name raw
  owner=${repo%%/*}
  name=${repo#*/}
  [ -n "$owner" ] && [ -n "$name" ] && [ "$owner" != "$name" ] || return 1
  raw=$(gh api graphql -F owner="$owner" -F name="$name" -F number="$pr" -f query='query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{isResolved isOutdated comments(first:20){nodes{databaseId author{login} body path line originalLine} pageInfo{hasNextPage}}} pageInfo{hasNextPage}}}}}' 2>/dev/null) || return 1
  printf '%s' "$raw" | jq -e '
    .data.repository.pullRequest.reviewThreads as $t
    | ((.errors // [])|length)==0
      and ($t|type)=="object"
      and $t.pageInfo.hasNextPage==false
      and all($t.nodes[]; .comments.pageInfo.hasNextPage==false)
  ' >/dev/null 2>&1 || return 1
  printf '%s' "$raw" | jq -c '.data.repository.pullRequest.reviewThreads.nodes'
}

# drop_resolved_comments THREAD_NODES — stdin: `pulls/N/comments` as ONE flat JSON array.
#   Prints that array minus every comment whose thread is resolved. REST line anchors survive
#   a resolve, so a bot comment a human settled still reads as live without this (Greptile
#   on #1710, 2026-09-26; CodeRabbit already reads isResolved). THREAD_NODES is the output of
#   fetch_review_threads. rc 1 on unreadable input.
drop_resolved_comments() {
  jq --argjson t "$1" '
    ([$t[]|select(.isResolved==true)|.comments.nodes[]?.databaseId]) as $r
    | if type=="array" then map(select((.id as $i | any($r[]; . == $i)) | not))
      else error("comments are not an array") end' 2>/dev/null
}
