#!/usr/bin/env bash
# Hermetic regression: takeover.sh honours a dispatch-time run-<RUN_ID> claim. The session
# that dispatched a run claims it (uzi-watcher hand-off) and lands it; another session taking
# over that run, by run id or by PR number, must stop at NEXT=claimed_by_other. A run key held
# by this session or by a dead owner is superseded: claiming the PR releases it.
set -eu
export NET_RETRY_BASE_SLEEP=0

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/takeover.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin" "$WORK/state/claims"
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
run() { printf '{"id":"run-1","repo_id":"r1","mr_iid":%s,"kind":"issue","status":"%s","created_at":"2026-01-01T00:00:00Z","mr_rework_enabled":false}' "${RUN_PR:-null}" "${RUN_STATUS:-running}"; }
case "$*" in
  "repo list --json") echo '[{"id":"r1","path_with_namespace":"test/repo"}]' ;;
  "run list --json") printf '['; run; printf ']\n' ;;
  "run get run-1 --json") run; echo ;;
  *) echo "unexpected uzi call: $*" >&2; exit 99 ;;
esac
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Only the PR view must succeed for the snapshot to reach the claim; every other lookup
# failing just makes the snapshot UNKNOWN, which is not under test here.
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then
  # INJECT_KEY: another live session takes the run key between takeover's run claim and its
  # PR claim (a forced interleaving; the PR view runs in between).
  if [ -n "${INJECT_KEY:-}" ]; then
    printf '{"key":"run-run-1","repo":"test/repo","pr":null,"owner":"other","owner_uuid":"uuid-other","kind":"claude","last_seen":"%s","state":""}\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$INJECT_KEY"
  fi
  echo '{"number":42,"state":"OPEN","isDraft":false,"headRefOid":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","headRefName":"agent/issue-1","baseRefName":"main","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"","title":"t"}'
  exit 0
fi
exit 1
STUB
# A verified registry listing this session and one other live session. state.sh runs
# `python3 peers.py list --json`; a python3 stub serves it, so the test needs no Python.
cat > "$WORK/bin/python3" <<'STUB'
#!/usr/bin/env bash
echo '{"claude":[{"sessionId":"uuid-me","name":"me","socket":"/x"},{"sessionId":"uuid-other","name":"other","socket":"/y"}],"codex":[],"codex_schema_recognised":true}'
STUB
: > "$WORK/peers.py"
chmod +x "$WORK/bin/uzi" "$WORK/bin/gh" "$WORK/bin/python3"
export PATH="$WORK/bin:$PATH" UZI_LANDER_STATE_DIR="$WORK/state" SESSION_PEERS_PY="$WORK/peers.py"
export SESSION_PEERS_NAME=me SESSION_PEERS_UUID=uuid-me SESSION_PEERS_KIND=claude
CL="$WORK/state/claims"

seed() { printf '{"key":"run-run-1","repo":"test/repo","pr":null,"owner":"%s","owner_uuid":"%s","kind":"claude","last_seen":"%s","state":""}\n' "$1" "$2" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$CL/run-run-1.json"; }
snap() { local tag=$1; shift; rc=0; bash "$SCRIPT" "$@" --repo test/repo > "$WORK/$tag.out" 2>&1 || rc=$?; echo "$rc" > "$WORK/$tag.rc"; }
has() { grep -qF -- "$2" "$WORK/$1.out" || fail "$1: missing '$2': $(cat "$WORK/$1.out")"; }
hasnt() { if grep -qF -- "$2" "$WORK/$1.out"; then fail "$1: unexpected '$2': $(cat "$WORK/$1.out")"; fi; }
rc_is() { [ "$(cat "$WORK/$1.rc")" = "$2" ] || fail "$1: exit $(cat "$WORK/$1.rc"), want $2: $(cat "$WORK/$1.out")"; }

# 1. Another live session dispatched the run: a takeover by run id stops, even mid-run.
seed other uuid-other
export RUN_STATUS=running
snap active run-1
has active 'CLAIM_HELD_BY=other'; has active 'NEXT=claimed_by_other'; rc_is active 4
hasnt active 'NEXT=run_active'
# --no-claim stays a pure snapshot.
snap noclaim run-1 --no-claim
has noclaim 'NEXT=run_active'; rc_is noclaim 0

# 2. Same, entered by PR number once the run opened PR #42: no '#42' claim is written.
export RUN_STATUS=completed RUN_PR=42
snap bypr 42
has bypr 'NEXT=claimed_by_other'; rc_is bypr 4
[ ! -f "$CL/#42.json" ] || fail "bypr: claimed #42 over the dispatching session"

# 3. A dead owner's run key is superseded: the PR is claimed and the run key released.
seed ghost uuid-gone
snap dead run-1
has dead 'CLAIMED=#42 OWNER=me'; hasnt dead 'NEXT=claimed_by_other'
[ ! -f "$CL/run-run-1.json" ] || fail "dead: run key not released"
rm -f "$CL/#42.json"

# 4. This session's own run key converts to the PR claim.
seed me uuid-me
snap own run-1
has own 'CLAIMED=#42 OWNER=me'
[ ! -f "$CL/run-run-1.json" ] || fail "own: run key not released"

# 5. Two concurrent takeovers of an unclaimed active run: exactly one proceeds.
rm -f "$CL"/*.json
export RUN_STATUS=running RUN_PR=null
for who in me other; do
  ( SESSION_PEERS_NAME=$who SESSION_PEERS_UUID=uuid-$who snap "race-$who" run-1 ) &
done
wait
n_go=$(cat "$WORK/race-me.out" "$WORK/race-other.out" | grep -c 'NEXT=run_active' || true)
n_stop=$(cat "$WORK/race-me.out" "$WORK/race-other.out" | grep -c 'NEXT=claimed_by_other' || true)
[ "$n_go" = 1 ] && [ "$n_stop" = 1 ] || fail "race: proceeded=$n_go stopped=$n_stop: $(cat "$WORK/race-me.out" "$WORK/race-other.out")"
winner=$(jq -r .owner_uuid "$CL/run-run-1.json")
case "$winner" in uuid-me|uuid-other) ;; *) fail "race: run key owner $winner";; esac

# 6. Another live session takes the run key after our run claim, before our PR claim: the
# conversion never deletes its key.
rm -f "$CL"/*.json
export RUN_STATUS=completed RUN_PR=42
INJECT_KEY="$CL/run-run-1.json" snap steal run-1
has steal 'CLAIMED=#42 OWNER=me'
[ -f "$CL/run-run-1.json" ] || fail "steal: deleted another session's run key: $(cat "$WORK/steal.out")"
[ "$(jq -r .owner_uuid "$CL/run-run-1.json")" = uuid-other ] || fail "steal: run key owner changed"
rm -f "$CL"/*.json

# 7. The PR is another live session's: the run key this takeover just took is released, so
# it never blocks the PR's lander on its next takeover.
rm -f "$CL"/*.json
printf '{"key":"#42","repo":"test/repo","pr":42,"owner":"other","owner_uuid":"uuid-other","kind":"claude","last_seen":"%s","state":""}\n' \
  "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$CL/#42.json"
export RUN_STATUS=completed RUN_PR=42
snap strand run-1
has strand 'NEXT=claimed_by_other'; rc_is strand 4
[ ! -f "$CL/run-run-1.json" ] || fail "strand: run key left behind: $(cat "$CL/run-run-1.json")"
rm -f "$CL"/*.json

echo "PASS takeover-run-claim: a live dispatcher's run-<RUN_ID> claim stops other landers; an own or dead one converts to the PR claim; acquisition and conversion are serialised"
