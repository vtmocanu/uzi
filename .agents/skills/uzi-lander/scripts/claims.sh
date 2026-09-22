#!/usr/bin/env bash
# claims.sh — which session is landing which PR, so several landers on one repo (Claude or
# Codex, via the session-peers registry) can see each other, avoid double-driving a PR,
# sequence their CodeRabbit consumption, and clean up after themselves.
#
# One JSON per key under <state dir>/claims/ (see lib/state.sh for where that is: the
# repo's shared .git/uzi-lander by default, never tracked). A claim records the owner's
# name and durable session uuid, kind (claude|codex), claimed_at, last_seen (bumped by
# every trail.sh append), the PR's size (files, lines), a priority, depends_on, a note,
# and the last trail state.
#
# Usage:
#   claims.sh claim <key> [--repo O/R] [--pr N] [--size FILES] [--lines N] [--priority N]
#                         [--depends-on '#a,#b'] [--note TEXT] [--as NAME --uuid UUID --kind K] [--force]
#   claims.sh release <key> [--purge]      # --purge also drops the trail (after a merge)
#   claims.sh touch <key> [--state TEXT]   # heartbeat + last state (trail.sh calls this)
#   claims.sh show <key>
#   claims.sh list [--json] [--all]        # this repo's claims, priority-desc; --all = every state dir key
#   claims.sh reap [--repo O/R] [--dry-run] # drop terminal/dead claims and stale orphan trails
#   claims.sh whoami
#
# Priority: sessions decide. The default is the PR's file count (bigger first), because
# CodeRabbit reviews are the scarce resource and are worth spending on the large PRs; a
# small PR is cheap to review with a local agent. `--priority N` overrides; the user has the
# last word. `--depends-on` names keys that must land first; `list` shows them so nobody
# merges a dependent ahead of its base.
#
# Exit codes:
#   0  ok
#   2  usage
#   3  error (state dir, jq, gh)
#   4  claim: held by ANOTHER LIVE session (its name/uuid printed: message it, do not steal;
#      --force takes it anyway, say why in the trail)
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/state.sh
. "$HERE/lib/state.sh"

