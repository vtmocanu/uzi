#!/usr/bin/env bash
# cr-rate-limit.sh — is CodeRabbit rate-limited on this PR, and when does the limit reset?
#
# CodeRabbit names the reset window in two places, and in neither on a bare re-trigger:
#   1. the walkthrough/status comment it edits in place: a "rate limited by coderabbit.ai"
#      block reading "Next included review available in N minutes" (free to read);
#   2. its reply to the exact two-word command `@coderabbitai rate limit`: "More reviews
#      will be available in N minutes" (costs one comment; `@coderabbitai ratelimits` and a
#      plain-English question get a useless "I cannot view the quota" non-answer).
#   A `@coderabbitai review` while limited only replies "Review rate limited" with no time,
#   and its commit status on the head reads "Review rate limited" with state=success.
# Every "N minutes" is relative to the comment's own timestamp, so this script converts it
# to an absolute reset instant and prints the REMAINING minutes as of now.
#
# Usage: cr-rate-limit.sh OWNER/REPO PR [--ask] [--wait] [--max-wait-min N] [--interval S]
#   --ask            when limited and no reset time is on the PR, post `@coderabbitai rate
#                    limit` ONCE and parse the reply (polls up to ~3 min for it).
#   --wait           when limited with a known reset, poll until the reset elapses (+2 min
#                    margin) or CR's status leaves "rate limited"; then exit 0. With an
#                    UNKNOWN reset, --wait waits --max-wait-min as a ceiling.
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

REPO=""; PR=""; ASK=0; WAIT=0; MAX_WAIT=180; INTERVAL=60
while [ $# -gt 0 ]; do
  case "$1" in
    --ask) ASK=1; shift;;
    --wait) WAIT=1; shift;;
    --max-wait-min) MAX_WAIT="${2:?}"; shift 2;;
    --interval) INTERVAL="${2:?}"; shift 2;;
    -h|--help) sed -n '2,32p' "$0"; exit 3;;
    -*) echo "unknown flag: $1" >&2; exit 3;;
    *) if [ -z "$REPO" ]; then REPO="$1"; elif [ -z "$PR" ]; then PR="$1"; else echo "unexpected arg: $1" >&2; exit 3; fi; shift;;
  esac
done
[ -n "$REPO" ] && [ -n "$PR" ] || { echo "usage: cr-rate-limit.sh OWNER/REPO PR [--ask] [--wait] [--max-wait-min N] [--interval S]" >&2; exit 3; }

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
# edits in place (base = its updated_at) and (2) the newest bot reply carrying "More reviews
# will be available in N minutes" (base = its created_at). The later base wins.
reset_from_pr() {
  local comments best_ts="" best_src="" b n ts
  comments=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null | jq -s 'add // []') || return 1
  # (1) walkthrough / status comment with the rate-limited block.
  b=$(printf '%s' "$comments" | jq -r '[.[]|select(.user.login=="coderabbitai[bot]" and (.body|contains("rate limited by coderabbit.ai")))]|last|"\(.updated_at)\t\(.body)"' 2>/dev/null)
  if [ -n "$b" ] && [ "$b" != "null" ]; then
    ts=$(printf '%s' "$b" | head -1 | cut -f1)
    n=$(printf '%s' "$b" | awk '/auto-generated comment: rate limited by coderabbit.ai/{f=1} f{print} /end of auto-generated comment: rate limited/{f=0}' \
        | grep -oE 'available in [0-9]+ minutes' | tail -1 | grep -oE '[0-9]+' || true)
    if [ -n "$n" ] && [ -n "$ts" ]; then best_ts=$(( $(iso2epoch "$ts") + n*60 )); best_src="walkthrough"; fi
  fi
  # (2) the newest `rate limit` reply.
  b=$(printf '%s' "$comments" | jq -r '[.[]|select(.user.login=="coderabbitai[bot]" and (.body|test("More reviews will be available in [0-9]+ minutes")))]|last|"\(.created_at)\t\(.body)"' 2>/dev/null)
  if [ -n "$b" ] && [ "$b" != "null" ]; then
    ts=$(printf '%s' "$b" | head -1 | cut -f1)
    n=$(printf '%s' "$b" | grep -oE 'More reviews will be available in [0-9]+ minutes' | tail -1 | grep -oE '[0-9]+' || true)
    if [ -n "$n" ] && [ -n "$ts" ]; then
      cand=$(( $(iso2epoch "$ts") + n*60 ))
      # Prefer the statement made LATER (its base is fresher), not the later reset instant.
      if [ -z "$best_ts" ] || [ "$(iso2epoch "$ts")" -ge "$(( best_ts - n*60 ))" ]; then best_ts=$cand; best_src="reply"; fi
    fi
  fi
  [ -n "$best_ts" ] && printf '%s\t%s\n' "$best_ts" "$best_src"
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
case "$status" in
  *"rate limited"*) ;;
  *) echo "CR_LIMITED=0"; exit 0 ;;
esac
echo "CR_LIMITED=1"

row=$(reset_from_pr) || { echo "gh error reading PR comments" >&2; exit 3; }
reset_ts=$(printf '%s' "$row" | cut -f1); src=$(printf '%s' "$row" | cut -f2)

if [ -z "$reset_ts" ] && [ "$ASK" -eq 1 ]; then
  asked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  gh pr comment "$PR" --repo "$REPO" --body '@coderabbitai rate limit' >/dev/null 2>&1 || { echo "could not post the rate-limit query" >&2; exit 3; }
  echo "ASKED_AT=$asked_at"
  for _ in $(seq 1 12); do
    sleep 15
    row=$(reset_from_pr) || continue
    src=$(printf '%s' "$row" | cut -f2)
    # Only a reply posted after our ask counts (a stale earlier reply would mislead).
    if [ "$src" = "reply" ]; then
      newest=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null | jq -s 'add // []' \
        | jq -r --arg a "$asked_at" '[.[]|select(.user.login=="coderabbitai[bot]" and (.body|test("More reviews will be available")) and .created_at > $a)]|length')
      [ "${newest:-0}" -ge 1 ] && { reset_ts=$(printf '%s' "$row" | cut -f1); break; }
    fi
  done
fi

report "$reset_ts" "${src:-}"

if [ "$WAIT" -eq 0 ]; then
  [ -z "$reset_ts" ] && exit 2
  [ "$reset_ts" -le "$(date +%s)" ] && { echo "CR_RESET_ELAPSED=1"; exit 0; }
  exit 1
fi

# --wait: poll until the reset (+2 min) elapses, CR's status changes, or the ceiling hits.
deadline=$(( $(date +%s) + MAX_WAIT*60 ))
[ -n "$reset_ts" ] && [ $(( reset_ts + 120 )) -lt "$deadline" ] && deadline=$(( reset_ts + 120 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  sleep "$INTERVAL"
  s=$(cr_status) || continue
  case "$s" in
    *"rate limited"*) echo "$(date +%H:%M:%S) still limited; reset at ${reset_ts:+$(jq -rn --argjson e "$reset_ts" '$e|todate')}${reset_ts:-unknown}" ;;
    *) echo "CR_STATUS='${s:-absent}'"; echo "CR_RESUMED=1"; exit 0 ;;
  esac
done
echo "CR_RESET_ELAPSED=1"
exit 0
