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
