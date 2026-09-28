# shellcheck shell=bash
# pr-comments.sh — the author-agnostic merge blockers, shared by watch-pr.sh, pr-findings.sh,
# merge.sh and ack-comments.sh. Sourced, never executed. It sources lib/sanitize.sh and
# lib/state.sh itself.
#
# Three blockers, each counted from ANY author (humans, CodeQL's github-advanced-security,
# CodeRabbit, Greptile, any bot), each an "item" {kind,key,id,author,at,body[,fp,digest]}
# rendered only via untrusted_row. None is waived by a --reviewer selection.
#   thread   EVERY unresolved, non-outdated review thread. Resolve it (or let a push outdate
#            it) to clear it. Input: fetch_review_threads output.
#   alert    an open code-scanning alert on refs/pull/N/head. When the INITIAL request fails
#            with 404 (no analysis) or a 403 saying code scanning / Advanced Security is not
#            enabled, with stdout empty or only gh's error body: CS_STATE=unavailable (printed,
#            counted as none). Any other failure, any partial or malformed output,
#            including one after a page was already read: CS_STATE=unknown.
#   comment / review-body
#            an issue comment or non-empty review body, minus known pure-status bot output
#            (matched by EXACT author login AND marker) and bare bot-trigger commands. It
#            blocks until acknowledged (ack-comments.sh). Its digest is a short sha256 of its
#            updated_at (submitted_at for a review) and body; the ack must name the digest the
#            lander read (ID@DIGEST), so an edit before or after the ack blocks again.
#            Acks: <state dir>/acks/<owner>+<repo>+<pr>.json, {key: digest}.
#
# Comment text is untrusted data: it is handled only inside jq and printed through
# untrusted_row; nothing here evaluates or interpolates it. The digest hashes the
# base64 form jq emits, so no comment byte reaches a shell word.

_PRC_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=sanitize.sh
. "$_PRC_LIB/sanitize.sh"
# shellcheck source=state.sh
. "$_PRC_LIB/state.sh"

# shellcheck disable=SC2016  # jq program text
PRC_JQ='
def open_threads:
  [ .[]
    | select(.isResolved == false and .isOutdated == false)
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
        or ($b | contains("<!-- CodeRabbit review command invocation:"))
        # The answer to the cr-rate-limit.sh quota query: pure status, short, no finding.
        or (($b | contains("<!-- This is an auto-generated reply by CodeRabbit -->"))
            and ($b | test("More reviews will be available in [0-9]+ minutes?|Reviews are available now"))
            and ($b | length) < 600)))
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
def unacked($acks): map(select(.digest == null or ($acks[.key] // null) != .digest));
'

# _prc_sha STDIN -> the first 16 hex chars of its sha256.
_prc_sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi | cut -c1-16
}

# open_threads_json THREAD_NODES -> JSON array of thread items. rc 1 on unreadable input.
open_threads_json() { printf '%s' "$1" | jq -c "$PRC_JQ"' open_threads' 2>/dev/null; }

# code_scanning_open REPO PR -> sets CS_STATE (ok|unavailable|unknown), CS_ITEMS (alert
# items, [] unless ok) and CS_NOTE (why, for unavailable/unknown). Always rc 0.
# shellcheck disable=SC2034  # CS_* are this function's outputs, read by the sourcing scripts.
code_scanning_open() {
  local repo=$1 pr=$2 errf raw rc=0 txt stdout_kind
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
  # `unavailable` needs POSITIVE proof that the initial request itself failed: stdout must be
  # empty, or exactly gh's single JSON error body ({message, documentation_url, status}), and
  # hold no alert data. Anything else (an alert page, malformed or partial output, a second
  # document) is unknown; a jq failure is never read as "no page was read".
  stdout_kind=$(printf '%s' "$raw" | jq -rs '
    if length == 0 then "empty"
    elif length == 1 and (.[0] | type) == "object" and (.[0] | has("message"))
         and ((.[0] | keys) - ["message", "documentation_url", "status"] | length) == 0
      then "error-body"
    else "data" end' 2>/dev/null) || stdout_kind=unparseable
  case "$stdout_kind" in
    empty|error-body) ;;
    *) CS_NOTE="alert listing failed with output that is not a bare error ($stdout_kind; gh exit $rc)"; return 0 ;;
  esac
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

# must_ack_json ISSUE_FLAT REVIEWS_FLAT -> JSON array of comment/review-body items, each
# with its digest. Both inputs are ONE flat JSON array each. rc 1 on unreadable input.
must_ack_json() {
  local items b64 digests='' d
  items=$(jq -nc --argjson i "$1" --argjson r "$2" "$PRC_JQ"' if ($i|type) == "array" and ($r|type) == "array" then must_ack($i; $r) else error("x") end' 2>/dev/null) || return 1
  # One base64 line per item, in order; the digest hashes that line.
  while IFS= read -r b64; do
    d=$(printf '%s' "$b64" | _prc_sha) || return 1
    digests="${digests}${d}"$'\n'
  done < <(printf '%s' "$items" | jq -r '.[] | .fp | @base64')
  jq -nc --argjson it "$items" --arg d "$digests" \
    '($d | split("\n") | map(select(. != ""))) as $ds
     | if ($ds | length) != ($it | length) then error("digest count") else [range($it | length) as $n | $it[$n] + {digest: $ds[$n]}] end' 2>/dev/null
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

# unacked_json ITEMS ACKS -> the items whose key has no ack at their current digest.
unacked_json() { jq -nc --argjson it "$1" --argjson a "$2" "$PRC_JQ"' $it | unacked($a)' 2>/dev/null; }

# print_items ITEMS -> one sanitized untrusted_row per item.
print_items() { printf '%s' "$1" | jq -r "$UNTRUSTED_JQ"' .[] | untrusted_row' 2>/dev/null; }
