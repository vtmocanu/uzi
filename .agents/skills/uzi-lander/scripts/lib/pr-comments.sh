# shellcheck shell=bash
# pr-comments.sh — the author-agnostic merge blockers, shared by watch-pr.sh, pr-findings.sh,
# merge.sh and ack-comments.sh. Sourced, never executed. It sources lib/sanitize.sh and
# lib/state.sh itself.
#
# Three blockers, each counted from ANY author (humans, CodeQL's github-advanced-security,
# any bot), each an "item" {kind,key,id,author,at,body[,fp]} rendered only via untrusted_row:
#   thread   an unresolved, non-outdated review thread NOT already counted per bot: no
#            CodeRabbit comment in it, and not Greptile-only (Greptile's liveness is scoped to
#            its verdicts by lib/greptile-verdict.sh). A human reply on a Greptile thread
#            makes it count here. Input: fetch_review_threads output.
#   alert    an open code-scanning alert on refs/pull/N/head. 404 (no analysis) or a 403 that
#            says code scanning / Advanced Security is not enabled = CS_STATE=unavailable,
#            never a silent 0; any other failure = CS_STATE=unknown.
#   comment / review-body
#            an issue comment or non-empty review body, minus known pure-status bot output
#            (matched by exact author AND marker) and bare bot-trigger commands. It blocks
#            until ACKNOWLEDGED (ack-comments.sh). The ack is keyed on the item key (c<id> /
#            r<id>) and its fingerprint (updated_at or submitted_at, plus the body), so an
#            edit after the ack blocks again. Acks: <state dir>/acks/<owner>+<repo>+<pr>.json.
#
# Comment text is untrusted data: it is handled only inside jq and printed through
# untrusted_row; nothing here evaluates or interpolates it.

_PRC_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=sanitize.sh
. "$_PRC_LIB/sanitize.sh"
# shellcheck source=state.sh
. "$_PRC_LIB/state.sh"

