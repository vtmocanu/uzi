#!/usr/bin/env bash
# cr-rate-limit.sh — is CodeRabbit rate-limited on this PR, and when does the limit reset?
#
# CodeRabbit names the reset window in two places, and in neither on a bare re-trigger:
#   1. the walkthrough/status comment it edits in place: a "rate limited by coderabbit.ai"
#      block reading "Next included review available in N minute(s)" (free to read);
#   2. its reply to the exact two-word command `@coderabbitai rate limit`: either "More
#      reviews will be available in N minute(s)" or "Reviews are available now" (costs one
#      comment; `@coderabbitai ratelimits` and plain-English questions get a non-answer).
#   A `@coderabbitai review` while limited only replies "Review rate limited" with no time,
#   and its commit status on the head reads "Review rate limited" with state=success.
# Every countdown is relative to the comment's own timestamp, so this script converts it
# to an absolute reset instant and prints the REMAINING minutes as of now.
#
# Usage: cr-rate-limit.sh OWNER/REPO PR [--ask] [--query] [--wait] [--max-wait-min N] [--interval S]
#   --ask            when limited and no reset time is on the PR, post `@coderabbitai rate
#                    limit` ONCE and parse the reply (polls up to ~3 min for it).
#   --query          post that exact query even when the head status is stale/non-limited;
#                    implies --ask. Use when exact quota timing matters.
#   --wait           when limited with a known reset, poll until the reset elapses (+2 min
#                    margin) or CR's status leaves "rate limited"; then exit 0. With an
#                    UNKNOWN reset, --wait waits --max-wait-min as a ceiling. Hitting the
#                    ceiling with the reset still ahead exits 1 (unknown: 2), never 0.
#   --max-wait-min   ceiling for --wait (default 180).
#   --interval       poll seconds for --wait (default 60).
#
# Prints KEY=VALUE lines: CR_LIMITED=0|1, CR_STATUS='<description>', and when limited
# CR_RESET_MIN=<remaining minutes|unknown>, CR_RESET_AT=<UTC>, CR_RESET_SOURCE=<walkthrough|reply>.
#
# Exit codes:
#   0  not limited, OR the limit window has elapsed (safe to post `@coderabbitai review`
#      ONCE — do not spam it: every trigger while limited is a wasted comment)
#   1  limited, reset known and still in the future (wait; --wait does it for you)
#   2  limited, reset unknown (re-run with --ask, or --wait with a ceiling)
#   3  usage / gh error
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/state.sh
. "$HERE/lib/state.sh"

REPO=""; PR=""; ASK=0; QUERY=0; WAIT=0; MAX_WAIT=180; INTERVAL=60
while [ $# -gt 0 ]; do
  case "$1" in
    --ask) ASK=1; shift;;
    --query) QUERY=1; ASK=1; shift;;
    --wait) WAIT=1; shift;;
    --max-wait-min) MAX_WAIT="${2:?}"; shift 2;;
    --interval) INTERVAL="${2:?}"; shift 2;;
    -h|--help) sed -n '2,32p' "$0"; exit 3;;
    -*) echo "unknown flag: $1" >&2; exit 3;;
    *) if [ -z "$REPO" ]; then REPO="$1"; elif [ -z "$PR" ]; then PR="$1"; else echo "unexpected arg: $1" >&2; exit 3; fi; shift;;
  esac
done
[ -n "$REPO" ] && [ -n "$PR" ] || { echo "usage: cr-rate-limit.sh OWNER/REPO PR [--ask] [--query] [--wait] [--max-wait-min N] [--interval S]" >&2; exit 3; }

# ISO-8601 (GitHub's "2026-09-17T05:57:46Z") -> epoch seconds, via jq so it is portable
# across BSD and GNU date. Fractional seconds are stripped first.
iso2epoch() { jq -rn --arg t "$1" '$t|sub("\\.[0-9]+";"")|fromdateiso8601' 2>/dev/null; }