usage() { sed -n '2,32p' "$0" >&2; exit 2; }
[ $# -ge 1 ] || usage
verb=$1; shift

key_check() { case "$1" in ""|*/*) echo "bad key '$1' (non-empty, no '/')" >&2; exit 2;; esac; }
SD=$(state_dir) || exit 3
CL="$SD/claims"

# owner_live <uuid> -> prints live|dead|unknown ; a dead-or-stale claim may be taken over.
owner_live() {
  local u="$1" ls rc
  is_live "$u"; rc=$?
  if [ "$rc" -eq 0 ]; then echo live; return; fi
  if [ "$rc" -eq 1 ]; then echo dead; return; fi
  ls=$(jq -r '.last_seen // ""' "$2" 2>/dev/null)
  if [ -n "$ls" ] && [ "$(( $(date +%s) - $(iso2epoch "$ls") ))" -gt "$(( STALE_HOURS * 3600 ))" ]; then echo dead; else echo unknown; fi
}

case "$verb" in
  whoami)
    self_identity | awk -F'\t' '{printf "SESSION_NAME=%s\nSESSION_UUID=%s\nSESSION_KIND=%s\n",$1,$2,$3}'; exit 0;;

  claim)
    key=${1:-}; key_check "$key"; shift
    repo=""; pr=""; size=""; lines=""; prio=""; deps=""; note=""; as=""; uuid=""; kind=""; force=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --repo) repo="${2:?}"; shift 2;; --pr) pr="${2:?}"; shift 2;;
        --size) size="${2:?}"; shift 2;; --lines) lines="${2:?}"; shift 2;;
        --priority) prio="${2:?}"; shift 2;; --depends-on) deps="${2:?}"; shift 2;;
        --note) note="${2:?}"; shift 2;; --as) as="${2:?}"; shift 2;; --uuid) uuid="${2:?}"; shift 2;;
        --kind) kind="${2:?}"; shift 2;; --force) force=1; shift;;
        *) echo "unknown arg: $1" >&2; usage;;
      esac
    done
    me=$(self_identity "$as" "$uuid" "$kind"); my_name=$(printf '%s' "$me" | cut -f1); my_uuid=$(printf '%s' "$me" | cut -f2); my_kind=$(printf '%s' "$me" | cut -f3)
    f="$CL/$key.json"
    # Acquisition is serialised per key with a mkdir lock (atomic on POSIX): two fresh claims
    # cannot both succeed, the check-then-write below runs under it. A lock older than 60 s
    # belongs to a crashed claimer and is broken.
    lock="$CL/.lock.$key"; got=0
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      if mkdir "$lock" 2>/dev/null; then got=1; break; fi
      lage=$(( $(date +%s) - $(stat -f %m "$lock" 2>/dev/null || stat -c %Y "$lock" 2>/dev/null || date +%s) ))
      [ "$lage" -gt 60 ] && rm -rf "$lock"
      sleep 0.3
    done
    [ "$got" -eq 1 ] || { echo "claim lock busy for $key; retry" >&2; exit 3; }
    trap 'rm -rf "$lock"' EXIT
    if [ -f "$f" ]; then
      o_uuid=$(jq -r '.owner_uuid // ""' "$f"); o_name=$(jq -r '.owner // ""' "$f")
      if [ "$o_uuid" != "$my_uuid" ] && [ "$force" -eq 0 ]; then
        st=$(owner_live "$o_uuid" "$f")
        if [ "$st" != "dead" ]; then
          echo "CLAIM_HELD_BY=$o_name"; echo "CLAIM_HELD_UUID=$o_uuid"; echo "CLAIM_OWNER_LIVENESS=$st"
          echo "held by another session ($st): message @$o_name before touching $key, or --force with a reason" >&2
          exit 4
        fi
        echo "note: previous owner $o_name ($o_uuid) is dead/stale; taking over $key" >&2
      fi
    fi
    [ -n "$prio" ] || prio="${size:-0}"
    deps_json=$(jq -Rn --arg d "$deps" '$d|split(",")|map(gsub("^\\s+|\\s+$";""))|map(select(length>0))')
    tmp=$(mktemp "$CL/.tmp.XXXXXX")
    jq -n --arg key "$key" --arg repo "$repo" --arg pr "$pr" --arg owner "$my_name" --arg uuid "$my_uuid" --arg kind "$my_kind" \
          --arg now "$(now_iso)" --argjson size "${size:-0}" --argjson lines "${lines:-0}" --argjson prio "$prio" \
          --argjson deps "$deps_json" --arg note "$note" \
          --arg prev_claimed "$( [ -f "$f" ] && jq -r '.claimed_at // ""' "$f" || true)" \
          --arg prev_state "$( [ -f "$f" ] && jq -r '.state // ""' "$f" || true)" '
      {key:$key, repo:$repo, pr:(if $pr=="" then null else ($pr|tonumber) end), owner:$owner, owner_uuid:$uuid, kind:$kind,
       claimed_at:(if $prev_claimed=="" then $now else $prev_claimed end), last_seen:$now,
       size_files:$size, size_lines:$lines, priority:$prio, depends_on:$deps, note:$note, state:$prev_state}' > "$tmp" || { rm -f "$tmp"; exit 3; }
    mv -f "$tmp" "$f"
    echo "CLAIMED=$key OWNER=$my_name PRIORITY=$prio"; exit 0;;

  release)
    key=${1:-}; key_check "$key"; shift
    purge=0; [ "${1:-}" = "--purge" ] && purge=1
    rm -f "$CL/$key.json"
    suffix=""
    if [ "$purge" -eq 1 ]; then rm -f "$SD/trail/$key.trail"; suffix=" (trail purged)"; fi
    echo "RELEASED=$key$suffix"; exit 0;;

  touch)
    key=${1:-}; key_check "$key"; shift
    state=""; [ "${1:-}" = "--state" ] && state="${2:-}"
    f="$CL/$key.json"; [ -f "$f" ] || exit 0
    tmp=$(mktemp "$CL/.tmp.XXXXXX")
    jq --arg now "$(now_iso)" --arg s "$state" '.last_seen=$now | if $s!="" then .state=$s else . end' "$f" > "$tmp" && mv -f "$tmp" "$f"
    exit 0;;

  show)
    key=${1:-}; key_check "$key"
    [ -f "$CL/$key.json" ] && cat "$CL/$key.json" || { echo "no claim for $key"; exit 1; };;

  list)
    json=0; [ "${1:-}" = "--json" ] && json=1
    rows=$(for f in "$CL"/*.json; do [ -f "$f" ] || continue
      u=$(jq -r '.owner_uuid' "$f"); l=$(owner_live "$u" "$f")
      jq -c --arg live "$l" '. + {live:$live}' "$f"; done | jq -s 'sort_by(-.priority)')
    if [ "$json" -eq 1 ]; then printf '%s\n' "$rows"; exit 0; fi
    printf '%-9s %-22s %-8s %-4s %-6s %-9s %-14s %s\n' KEY OWNER KIND PRIO FILES LIVE DEPENDS_ON STATE
    printf '%s' "$rows" | jq -r '.[]|[.key, .owner, .kind, (.priority|tostring), (.size_files|tostring), .live, (.depends_on|join(",")|if .=="" then "-" else . end), (.state|if .=="" then "-" else . end)]|@tsv' \
      | awk -F'\t' '{printf "%-9s %-22s %-8s %-4s %-6s %-9s %-14s %s\n",$1,$2,$3,$4,$5,$6,$7,$8}'
    exit 0;;

  reap)
    repo=""; dry=0
    while [ $# -gt 0 ]; do case "$1" in --repo) repo="${2:?}"; shift 2;; --dry-run) dry=1; shift;; *) usage;; esac; done
    n=0
    for f in "$CL"/*.json; do
      [ -f "$f" ] || continue
      key=$(jq -r '.key' "$f"); pr=$(jq -r '.pr // ""' "$f"); r=$(jq -r '.repo // ""' "$f"); u=$(jq -r '.owner_uuid' "$f"); o=$(jq -r '.owner' "$f")
      [ -n "$repo" ] && [ -n "$r" ] && [ "$r" != "$repo" ] && continue
      why=""
      if [ -n "$pr" ] && [ -n "$r" ]; then
        st=$(gh pr view "$pr" --repo "$r" --json state -q .state 2>/dev/null || echo "")
        case "$st" in MERGED|CLOSED) why="PR $st";; esac
      fi
      if [ -z "$why" ] && [ "$(owner_live "$u" "$f")" = "dead" ]; then why="owner $o dead/stale"; fi
      [ -n "$why" ] || continue
      n=$((n+1))
      if [ "$dry" -eq 1 ]; then echo "would reap $key ($why)"; else rm -f "$f" "$SD/trail/$key.trail"; echo "reaped $key ($why)"; fi
    done
    # A merged PR releases its live claim immediately but preserves the trail through the
    # post-merge CI watch. Normal completion purges it explicitly; if that session dies,
    # reap a claimless trail only after the same stale-owner TTL so a healthy watch cannot
    # lose its history to another lander's concurrent reap.
    for t in "$SD/trail"/*.trail; do
      [ -f "$t" ] || continue
      name=${t##*/}; key=${name%.trail}
      [ -f "$CL/$key.json" ] && continue
      modified=$(stat -c %Y "$t" 2>/dev/null || stat -f %m "$t" 2>/dev/null || echo "")
      [ -n "$modified" ] || continue
      age=$(( $(date +%s) - modified ))
      [ "$age" -gt "$(( STALE_HOURS * 3600 ))" ] || continue
      n=$((n+1))
      if [ "$dry" -eq 1 ]; then echo "would reap $key (orphan trail stale ${age}s)"; else rm -f "$t"; echo "reaped $key (orphan trail stale ${age}s)"; fi
    done
    if [ "$dry" -eq 1 ]; then echo "REAPED=$n (dry-run)"; else echo "REAPED=$n"; fi
    exit 0;;

  *) usage;;
esac
