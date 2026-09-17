#!/usr/bin/env bash
# trail.sh — the one-line status trail the lander shows the user on every state change.
#
# The take-over stance is "poll, do not watch": the user gets one breadcrumb line per
# transition and nothing in between. Keeping the trail in a file (not in the model's
# head) makes the line the same shape from every session and survives a context reset.
#
# Usage:
#   trail.sh <key> <state>        append <state> (deduped if it equals the last one), then
#                                 print the whole trail: "<key>: a → b → c"
#   trail.sh <key>                print the trail without appending
#   trail.sh <key> --reset        start over (empty trail)
#   <key> is "#<PR>" or "run:<id8>"; anything without a slash.
#
# Vocabulary (keep to it, so trails read the same across PRs):
#   run:<status>            run queued|running|awaiting_input|limit_wait|completed|failed
#   pr opened               the MR exists
#   ci pending|green|red    required checks on the head
#   cr pending|clean|findings(n)|rate-limited(Nm)|skipped
#   greptile pending|clean|findings(n)
#   waiting <what>          e.g. "waiting cr reset 57m", "waiting mr_rework"
#   fix local|rework|skip   the decision on findings
#   rebase+renumber|rebase  land-prep did base hygiene
#   pushed                  a new head went up (a re-review follows)
#   admin-merged <sha8>     merged
#   main ci green|red|superseded
#
# Storage: $UZI_LANDER_TRAIL_DIR (default /tmp/uzi-lander-trail)/<key>.trail, one state
# per line. Exit 0; 2 on usage.
set -u

KEY=${1:-}; [ -n "$KEY" ] || { echo "usage: trail.sh <key> [<state> | --reset]" >&2; exit 2; }
case "$KEY" in */*) echo "key must not contain '/'" >&2; exit 2;; esac
shift
DIR="${UZI_LANDER_TRAIL_DIR:-/tmp/uzi-lander-trail}"
mkdir -p "$DIR"
F="$DIR/$KEY.trail"

if [ "${1:-}" = "--reset" ]; then : > "$F"; shift; fi
if [ $# -gt 0 ]; then
  state="$*"
  last=""
  [ -s "$F" ] && last=$(tail -n 1 "$F")
  [ "$state" != "$last" ] && printf '%s\n' "$state" >> "$F"
fi

if [ -s "$F" ]; then
  printf '%s: ' "$KEY"
  awk 'NR>1{printf " → "} {printf "%s",$0} END{print ""}' "$F"
else
  echo "$KEY: (no trail yet)"
fi
exit 0