# CodeRabbit's commit-status description on the PR head ("Review rate limited", "Review in
# progress", "Review skipped: …", or empty when CR never touched the head).
cr_status() {
  local head
  head=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid 2>/dev/null) || return 1
  gh api "repos/$REPO/commits/$head/status" \
    --jq '[.statuses[]|select(.context=="CodeRabbit")]|last|.description // empty' 2>/dev/null
}

# Find the newest reset statement on the PR. Prints "<epoch-of-reset>\t<source>" or nothing.
# Reads the issue comments once; considers (1) the rate-limited block in the comment CR
# edits in place (base = its updated_at) and (2) the newest bot reply carrying a countdown
# or "Reviews are available now" (base = its created_at). The later base wins.
reset_from_pr() {
  local comments best_ts="" best_base="" best_src="" b n ts base
  comments=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null | jq -s 'add // []') || return 1
  printf '%s' "$comments" | jq -e 'type=="array"' >/dev/null 2>&1 || return 1
  # (1) walkthrough / status comment with the rate-limited block.
  b=$(printf '%s' "$comments" | jq -r '[.[]|select(.user.login=="coderabbitai[bot]" and (.body|contains("rate limited by coderabbit.ai")))]|last|select(.!=null)|"\(.updated_at)\t\(.body)"' 2>/dev/null)
  if [ -n "$b" ]; then
    ts=$(printf '%s' "$b" | head -1 | cut -f1)
    n=$(printf '%s' "$b" | awk '/auto-generated comment: rate limited by coderabbit.ai/{f=1} f{print} /end of auto-generated comment: rate limited/{f=0}' \
        | grep -oE 'available in [0-9]+ minutes?' | tail -1 | grep -oE '[0-9]+' || true)
    if [ -n "$n" ] && [ -n "$ts" ] && base=$(iso2epoch "$ts") && [ -n "$base" ]; then
      best_base=$base; best_ts=$(( base + n*60 )); best_src="walkthrough"
    fi
  fi
  # (2) the newest `rate limit` reply. The statement with the LATER base timestamp wins
  # (its countdown or immediate availability is the fresher figure), not the later reset.
  b=$(printf '%s' "$comments" | jq -r '[.[]|select(.user.login=="coderabbitai[bot]" and
    ((.body // "")|test("More reviews will be available in [0-9]+ minutes?|Reviews are available now")))]
    |last|select(.!=null)|"\(.created_at)\t\(.body)"' 2>/dev/null)
  if [ -n "$b" ]; then
    ts=$(printf '%s' "$b" | head -1 | cut -f1)
    # No-pipeline match test: `printf … | grep -qF` can return 141 (SIGPIPE) under
    # `set -o pipefail` — grep -q closes the pipe on a match before printf finishes writing,
    # so a real "Reviews are available now" reply reads as no-match (observed on #1504,
    # flaky by body size). `case` against the captured body has no pipe and no such race.
    case "$b" in
      *"Reviews are available now"*) n=0 ;;
      *) n=$(printf '%s' "$b" | grep -oE 'More reviews will be available in [0-9]+ minutes?' | tail -1 | grep -oE '[0-9]+' || true) ;;
    esac
    if [ -n "$n" ] && [ -n "$ts" ] && base=$(iso2epoch "$ts") && [ -n "$base" ]; then
      if [ -z "$best_base" ] || [ "$base" -ge "$best_base" ]; then
        best_base=$base; best_ts=$(( base + n*60 )); best_src="reply"
      fi
    fi
  fi
  [ -n "$best_ts" ] && printf '%s\t%s\n' "$best_ts" "$best_src"
  return 0
}