# shellcheck disable=SC2016  # jq program text
PRC_JQ='
def other_threads:
  [ .[]
    | select(.isResolved == false and .isOutdated == false)
    | select((any(.comments.nodes[]?; ((.author.login // "") | startswith("coderabbitai")))) | not)
    | select(any(.comments.nodes[]?; ((.author.login // "") != "greptile-apps")))
    | (.comments.nodes[0] // {}) as $c
    | {kind: "thread", key: "t\($c.databaseId // "?")", id: "t\($c.databaseId // "?")",
       author: ($c.author.login // "ghost"),
       at: "\($c.path // "-"):\($c.line // $c.originalLine // "-")", body: ($c.body // "")} ];
def alert_items:
  map({kind: "alert", key: "a\(.number)", id: "a\(.number)",
       author: (.tool.name // "code-scanning"),
       at: "\(.most_recent_instance.location.path // "-"):\(.most_recent_instance.location.start_line // "-")",
       body: "\(.rule.id // "?") \(.rule.severity // .rule.security_severity_level // ""): \(.most_recent_instance.message.text // .rule.description // "")"});
def bare_command:
  ((. // "") | ascii_downcase | [splits("\\s+")] | map(select(. != ""))) as $w
  | ($w | length) >= 2
    and (($w[1:] | join(" ")) as $cmd
      | if $w[0] == "@coderabbitai" or $w[0] == "@coderabbitai[bot]" then
          any(["review", "full review", "rate limit", "reviews remaining?", "ignore", "pause", "resume"][]; . == $cmd)
        elif $w[0] == "@greptileai" or $w[0] == "@greptile" then $cmd == "review"
        else false end);
def status_only:
  (.user.login // "") as $u | (.body // "") as $b
  | ($u == "coderabbitai[bot]" and (
        ($b | contains("<!-- This is an auto-generated comment: summarize by coderabbit.ai -->"))
        or ($b | contains("<!-- walkthrough_start -->"))
        or ($b | contains("<!-- auto-generated comment: rate limited by coderabbit.ai -->"))
        or ($b | contains("<!-- CodeRabbit review command invocation:"))))
    or ($u == "greptile-apps[bot]" and (
        ($b | contains("<!-- greptile_comment -->"))
        or ($b | contains("<!-- greptile_outside_diff -->"))));
def must_ack($issue; $reviews):
  [ ($issue[] | select(((.body // "") | test("\\S")) and (status_only | not) and ((.body | bare_command) | not))
      | {kind: "comment", key: "c\(.id)", id: "c\(.id)", author: (.user.login // "ghost"), at: "-",
         body: .body, fp: "\(.updated_at // .created_at // "")\n\(.body)"}),
    ($reviews[] | select(((.body // "") | test("\\S")) and ((.body | bare_command) | not))
      | {kind: "review-body", key: "r\(.id)", id: "r\(.id)", author: (.user.login // "ghost"),
         at: "\(.state // "-")@\((.commit_id // "-")[0:8])",
         body: .body, fp: "\(.submitted_at // "")\n\(.body)"}) ];
def unacked($acks): map(select(($acks[.key] // null) != .fp));
'

# other_threads_json THREAD_NODES -> JSON array of thread items. rc 1 on unreadable input.
other_threads_json() { printf '%s' "$1" | jq -c "$PRC_JQ"' other_threads' 2>/dev/null; }

# code_scanning_open REPO PR -> sets CS_STATE (ok|unavailable|unknown), CS_ITEMS (alert
# items, [] unless ok) and CS_NOTE (why, for unavailable/unknown). Always rc 0.
# shellcheck disable=SC2034  # CS_* are this function's outputs, read by the sourcing scripts.
code_scanning_open() {
  local repo=$1 pr=$2 errf raw rc=0 txt
  CS_STATE=unknown; CS_ITEMS='[]'; CS_NOTE=""
  errf=$(mktemp "${TMPDIR:-/tmp}/uzi-lander-cs.XXXXXX") || { CS_NOTE="mktemp failed"; return 0; }
  raw=$(gh api --paginate "repos/${repo}/code-scanning/alerts?ref=refs/pull/${pr}/head&state=open&per_page=100" 2>"$errf") || rc=$?
  txt="$(cat "$errf" 2>/dev/null) ${raw}"
  rm -f "$errf"
  if [ "$rc" -eq 0 ]; then
    if CS_ITEMS=$(printf '%s' "$raw" | jq -sc "$PRC_JQ"' if length > 0 and all(.[]; type == "array") then add | alert_items else error("not an array") end' 2>/dev/null); then
      CS_STATE=ok
    else
      CS_ITEMS='[]'; CS_NOTE="unreadable alert listing"
    fi
    return 0
  fi
  case "$txt" in
    *"HTTP 404"*) CS_STATE=unavailable; CS_NOTE="no code-scanning analysis (HTTP 404)" ;;
    *"HTTP 403"*)
      case "$txt" in
        *"Advanced Security must be enabled"*|*"ode scanning is not enabled"*)
          CS_STATE=unavailable; CS_NOTE="code scanning not enabled (HTTP 403)" ;;
        *) CS_NOTE="HTTP 403 without a not-enabled message (permissions or rate limit)" ;;
      esac ;;
    *) CS_NOTE="alert lookup failed (gh exit $rc)" ;;
  esac
  return 0
}

# must_ack_json ISSUE_FLAT REVIEWS_FLAT -> JSON array of comment/review-body items. Both
# inputs are ONE flat JSON array each. rc 1 on unreadable input.
must_ack_json() {
  jq -nc --argjson i "$1" --argjson r "$2" "$PRC_JQ"' if ($i|type) == "array" and ($r|type) == "array" then must_ack($i; $r) else error("x") end' 2>/dev/null
}

# ack_path REPO PR -> the ack file path (creates the acks dir). rc 1 when no state dir.
ack_path() {
  local sd
  sd=$(state_dir) || return 1
  mkdir -p "$sd/acks" 2>/dev/null || return 1
  printf '%s/acks/%s+%s.json' "$sd" "$(printf '%s' "$1" | tr '/' '+')" "$2"
}

# ack_read REPO PR -> the recorded acks, a JSON object ({} when none yet). rc 1 when the
# file exists but is unreadable; callers treat that as an unknown lookup, never as acked.
ack_read() {
  local f
  f=$(ack_path "$1" "$2") || return 1
  [ -e "$f" ] || { echo '{}'; return 0; }
  jq -ce 'if type == "object" then . else error("x") end' "$f" 2>/dev/null
}

# unacked_json ITEMS ACKS -> the items whose key+fingerprint has no matching ack.
unacked_json() { jq -nc --argjson it "$1" --argjson a "$2" "$PRC_JQ"' $it | unacked($a)' 2>/dev/null; }

# print_items ITEMS -> one sanitized untrusted_row per item.
print_items() { printf '%s' "$1" | jq -r "$UNTRUSTED_JQ"' .[] | untrusted_row' 2>/dev/null; }
