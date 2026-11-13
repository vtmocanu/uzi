#!/usr/bin/env bash
# Hermetic regression: one claims.sh invocation fetches the session-peers registry
# (`python3 peers.py list --json`) at most once. lib/state.sh caches it in shell variables,
# and the liveness and identity helpers run inside $(...) subshells, so without a warm
# fetch in the parent every subshell refetched: claims.sh list made two fetches per claim.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/claims.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin" "$WORK/state/claims"
export REAL_PYTHON3 FETCH_LOG="$WORK/fetches"
REAL_PYTHON3=$(command -v python3)
# Stub only the registry call; count each one. Everything else (atomic_mkdir) runs for real.
cat > "$WORK/bin/python3" <<'STUB'
#!/usr/bin/env bash
if [ "$*" != "$SESSION_PEERS_PY list --json" ]; then exec "$REAL_PYTHON3" "$@"; fi
echo x >> "$FETCH_LOG"
if [ "${REG_BAD:-0}" = 1 ]; then echo 'registry unavailable'; exit 1; fi
echo '{"claude":[{"sessionId":"uuid-me","name":"me","socket":"/x"},{"sessionId":"uuid-other","name":"other","socket":"/y"}],"codex":[],"codex_schema_recognised":true}'
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "unexpected gh call: $*" >&2; exit 99
STUB
: > "$WORK/peers.py"
chmod +x "$WORK/bin/python3" "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH" UZI_LANDER_STATE_DIR="$WORK/state" SESSION_PEERS_PY="$WORK/peers.py"
# Resolve identity through the registry (the CLAUDE_CODE_MESSAGING_SOCKET path), not overrides.
unset SESSION_PEERS_NAME SESSION_PEERS_UUID SESSION_PEERS_KIND CODEX_THREAD_ID CODEX_SESSION_ID \
  OPENCODE_SESSION_ID CLAUDE_CODE_SESSION_ID 2>/dev/null || true
export CLAUDE_CODE_MESSAGING_SOCKET=/x
CL="$WORK/state/claims"

seed() { # seed <key> <owner> <uuid>
  printf '{"key":"%s","repo":"test/repo","pr":null,"owner":"%s","owner_uuid":"%s","kind":"claude","last_seen":"%s","priority":1,"depends_on":[],"state":""}\n' \
    "$1" "$2" "$3" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$CL/$1.json"
}
fetches() { if [ -f "$FETCH_LOG" ]; then wc -l < "$FETCH_LOG" | tr -d ' '; else echo 0; fi; }
run() { # run <tag> <want-rc> <claims.sh args...>
  local tag=$1 want=$2 rc=0; shift 2
  rm -f "$FETCH_LOG"
  bash "$SCRIPT" "$@" > "$WORK/$tag.out" 2>&1 || rc=$?
  [ "$rc" -eq "$want" ] || fail "$tag: exit $rc, want $want: $(cat "$WORK/$tag.out")"
}
once() { [ "$(fetches)" -eq 1 ] || fail "$1: $(fetches) registry fetches, want 1"; }

seed a other uuid-other; seed b other uuid-other; seed c other uuid-other

# list: before the fix, 2 per claim (owner_live + registry_live), so 6 here.
run list 0 list
grep -qF 'live' "$WORK/list.out" || fail "list: no liveness column: $(cat "$WORK/list.out")"
once list

# claim on a key another live session holds: self_identity + owner_live, 2 before the fix.
run claim 4 claim a --repo test/repo
grep -qF 'CLAIM_HELD_BY=other' "$WORK/claim.out" || fail "claim: $(cat "$WORK/claim.out")"
once claim

# reap: one owner_live per claim, 3 before the fix. Live owners keep every claim.
run reap 0 reap --dry-run
grep -qF 'REAPED=0 (dry-run)' "$WORK/reap.out" || fail "reap: $(cat "$WORK/reap.out")"
once reap

# Explicit identity needs no registry for a free key, and one fetch for another's key.
run explicit_free 0 claim d --repo test/repo --as me --uuid uuid-me --kind claude
[ "$(fetches)" -eq 0 ] || fail "explicit-identity claim of a free key fetched the registry"
run explicit_held 4 claim a --repo test/repo --as me --uuid uuid-me --kind claude
once explicit_held
rm -f "$CL/d.json"

# A failed registry is fetched once and cached: liveness stays unknown, never dead.
export REG_BAD=1
run bad_list 0 list
grep -qF 'unknown' "$WORK/bad_list.out" || fail "bad registry: liveness not unknown: $(cat "$WORK/bad_list.out")"
once bad_list
run bad_reap 0 reap --dry-run
grep -qF 'REAPED=0 (dry-run)' "$WORK/bad_reap.out" || fail "bad registry reaped a fresh claim: $(cat "$WORK/bad_reap.out")"
once bad_reap
unset REG_BAD

# An unverifiable (unknown-*) owner is never looked up, so its board costs nothing.
rm -f "$CL"/*.json; seed e host unknown-host-1
run unknown_list 0 list
[ "$(fetches)" -eq 0 ] || fail "list of unknown-* owners fetched the registry"

# Verbs that need no registry still never fetch, and an empty board costs nothing.
run touch 0 touch e --state x
[ "$(fetches)" -eq 0 ] || fail "touch fetched the registry"
rm -f "$CL"/*.json
run empty 0 list
[ "$(fetches)" -eq 0 ] || fail "list on an empty board fetched the registry"

echo "PASS claims-registry-fetch: list, claim and reap fetch the session-peers registry at most once per invocation (a failure stays cached and unknown); no-registry paths stay free"
