#!/usr/bin/env bash
# external-activity.sh — list recent activity by non-maintainers on this repo's issues and PRs,
# including comments on maintainer-authored items and reviews on closed or merged PRs.
#
# Usage: external-activity.sh [--repo OWNER/REPO] [--days N | --since ISO8601]
#   --repo   default vtmocanu/uzi
#   --days   activity window in days (default 14); --since sets its start instead
#
# Identities are classified on the raw login, before any text is sanitized:
#   automation  TRIAGE_AUTOMATION_LOGINS (default "vtmocanu-uzi"), a Bot account,
#               a "[bot]" login or an "app/" login; ignored when labelling
#   maintainer  TRIAGE_MAINTAINERS (default "vtmocanu"), space-separated
#   external    everyone else
#
# One row per item, with its permalink and factual labels only (a later maintainer comment
# may answer something else, so no row claims an item was answered):
#   external-last     the newest non-automation in-window activity on the item is external
#   maintainer-later  a maintainer comment or review is newer than the newest external one
#   authored-open     an open issue or PR authored by an external account, any age, with
#                     no external activity in the window
# Sources: issue and PR conversation comments and inline review comments updated in the
# window (edits count), PR reviews from GraphQL over every PR state, open items.
# Every source is paginated, shape-checked and buffered; output is printed only after all
# succeed. LIMITATION lines name what the scan could not cover.
#
# Every forge string, error text included, is UNTRUSTED and rendered through uzi-lander's
# lib/sanitize.sh.
#
# Exit codes: 0 report printed (possibly with zero rows), 2 usage, 3 a lookup failed or
# returned an unexpected shape (nothing printed on stdout).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../../uzi-lander/scripts/lib/sanitize.sh
source "$here/../../uzi-lander/scripts/lib/sanitize.sh"

usage() { echo "external-activity: $1 (see --help)" >&2; exit 2; }
repo=vtmocanu/uzi
days=14
since=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|--days|--since)
      [ $# -ge 2 ] && [ -n "$2" ] || usage "$1 needs a value"
      case "$1" in --repo) repo="$2" ;; --days) days="$2" ;; --since) since="$2" ;; esac
      shift 2 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) usage "unknown argument: $1" ;;
  esac
