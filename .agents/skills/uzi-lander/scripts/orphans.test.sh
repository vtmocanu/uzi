#!/usr/bin/env bash
# Hermetic regression: orphans.sh classifies run PRs and in-flight runs by board claim
# (#PR, run-<RUN_ID>, none or dead owner), never reports an unverifiable owner as orphan,
# and fails closed on an unreadable lookup; claims.sh reap drops finished run-<RUN_ID> keys.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin" "$WORK/state/claims"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "pr list --repo test/repo --state open --limit 200 --json number,headRefName,title")
    [ "${GH_FAIL:-0}" = 1 ] && exit 1
    echo '[{"number":10,"headRefName":"agent/issue-1","title":"a"},{"number":11,"headRefName":"agent/issue-2","title":"b"},
           {"number":12,"headRefName":"uzi/self-improve","title":"c"},{"number":13,"headRefName":"renovate/x","title":"d"},
           {"number":14,"headRefName":"agent/issue-5","title":"e"},{"number":15,"headRefName":"agent/issue-6","title":"f"}]' ;;
  "pr view 70 --repo test/repo --json state -q .state") echo MERGED ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "repo list --json") echo '[{"id":"r1","path_with_namespace":"test/repo"},{"id":"r2","path_with_namespace":"other/repo"}]' ;;
  "run list --json")
    [ "${UZI_FAIL:-0}" = 1 ] && exit 1
    [ -n "${BIG_RUNS:-}" ] && { cat "$BIG_RUNS"; exit 0; }
    echo '[{"id":"aaaaaaaa-1","repo_id":"r1","mr_iid":10,"issue_iid":1,"kind":"issue","status":"completed","created_at":"2026-01-01T00:00:00Z"},
           {"id":"bbbbbbbb-1","repo_id":"r1","mr_iid":11,"issue_iid":2,"kind":"issue","status":"completed","created_at":"2026-01-01T00:00:00Z"},
           {"id":"bbbbbbbb-2","repo_id":"r1","mr_iid":11,"issue_iid":2,"kind":"mr_rework","status":"running","created_at":"2026-01-02T00:00:00Z"},
           {"id":"cccccccc-1","repo_id":"r1","mr_iid":12,"issue_iid":null,"kind":"self_improve","status":"completed","created_at":"2026-01-01T00:00:00Z"},
           {"id":"eeeeeeee-1","repo_id":"r1","mr_iid":14,"issue_iid":5,"kind":"issue","status":"completed","created_at":"2026-01-01T00:00:00Z"},
           {"id":"ffffffff-1","repo_id":"r1","mr_iid":15,"issue_iid":6,"kind":"issue","status":"completed","created_at":"2026-01-01T00:00:00Z"},
           {"id":"dddddddd-1","repo_id":"r1","mr_iid":null,"issue_iid":7,"kind":"issue","status":"running","created_at":"2026-01-01T00:00:00Z"},
           {"id":"dddddddd-2","repo_id":"r1","mr_iid":null,"issue_iid":null,"kind":"chat","status":"running","created_at":"2026-01-01T00:00:00Z"},
           {"id":"dddddddd-3","repo_id":"r1","mr_iid":null,"issue_iid":8,"kind":"issue","status":"completed","created_at":"2026-01-01T00:00:00Z"},
           {"id":"dddddddd-4","repo_id":"r2","mr_iid":null,"issue_iid":9,"kind":"issue","status":"running","created_at":"2026-01-01T00:00:00Z"}]' ;;
  # claims.sh reap's run-key lookups.
  "run get g-done --field status") echo completed ;;
  "run get g-done --field mr_iid") echo ;;
  "run get h-live --field status") echo running ;;
  "run get i-merged --field status") echo completed ;;
  "run get i-merged --field mr_iid") echo 70 ;;
  "run get j-unreadable --field status"|"run get j2-unreadable --field status") exit 6 ;;
  *) echo "unexpected uzi call: $*" >&2; exit 99 ;;
