# shellcheck shell=bash
# state.sh — the shared state directory and the session identity for the uzi-lander
# scripts. Sourced (never executed); defines state_dir, peers_json, self_identity, is_live.
#
# STATE DIR: $UZI_LANDER_STATE_DIR, else <git common dir>/uzi-lander — the main checkout's
# .git/, which every worktree of the repo shares, is never tracked, and needs no .gitignore
# entry — else /tmp/uzi-lander when outside a checkout. Sub-dirs: trail/ claims/ locks/.
# A separate `git clone` has its own .git and so its own state; pass the env to share.
#
# IDENTITY (name, durable uuid, kind): --as/--uuid flags on the calling script win; then
# SESSION_PEERS_NAME/SESSION_PEERS_UUID/SESSION_PEERS_KIND; then a Claude Code session via
# CLAUDE_CODE_MESSAGING_SOCKET matched in the session-peers registry (`peers.py list
# --json`, .claude[].socket); then a Codex thread via CODEX_THREAD_ID (.codex[].id); else
# host-pid with kind "unknown" (a warning — such a claim cannot be liveness-checked and
# ages out on the TTL). The uuid is what other sessions address and what liveness keys on;
# the name is a mutable alias.
#
# LIVENESS: a uuid is live when the registry lists it as a Claude session (.claude[]
# .sessionId, which includes Codex shims) or as a Codex thread with a holder process
# (.codex[] .holder_pid). Registry unavailable => unknown (rc 2), and callers fall back to
# the last_seen TTL (UZI_LANDER_STALE_HOURS, default 6).
PEERS_PY="${SESSION_PEERS_PY:-$HOME/.claude/skills/session-peers/scripts/peers.py}"
# shellcheck disable=SC2034  # read by claims.sh (owner_live's TTL) after it sources this lib
STALE_HOURS="${UZI_LANDER_STALE_HOURS:-6}"

state_dir() {
  local d
  if [ -n "${UZI_LANDER_STATE_DIR:-}" ]; then d="$UZI_LANDER_STATE_DIR"
  elif d=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) && [ -n "$d" ]; then d="$d/uzi-lander"
  else d="/tmp/uzi-lander"; fi
  mkdir -p "$d/trail" "$d/claims" "$d/locks" 2>/dev/null || { echo "cannot create state dir $d" >&2; return 1; }
  printf '%s' "$d"
}

# The registry, fetched at most once per process. Empty when peers.py is absent or fails.
_PEERS_CACHE=""; _PEERS_FETCHED=0
peers_json() {
  if [ "$_PEERS_FETCHED" -eq 0 ]; then
    _PEERS_FETCHED=1
    if [ -f "$PEERS_PY" ]; then _PEERS_CACHE=$(python3 "$PEERS_PY" list --json 2>/dev/null || true); fi
    printf '%s' "$_PEERS_CACHE" | jq -e . >/dev/null 2>&1 || _PEERS_CACHE=""
  fi
  printf '%s' "$_PEERS_CACHE"
}

# Prints "name<TAB>uuid<TAB>kind". $1/$2/$3 optional overrides (name uuid kind).
self_identity() {
  local name="${1:-${SESSION_PEERS_NAME:-}}" uuid="${2:-${SESSION_PEERS_UUID:-}}" kind="${3:-${SESSION_PEERS_KIND:-}}" pj row
  if [ -z "$uuid" ]; then
    pj=$(peers_json)
    if [ -n "${CLAUDE_CODE_MESSAGING_SOCKET:-}" ] && [ -n "$pj" ]; then
      row=$(printf '%s' "$pj" | jq -r --arg s "$CLAUDE_CODE_MESSAGING_SOCKET" \
        '.claude[]?|select(.socket==$s)|"\(.name)\t\(.sessionId)\t\(if .entrypoint=="codex" then "codex" else "claude" end)"' 2>/dev/null | head -1)
      if [ -n "$row" ]; then
        name=$(printf '%s' "$row" | cut -f1); uuid=$(printf '%s' "$row" | cut -f2); kind=$(printf '%s' "$row" | cut -f3)
      fi
    fi
    if [ -z "$uuid" ] && [ -n "${CODEX_THREAD_ID:-}" ]; then
      uuid="$CODEX_THREAD_ID"; kind="codex"
      name=$(printf '%s' "$pj" | jq -r --arg u "$uuid" '.codex[]?|select(.id==$u)|(.peer_name // .name // "")' 2>/dev/null | head -1)
      [ -n "$name" ] || name="codex-${uuid:0:8}"
    fi
  fi
  if [ -z "$uuid" ]; then
    name="${name:-$(hostname -s)-$$}"; uuid="unknown-$(hostname -s)-$$"; kind="${kind:-unknown}"
    echo "warning: session identity not resolved (no session-peers registry match); claiming as $name" >&2
  fi
  printf '%s\t%s\t%s\n' "$name" "$uuid" "${kind:-claude}"
}

# is_live <uuid>: 0 live, 1 dead, 2 unknown (no registry).
is_live() {
  local pj n
  case "$1" in unknown-*) return 2;; esac
  pj=$(peers_json); [ -n "$pj" ] || return 2
  n=$(printf '%s' "$pj" | jq -r --arg u "$1" \
    '([.claude[]?|select(.sessionId==$u)] + [.codex[]?|select(.id==$u and .holder_pid!=null)])|length' 2>/dev/null || echo 0)
  [ "${n:-0}" -gt 0 ]
}

now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }
iso2epoch() { jq -rn --arg t "$1" '$t|sub("\\.[0-9]+";"")|fromdateiso8601' 2>/dev/null; }