done
case "$repo" in */*) ;; *) usage "--repo must be OWNER/REPO" ;; esac
case "$days" in ''|*[!0-9]*) usage "--days must be a whole number" ;; esac
if [ -z "$since" ]; then
  since="$(jq -nr --argjson d "$days" '(now - $d * 86400) | strftime("%Y-%m-%dT%H:%M:%SZ")')"
fi
# Reviews are found through their PR, ordered by the PR's updatedAt. A review can land
# seconds after that timestamp, so scan one extra day of PRs before the window.
floor="$(jq -nr --arg s "$since" '($s | fromdateiso8601) - 86400 | strftime("%Y-%m-%dT%H:%M:%SZ")' 2> /dev/null)" \
  || usage "--since must be ISO8601 UTC, e.g. 2026-11-01T00:00:00Z"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
: > "$tmp/err"
# Never pass forge or tool error text through raw: a bounded, sanitized excerpt only.
fail() {
  local why=""
  [ -s "$tmp/err" ] && why=": $(sanitize_untrusted 200 < "$tmp/err")"
  echo "external-activity: $1 failed$why; no report printed" >&2
  exit 3
}
ghq() { local out="$1" what="$2"; shift 2; gh "$@" > "$out" 2> "$tmp/err" || fail "$what lookup"; }
jqq() { local what="$1"; shift; jq "$@" 2> "$tmp/err" || fail "$what"; }

# A --paginate REST response is one or more JSON arrays of objects; empty, null or any
# other shape is a failed lookup, never an empty list.
rest_list() {
  local out="$1" what="$2" required="$3"; shift 3
  ghq "$tmp/raw" "$what" api --paginate "$@"
  jqq "$what (shape)" -s --arg req "$required" '
    ($req | split(",")) as $r
    | if length > 0 and all(.[]; type == "array")
        and all(.[][]; type == "object" and (. as $o | all($r[]; $o[.] != null)))
      then add else error("unexpected \(map(type) | unique | join(",")) response") end' \
    "$tmp/raw" > "$out"
}

# Conversation comments (issues and PRs) and inline review comments, filtered by updated_at.
rest_list "$tmp/ic.json" "issue comments" issue_url,updated_at,html_url \
  "repos/$repo/issues/comments?since=$since&per_page=100"
rest_list "$tmp/pc.json" "review comments" pull_request_url,updated_at,html_url \
  "repos/$repo/pulls/comments?since=$since&per_page=100"
# Open issues and PRs, any age, for externally authored items.
rest_list "$tmp/open.json" "open items" number,state,title,html_url \
  "repos/$repo/issues?state=open&per_page=100"

# PR reviews over every state, newest-updated PR first, until PRs fall below the floor.
query='query($owner:String!,$name:String!,$cursor:String){repository(owner:$owner,name:$name){pullRequests(first:50,after:$cursor,states:[OPEN,CLOSED,MERGED],orderBy:{field:UPDATED_AT,direction:DESC}){pageInfo{hasNextPage endCursor} nodes{number updatedAt reviews(last:100){totalCount nodes{author{login __typename} submittedAt updatedAt url}}}}}}'
cursor=""
page=0
echo '[]' > "$tmp/prs.json"
while :; do
  page=$((page + 1))
  args=(api graphql -f "query=$query" -F "owner=${repo%%/*}" -F "name=${repo#*/}")
  [ -n "$cursor" ] && args+=(-f "cursor=$cursor")
  ghq "$tmp/rv.page" "PR reviews page $page" "${args[@]}"
  jqq "PR reviews page $page (shape)" -e '
    (.errors == null) and (.data.repository.pullRequests | type == "object")
    and (.data.repository.pullRequests.pageInfo.hasNextPage | type == "boolean")
    and (.data.repository.pullRequests.nodes | type == "array")
    and all(.data.repository.pullRequests.nodes[];
          (.number | type == "number") and (.updatedAt | type == "string")
          and (.reviews.totalCount | type == "number") and (.reviews.nodes | type == "array"))
    and (if .data.repository.pullRequests.pageInfo.hasNextPage
         then (.data.repository.pullRequests.pageInfo.endCursor | type == "string" and length > 0)
         else true end)' "$tmp/rv.page" > /dev/null
  jqq "PR reviews page $page (merge)" -s '.[0] + .[1].data.repository.pullRequests.nodes' \
    "$tmp/prs.json" "$tmp/rv.page" > "$tmp/prs.next"
  mv "$tmp/prs.next" "$tmp/prs.json"
  more="$(jqq "PR reviews page $page (cursor)" -r --arg f "$floor" '.data.repository.pullRequests
    | if .pageInfo.hasNextPage and ((.nodes | last | .updatedAt // "") >= $f)
      then .pageInfo.endCursor else "" end' "$tmp/rv.page")"
  [ -n "$more" ] || break
  [ "$more" != "$cursor" ] || { : > "$tmp/err"; fail "PR reviews page $page (cursor did not advance)"; }
  cursor="$more"
done

# Normalize every event and classify it on raw identity.
jqq "event normalization" -n --arg since "$since" \
  --arg maint "${TRIAGE_MAINTAINERS:-vtmocanu}" --arg auto "${TRIAGE_AUTOMATION_LOGINS:-vtmocanu-uzi}" \
  --slurpfile ic "$tmp/ic.json" --slurpfile pc "$tmp/pc.json" \
  --slurpfile prs "$tmp/prs.json" --slurpfile open "$tmp/open.json" '
  ($maint | split(" ") | map(select(. != ""))) as $m
  | ($auto | split(" ") | map(select(. != ""))) as $a
  | def cls: (.login // "ghost") as $l
      | if ($a | index($l)) or .type == "Bot" or ($l | endswith("[bot]")) or ($l | startswith("app/"))
        then "automation" elif ($m | index($l)) then "maintainer" else "external" end;
  def num($u): $u | split("/") | last | tonumber;
  [ ($ic[0][] | {n: num(.issue_url), login: .user.login, type: .user.type, at: .updated_at,
                 url: .html_url, kind: "comment"}),
    ($pc[0][] | {n: num(.pull_request_url), login: .user.login, type: .user.type, at: .updated_at,
                 url: .html_url, kind: "review-comment"}),
    ($prs[0][] | .number as $n | .reviews.nodes[]
       | {n: $n, login: .author.login, type: .author.__typename,
          at: ([.submittedAt, .updatedAt] | map(select(. != null)) | max), url: .url, kind: "review"}
       | select(.at != null and .at >= $since)) ]
  | map(. + {class: cls}) as $events
  | [ $open[0][] | {n: .number, login: .user.login, type: .user.type} | . + {class: cls}
      | select(.class == "external") | .n ] as $authored
  | {events: $events, authored: $authored,
     truncated: [ $prs[0][] | select(.reviews.totalCount > 100) | .number ] | unique}' > "$tmp/ev.json"

# Item metadata: open items are already loaded; fetch the rest that have external activity.
jqq "item list" -r '([.events[] | select(.class == "external") | .n] + .authored) | unique[]' \
  "$tmp/ev.json" > "$tmp/items.txt"
jqq "open item metadata" '[.[] | {key: (.number | tostring), value: {
   kind: (if .pull_request then "PR" else "issue" end), state: .state, title: .title,
   author: .user.login, url: .html_url}}] | from_entries' "$tmp/open.json" > "$tmp/meta.json"