esac
STUB
# A verified registry listing this session and one other live session. state.sh runs
# `python3 peers.py list --json`; a python3 stub serves it, so the test needs no Python.
cat > "$WORK/bin/python3" <<'STUB'
#!/usr/bin/env bash
[ -n "${REGISTRY:-}" ] && { echo "$REGISTRY"; exit 0; }
echo '{"claude":[{"sessionId":"uuid-me","name":"me","socket":"/x"},{"sessionId":"uuid-other","name":"other","socket":"/y"}],"codex":[],"codex_schema_recognised":true}'
STUB
: > "$WORK/peers.py"
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi" "$WORK/bin/python3"
export PATH="$WORK/bin:$PATH" UZI_LANDER_STATE_DIR="$WORK/state" SESSION_PEERS_PY="$WORK/peers.py"
export SESSION_PEERS_NAME=me SESSION_PEERS_UUID=uuid-me SESSION_PEERS_KIND=claude
CL="$WORK/state/claims"
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
stale=2026-01-01T00:00:00Z   # far past the 6 h TTL
seed() {  # seed KEY OWNER UUID [PR] [LAST_SEEN]
  printf '{"key":"%s","repo":"test/repo","pr":%s,"owner":"%s","owner_uuid":"%s","kind":"claude","priority":0,"size_files":0,"depends_on":[],"last_seen":"%s","state":""}\n' \
    "$1" "${4:-null}" "$2" "$3" "${5:-$now}" > "$CL/$1.json"
}
seed '#10' other uuid-other 10
seed 'run-bbbbbbbb-1' other uuid-other
seed '#14' ghost uuid-gone 14
seed '#15' unverified unknown-host-1 15 "$stale"   # stale heartbeat, owner unverifiable

bash "$HERE/orphans.sh" --repo test/repo --json > "$WORK/o.json" 2> "$WORK/o.err" || fail "orphans exited $?: $(cat "$WORK/o.err")"
v() { jq -r --arg k "$1" '[.[]|select((.pr|tostring)==$k or .run==$k)]|if length==1 then .[0].verdict else "rows=\(length)" end' "$WORK/o.json"; }
want() { got=$(v "$1"); [ "$got" = "$2" ] || fail "$1: verdict $got, want $2: $(cat "$WORK/o.json")"; }
want 10 claimed
want 11 originator-claimed           # the mr_rework run is not the PR's run
want 12 orphan
want 14 orphan                       # only a dead owner's claim
want 15 claimed                      # unverifiable owner: never an orphan, however stale
want dddddddd-1 orphan               # in-flight run, no PR yet
[ "$(jq '[.[]|select(.pr==13)]|length' "$WORK/o.json")" = 0 ] || fail "renovate PR listed"
for r in dddddddd-2 dddddddd-3 dddddddd-4; do
  [ "$(jq --arg r "$r" '[.[]|select(.run==$r)]|length' "$WORK/o.json")" = 0 ] || fail "$r listed (chat, terminal or other repo)"
done
[ "$(jq -r '.[]|select(.pr==11)|.run' "$WORK/o.json")" = bbbbbbbb-1 ] || fail "PR 11 resolved to the rework run"
bash "$HERE/orphans.sh" --repo test/repo > "$WORK/o.txt" || fail "table mode exited $?"
grep -qE '^#12 +cccccccc +- +completed +- +- +orphan$' "$WORK/o.txt" || fail "table row: $(cat "$WORK/o.txt")"

# Fail closed: an unreadable lookup prints nothing and exits 3.
for f in UZI_FAIL GH_FAIL; do
  rc=0; env "$f=1" bash "$HERE/orphans.sh" --repo test/repo > "$WORK/fc.out" 2> /dev/null || rc=$?
  [ "$rc" = 3 ] || fail "$f: exit $rc, want 3"
  [ ! -s "$WORK/fc.out" ] || fail "$f: printed rows on a failed lookup: $(cat "$WORK/fc.out")"
done

# Fail closed: a corrupt claim file is never read as "no claim".
printf '{"key":"#10",' > "$CL/#10.json"
rc=0; bash "$HERE/orphans.sh" --repo test/repo > "$WORK/corrupt.out" 2> /dev/null || rc=$?
[ "$rc" = 3 ] || fail "corrupt claim: exit $rc, want 3: $(cat "$WORK/corrupt.out")"
[ ! -s "$WORK/corrupt.out" ] || fail "corrupt claim: printed rows: $(cat "$WORK/corrupt.out")"
rc=0; bash "$HERE/claims.sh" list --json > /dev/null 2>&1 || rc=$?
[ "$rc" = 3 ] || fail "claims.sh list on a corrupt claim: exit $rc, want 3"