# Exact query mode ignores a later-edited walkthrough: only a qualifying bot reply after
# this invocation's command can answer it. Prints "<epoch-of-reset>\treply" or nothing.
reset_from_reply_after() {
  local asked_at=$1 comments b n ts base
  comments=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null | jq -s 'add // []') || return 1
  printf '%s' "$comments" | jq -e 'type=="array"' >/dev/null 2>&1 || return 1
  b=$(printf '%s' "$comments" | jq -r --arg a "$asked_at" \
    '[.[]|select(.user.login=="coderabbitai[bot]" and .created_at>$a
                 and ((.body // "")|test("More reviews will be available in [0-9]+ minutes?|Reviews are available now")))]
     |last|select(.!=null)|"\(.created_at)\t\(.body)"' 2>/dev/null)
  [ -n "$b" ] || return 0
  ts=$(printf '%s' "$b" | head -1 | cut -f1)
  # No-pipeline match test: see reset_from_pr — `printf … | grep -qF` can return 141 (SIGPIPE)
  # under pipefail and misread a real "available now" reply as no-match.
  case "$b" in
    *"Reviews are available now"*) n=0 ;;
    *) n=$(printf '%s' "$b" | grep -oE 'More reviews will be available in [0-9]+ minutes?' | tail -1 | grep -oE '[0-9]+' || true) ;;
  esac
  if [ -n "$n" ] && [ -n "$ts" ] && base=$(iso2epoch "$ts") && [ -n "$base" ]; then
    printf '%s\treply\n' "$(( base + n*60 ))"
  fi
  return 0
}

report() {  # $1 = reset epoch or "", $2 = source
  local now rem
  now=$(date +%s)
  if [ -n "$1" ]; then
    rem=$(( ( $1 - now + 59 ) / 60 )); [ "$rem" -lt 0 ] && rem=0
    echo "CR_RESET_MIN=$rem"
    echo "CR_RESET_AT=$(jq -rn --argjson e "$1" '$e|todate')"
    echo "CR_RESET_SOURCE=$2"
  else
    echo "CR_RESET_MIN=unknown"
  fi
}

status=$(cr_status) || { echo "gh error resolving PR $PR on $REPO" >&2; exit 3; }
echo "CR_STATUS='${status:-absent}'"
status_limited=0
case "$status" in *"rate limited"*) status_limited=1;; esac
if [ "$status_limited" -eq 0 ] && [ "$QUERY" -eq 0 ]; then echo "CR_LIMITED=0"; exit 0; fi

row=$(reset_from_pr) || { echo "gh error reading PR comments" >&2; exit 3; }
reset_ts=$(printf '%s' "$row" | cut -f1); src=$(printf '%s' "$row" | cut -f2)
# --query asks for an exact live answer. Never let an inferred walkthrough timestamp satisfy it.
if [ "$QUERY" -eq 1 ]; then reset_ts=""; src=""; fi