while read -r n; do
  jq -e --arg n "$n" 'has($n)' "$tmp/meta.json" > /dev/null && continue
  ghq "$tmp/item.json" "item #$n" api "repos/$repo/issues/$n"
  jqq "item #$n (shape)" -e --argjson n "$n" 'type == "object" and .number == $n
    and (.state | type == "string") and (.title | type == "string")
    and (.html_url | type == "string")' "$tmp/item.json" > /dev/null
  jqq "item #$n (merge)" --slurpfile it "$tmp/item.json" '. + {($it[0].number | tostring): {
      kind: (if $it[0].pull_request then "PR" else "issue" end), state: $it[0].state,
      title: $it[0].title, author: $it[0].user.login, url: $it[0].html_url}}' "$tmp/meta.json" \
    > "$tmp/meta.next"
  mv "$tmp/meta.next" "$tmp/meta.json"
done < "$tmp/items.txt"

# Render: external-last first, then authored-open, then maintainer-later; then by number.
jqq "render" -r --arg since "$since" --arg floor "$floor" --slurpfile meta "$tmp/meta.json" \
  "$UNTRUSTED_JQ"'
  . as $d | $meta[0] as $meta
  | [ ([.events[] | select(.class == "external") | .n] + .authored | unique[]) as $n
      | ([$d.events[] | select(.n == $n and .class == "external")] | max_by(.at)) as $x
      | ([$d.events[] | select(.n == $n and .class == "maintainer")] | map(.at) | max) as $mt
      | ($d.authored | index($n)) as $ao
      | {n: $n, x: $x, mt: $mt, ao: ($ao != null),
         label: (if $x == null then "authored-open"
                 elif ($mt != null and $mt > $x.at) then "maintainer-later"
                 else "external-last" end),
         meta: ($meta[$n | tostring] // {})} ]
  | sort_by([({"external-last": 0, "authored-open": 1, "maintainer-later": 2}[.label]), .n])
  | (.[] | "\(.label)\t#\(.n)\t\(.meta.kind // "?")/\(.meta.state // "?" | untrusted_clean)"
      + "\titem=\(.meta.url // "?" | untrusted_excerpt(120))"
      + (if .ao then "\tauthored-by=\(.meta.author | untrusted_excerpt(40))" else "" end)
      + (if .x then "\tlast-external=\(.x.login | untrusted_excerpt(40)) \(.x.at | untrusted_clean) \(.x.kind) \(.x.url | untrusted_excerpt(120))" else "" end)
      + "\tmaintainer-in-window=\(.mt // "none" | untrusted_clean)"
      + "\t\(.meta.title // "" | untrusted_excerpt(80))"),
    "SUMMARY window-since=\($since) external-last=\(map(select(.label == "external-last")) | length) authored-open=\(map(select(.label == "authored-open")) | length) maintainer-later=\(map(select(.label == "maintainer-later")) | length)",
    "LIMITATION reviews: found through PRs updated since \($floor); a review on a PR whose updatedAt is older is not listed",
    (if ($d.truncated | length) > 0
     then "LIMITATION reviews: only the newest 100 reviews read on PR(s) \($d.truncated | map("#\(.)") | join(" ")); an all-clear needs those PRs read by hand"
     else empty end)
  ' "$tmp/ev.json" > "$tmp/out.txt"
cat "$tmp/out.txt"