# A claim missing its required fields is unreadable too.
printf '{"owner_uuid":"uuid-other"}\n' > "$CL/#10.json"
rc=0; bash "$HERE/orphans.sh" --repo test/repo > "$WORK/partial.out" 2> /dev/null || rc=$?
[ "$rc" = 3 ] && [ ! -s "$WORK/partial.out" ] || fail "partial claim: exit $rc: $(cat "$WORK/partial.out")"

# A malformed registry ({}) proves nothing: a listed owner's claim is never an orphan.
rm -f "$CL"/*.json
seed '#10' other uuid-other 10
REGISTRY='{}' bash "$HERE/orphans.sh" --repo test/repo --json > "$WORK/o.json" 2> /dev/null || fail "registry {}: exit $?"
want 10 claimed

# A run key's unverifiable owner is never taken over on the TTL (no heartbeat while the run
# implements); a '#PR' key keeps the documented TTL takeover.
rm -f "$CL"/*.json
seed run-x-1 unverified unknown-host-1 null "$stale"
rc=0; bash "$HERE/claims.sh" claim run-x-1 > "$WORK/rk.out" 2>&1 || rc=$?
[ "$rc" = 4 ] || fail "run key: stale unverifiable owner taken over (exit $rc): $(cat "$WORK/rk.out")"
seed '#20' unverified unknown-host-1 20 "$stale"
bash "$HERE/claims.sh" claim '#20' > "$WORK/pk.out" 2>&1 || fail "#PR key: TTL takeover refused: $(cat "$WORK/pk.out")"

# claims.sh reap: a finished run key goes, a live or unreadable one stays.
rm -f "$CL"/*.json
seed run-g-done other uuid-other
seed run-h-live other uuid-other
seed run-i-merged other uuid-other
seed run-j-unreadable other uuid-other
seed run-k-dead ghost uuid-gone
seed run-j2-unreadable unverified unknown-host-1 null "$stale"
printf '{"key":' > "$CL/run-z-corrupt.json"
bash "$HERE/claims.sh" reap --repo test/repo > "$WORK/reap.out" 2>&1 || fail "reap exited $?: $(cat "$WORK/reap.out")"
for k in run-g-done run-i-merged run-k-dead; do [ ! -f "$CL/$k.json" ] || fail "reap kept $k: $(cat "$WORK/reap.out")"; done
for k in run-h-live run-j-unreadable run-j2-unreadable run-z-corrupt; do [ -f "$CL/$k.json" ] || fail "reap dropped $k: $(cat "$WORK/reap.out")"; done

# A real run list is megabytes: it must never travel through argv (E2BIG, "Argument list too long").
rm -f "$CL"/*.json
jq -n '[range(0;6000)|{id:"bulk-\(.)",repo_id:"r1",mr_iid:null,issue_iid:null,kind:"chat",status:"completed",created_at:"2026-01-01T00:00:00Z",title:("x"*200)}]
  + [{id:"dddddddd-1",repo_id:"r1",mr_iid:null,issue_iid:7,kind:"issue",status:"running",created_at:"2026-01-01T00:00:00Z"}]' > "$WORK/big-runs.json"
[ "$(wc -c < "$WORK/big-runs.json")" -gt 1100000 ] || fail "big run list fixture too small to exceed ARG_MAX"
BIG_RUNS="$WORK/big-runs.json" bash "$HERE/orphans.sh" --repo test/repo --json > "$WORK/big.json" 2> "$WORK/big.err" \
  || fail "big run list: exit $?: $(cat "$WORK/big.err")"
[ "$(jq -r '.[]|select(.run=="dddddddd-1")|.verdict' "$WORK/big.json")" = orphan ] \
  || fail "big run list: dddddddd-1 not classified: $(head -c 400 "$WORK/big.json")"

# A lookup carrying two JSON values is unreadable, never its first value alone.
printf '[] []' > "$WORK/two-values.json"
rc=0; BIG_RUNS="$WORK/two-values.json" bash "$HERE/orphans.sh" --repo test/repo > "$WORK/two.out" 2> /dev/null || rc=$?
[ "$rc" = 3 ] && [ ! -s "$WORK/two.out" ] || fail "two-value run lookup: exit $rc: $(cat "$WORK/two.out")"

echo "PASS orphans: run PRs and in-flight runs classified by #PR / run-<RUN_ID> claim, fail-closed incl. a corrupt claim; unverifiable owners are never orphaned or reaped; reap drops finished run keys"