if { [ "$QUERY" -eq 1 ] || [ -z "$reset_ts" ]; } && [ "$ASK" -eq 1 ]; then
  # In-flight guard, FAIL CLOSED and serialised: the post happens only under a per-PR lock
  # (mkdir in the shared state dir, 10-min TTL) so two invocations cannot both post, and
  # only after a SUCCESSFUL read of the comments proves no unanswered `@coderabbitai rate
  # limit` from the last 10 minutes exists (an unreadable listing means do not post).
  SD=$(state_dir) || exit 3
  ask_lock="$SD/locks/cr-ask-${REPO//\//_}-$PR"
  if ! mkdir "$ask_lock" 2>/dev/null; then
    lage=$(( $(date +%s) - $(stat -f %m "$ask_lock" 2>/dev/null || stat -c %Y "$ask_lock" 2>/dev/null || date +%s) ))
    if [ "$lage" -gt 600 ]; then rm -rf "$ask_lock"; mkdir "$ask_lock" 2>/dev/null || { echo "ask lock busy" >&2; exit 3; }
    else echo "ASK_LOCK_HELD=1 (another invocation is asking; re-run without --ask in a minute)"; exit 2; fi
  fi
  trap 'rm -rf "$ask_lock"' EXIT
  comments_now=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null | jq -s 'add // []' 2>/dev/null || true)
  printf '%s' "$comments_now" | jq -e 'type=="array"' >/dev/null 2>&1 || { echo "cannot read the PR comments; not posting a rate-limit query" >&2; exit 3; }
  pending_ask=$(printf '%s' "$comments_now" | jq -r '
    ([.[]|select(((.user.login|test("\\[bot\\]$"))|not) and ((.body|gsub("^\\s+|\\s+$";""))=="@coderabbitai rate limit"))]|last) as $a
    | if $a==null then "" else
        ([.[]|select(.user.login=="coderabbitai[bot]" and
          (.body|test("More reviews will be available|Reviews are available now")) and
          .created_at > $a.created_at)]|length) as $replied
        | if $replied>0 then "" else $a.created_at end end' 2>/dev/null) || { echo "cannot parse the PR comments; not posting" >&2; exit 3; }
  if [ -n "$pending_ask" ] && [ $(( $(date +%s) - $(iso2epoch "$pending_ask") )) -lt 600 ]; then
    asked_at="$pending_ask"; echo "ASK_IN_FLIGHT_SINCE=$asked_at (not posting again)"
  else
    asked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    gh pr comment "$PR" --repo "$REPO" --body '@coderabbitai rate limit' >/dev/null 2>&1 || { echo "could not post the rate-limit query" >&2; exit 3; }
    echo "ASKED_AT=$asked_at"
  fi
  for _ in $(seq 1 12); do
    sleep 15
    row=$(reset_from_reply_after "$asked_at") || continue
    if [ -n "$row" ]; then
      reset_ts=$(printf '%s' "$row" | cut -f1)
      src=$(printf '%s' "$row" | cut -f2)
      break
    fi
  done
fi

if [ -n "$reset_ts" ]; then
  status_limited=0
  [ "$reset_ts" -gt "$(date +%s)" ] && status_limited=1
fi
echo "CR_LIMITED=$status_limited"
report "$reset_ts" "${src:-}"

if [ "$WAIT" -eq 0 ]; then
  [ -z "$reset_ts" ] && exit 2
  [ "$reset_ts" -le "$(date +%s)" ] && { echo "CR_RESET_ELAPSED=1"; exit 0; }
  exit 1
fi

# --wait: poll until the reset (+2 min) elapses, CR's status changes, or the ceiling hits.
deadline=$(( $(date +%s) + MAX_WAIT*60 ))
[ -n "$reset_ts" ] && [ $(( reset_ts + 120 )) -lt "$deadline" ] && deadline=$(( reset_ts + 120 ))
reset_label=unknown
[ -n "$reset_ts" ] && reset_label=$(jq -rn --argjson e "$reset_ts" '$e|todate')
while [ "$(date +%s)" -lt "$deadline" ]; do
  sleep "$INTERVAL"
  if [ "$QUERY" -eq 1 ]; then
    now=$(date +%s)
    if [ -n "$reset_ts" ] && [ "$now" -ge "$reset_ts" ]; then echo "CR_RESET_ELAPSED=1"; exit 0; fi
    echo "$(date +%H:%M:%S) waiting on exact quota reset at $reset_label"
    continue
  fi
  s=$(cr_status) || continue
  case "$s" in
    *"rate limited"*) echo "$(date +%H:%M:%S) still limited; reset at $reset_label" ;;
    *) echo "CR_STATUS='${s:-absent}'"; echo "CR_RESUMED=1"; exit 0 ;;
  esac
done
# The wait ended: only a reset that has actually passed is "elapsed". Hitting the ceiling
# with the reset still ahead (or unknown) is NOT permission to trigger a review.
now=$(date +%s)
if [ -n "$reset_ts" ] && [ "$now" -ge "$reset_ts" ]; then echo "CR_RESET_ELAPSED=1"; exit 0; fi
if [ -n "$reset_ts" ]; then echo "CR_WAIT_CEILING=1 (reset still $(( (reset_ts - now + 59) / 60 )) min ahead; re-run --wait)"; exit 1; fi
echo "CR_WAIT_CEILING=1 (reset unknown; re-run with --ask)"; exit 2
