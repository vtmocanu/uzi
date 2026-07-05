#!/usr/bin/env bash
# M6 end-to-end harness for the uzi agent runtime.
#
# Stands up an ISOLATED compose stack (unique -p, per-run --env-file + scratch
# dir) with DUMMY credentials and the STUB executor, then drives the full
# Success-Criteria path with NO live Anthropic session and NO real GitLab:
#
#   seed admin+forge+repo (boot) → issue a worker join token → worker online →
#   create a PRD issue → start a run → plan gate (awaiting_approval) →
#   approve → stub implement → worker pushes agent/issue-N to a local bare remote
#   + opens an MR against the fake GitLab → completed (branch + mr_iid).
#
# Plus restart-resilience (down/up keeping volumes, mid-run at the gate → the run
# is re-queued, re-claimed, and driven to completion with a gapless seq) and the
# server-side cancel path.
#
# Finally, the PRD #24 MR-close watcher: with the poller sped to ~2s, closing the
# completed run's MR (without merging, via forge-fake's /_e2e mutator) moves the
# card Human Review → In Progress; reopening restores it; and a manual drag is
# never fought (the reopen edge's source-column guard backs off).
#
# Observable assertions: DB run-state transitions (via the API), gapless
# run_messages seq, branch pushed to the remote, MR recorded by the fake GitLab,
# NO secret (PAT / Anthropic token / worker join token) in container logs or on
# the worker's disk, and — the M6 /proc hardening — the join token is absent from
# every process's /proc/<pid>/environ and its delivery file was unlinked.
#
# Everything tears down with `down -v`; the user's own `uzi` stack is never
# touched (unique project name, project-scoped volumes, its own env-file).
#
# Executor switch (for the OPTIONAL, user-gated live capstone — see README):
#   UZI_E2E_EXECUTOR=sdk  runs the real Claude Agent SDK instead of the stub.
#   Default is stub. The live path additionally needs a real seeded token and is
#   never exercised automatically.

set -euo pipefail

# --- layout ------------------------------------------------------------------
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXECUTOR="${UZI_E2E_EXECUTOR:-stub}"
PROJECT="${UZI_E2E_COMPOSE_PROJECT:-uzi-e2e-$$}"
# A real SDK agent turn takes minutes (observed ~13m: the seeded template spawns
# a reviewer subagent), where the stub finishes in seconds.
[ "$EXECUTOR" = sdk ] && COMPLETE_TIMEOUT_DEFAULT=1800 || COMPLETE_TIMEOUT_DEFAULT=90
RUNROOT="${E2E_RUN_DIR:-${TMPDIR:-/tmp}/uzi-e2e-$$}"
RUNROOT="${RUNROOT%/}"
ENVFILE="$RUNROOT/e2e.env"
JAR="$RUNROOT/admin.jar"
KEEP="${KEEP_RUNDIR:-}"

# Dummy credentials (must match the api overlay's seed literals).
ADMIN_EMAIL="admin@uzi.e2e"
ADMIN_PASS="e2e-admin-password-000000"
DUMMY_FORGE_PAT="e2e-dummy-forge-pat-000000"
DUMMY_ANTHROPIC="sk-ant-e2e-dummy-do-not-use-000000"

WEB_PORT="$(( 20000 + (RANDOM % 20000) ))"
BASE="http://127.0.0.1:${WEB_PORT}"

COMPOSE=(docker compose -p "$PROJECT" --project-directory "$ROOT" --env-file "$ENVFILE"
  -f "$ROOT/docker-compose.yml" -f "$ROOT/e2e/docker-compose.e2e.yml" --profile agent)

# --- output helpers ----------------------------------------------------------
say()  { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

cleanup() {
  local code=$?
  # KEEP_STACK leaves the whole stack running (containers + volumes + rundir) so
  # the auditor can inspect logs, the claim payload path, and the worker's /data
  # against a live run. Tear it down manually with the printed command.
  if [ -n "${KEEP_STACK:-}" ]; then
    say "leaving the stack UP for inspection (KEEP_STACK set)"
    printf '  project:  %s\n  web:      %s\n  rundir:   %s\n' "$PROJECT" "$BASE" "$RUNROOT"
    printf '  logs:     docker compose -p %s logs\n' "$PROJECT"
    printf '  worker:   docker compose -p %s exec agent sh\n' "$PROJECT"
    printf '  teardown: docker compose -p %s --env-file %s -f %s -f %s --profile agent down -v\n' \
      "$PROJECT" "$ENVFILE" "$ROOT/docker-compose.yml" "$ROOT/e2e/docker-compose.e2e.yml"
    exit $code
  fi
  say "tearing down (down -v)"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  [ -n "$KEEP" ] || rm -rf "$RUNROOT"
  exit $code
}
trap cleanup EXIT

WTOKEN=""  # the worker join token, minted once and re-written before each start
write_token() { mkdir -p "$RUNROOT/worker-secret"; printf '%s' "$WTOKEN" > "$RUNROOT/worker-secret/token"; }

# --- api helpers (session cookie + CSRF, like scripts/smoke.sh) --------------
csrf() { awk '$6=="uzi_csrf"{print $7}' "$JAR"; }
login() {
  curl -fsS -c "$JAR" -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" >/dev/null
}
apiget()  { curl -fsS -b "$JAR" "$BASE$1"; }
apipost() { curl -fsS -b "$JAR" -X POST "$BASE$1" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(csrf)" -d "$2"; }
apiput()  { curl -fsS -b "$JAR" -X PUT "$BASE$1" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(csrf)" -d "$2"; }
# apiput_code — like apiput but never -f: echoes the HTTP status and swallows the
# body, so the caller can assert on 200-vs-4xx (the concurrent-PUT race).
apiput_code() { curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" -X PUT "$BASE$1" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" -d "$2"; }

wait_http() {
  local deadline=$((SECONDS + 90))
  while [ $SECONDS -lt $deadline ]; do
    curl -fsS "$BASE/api/health" >/dev/null 2>&1 && return 0
    sleep 1
  done
  fail "api health never came up at $BASE"
}

wait_worker_online() {
  local deadline=$((SECONDS + 40)) s
  while [ $SECONDS -lt $deadline ]; do
    s="$(apiget /api/workers | jq -r '.workers[0].status // empty')"
    [ "$s" = online ] && return 0
    sleep 2
  done
  fail "worker never reached online (last status: ${s:-none})"
}

# wait_status RUN WANT [TIMEOUT] — poll a run until it reaches WANT; abort early
# if it lands in an unexpected terminal state.
wait_status() {
  local run="$1" want="$2" timeout="${3:-90}" deadline=$((SECONDS + ${3:-90})) s
  while [ $SECONDS -lt $deadline ]; do
    s="$(apiget "/api/runs/$run" | jq -r '.run.status')"
    [ "$s" = "$want" ] && return 0
    case "$s" in
      failed|cancelled)
        [ "$s" = "$want" ] && return 0
        local reason
        reason="$(apiget "/api/runs/$run" | jq -r '.run.failure_reason // empty')"
        fail "run $run entered '$s' (${reason:-no reason}) while waiting for '$want'";;
    esac
    sleep 2
  done
  fail "timeout: run $run never reached '$want' (last: ${s:-none})"
}

# card_column IID — the board's resolved column for one issue (empty = Open).
card_column() {
  apiget "/api/repos/$REPO_ID/board" | jq -r --argjson iid "$1" \
    '.board.cards[] | select(.iid==$iid) | .column'
}

# wait_card_column IID WANT [TIMEOUT] — poll the board until a card resolves to
# WANT (used for the MR-close watcher's async, poller-driven moves).
wait_card_column() {
  local iid="$1" want="$2" deadline=$((SECONDS + ${3:-40})) c
  while [ $SECONDS -lt $deadline ]; do
    c="$(card_column "$iid")"
    [ "$c" = "$want" ] && return 0
    sleep 2
  done
  fail "timeout: card #$iid never reached column '$want' (last: '${c:-none}')"
}

# flip_mr IID STATE — drive forge-fake's /_e2e state mutator (the harness stand-in
# for a reviewer closing/reopening/merging an MR). Run from inside the agent, which
# resolves forge-fake.e2e and trusts its self-signed cert (NODE_EXTRA_CA_CERTS).
flip_mr() {
  local iid="$1" state="$2"
  "${COMPOSE[@]}" exec -T agent node -e '
    const https=require("https");
    const data=JSON.stringify({state:process.argv[2]});
    const req=https.request({hostname:"forge-fake.e2e",port:443,method:"POST",
      path:`/_e2e/mrs/${process.argv[1]}/state`,
      headers:{"Content-Type":"application/json","Content-Length":Buffer.byteLength(data)}},
      r=>{let b="";r.on("data",c=>b+=c);r.on("end",()=>{
        if(r.statusCode!==200){console.error("flip failed",r.statusCode,b);process.exit(1)}});});
    req.on("error",e=>{console.error(e.message);process.exit(2)});
    req.write(data);req.end();
  ' "$iid" "$state" >/dev/null
}

# --- autopilot (PRD #19) helpers ---------------------------------------------
# fake_post PATH JSON — POST to a forge-fake /_e2e mutator from inside the agent
# (which resolves forge-fake.e2e and trusts its cert), echoing the response body.
# Mirrors flip_mr; used to stage PRD/autopilot-labelled issues and label events
# the way a human filing/labelling an issue would, which uzi's own CreateIssue
# (PRD-label-only) cannot.
fake_post() {
  "${COMPOSE[@]}" exec -T agent node -e '
    const https=require("https");
    const data=process.argv[2];
    const req=https.request({hostname:"forge-fake.e2e",port:443,method:"POST",
      path:process.argv[1],
      headers:{"Content-Type":"application/json","Content-Length":Buffer.byteLength(data)}},
      r=>{let b="";r.on("data",c=>b+=c);r.on("end",()=>{
        process.stdout.write(b);
        if(r.statusCode>=300){console.error("fake_post",r.statusCode,b);process.exit(1)}});});
    req.on("error",e=>{console.error(e.message);process.exit(2)});
    req.write(data);req.end();
  ' "$1" "$2"
}

# fake_state — the fake's persisted record (issues, MRs, notes, label events).
fake_state() { "${COMPOSE[@]}" exec -T forge-fake cat /state/state.json; }

# note_count IID / notes_text IID — issue-comment introspection.
note_count() { fake_state | jq --argjson iid "$1" '[.notes[]? | select(.issue_iid==$iid)] | length'; }
notes_text() { fake_state | jq -r --argjson iid "$1" '.notes[]? | select(.issue_iid==$iid) | .body'; }

# add_label_event IID ACTION USERNAME — append an add/remove of the `autopilot`
# label by USERNAME (a fresh, larger event id each time).
add_label_event() {
  fake_post "/_e2e/issues/$1/label-events" \
    "$(jq -nc --arg ac "$2" --arg u "$3" '{action:$ac,label:"autopilot",username:$u}')" >/dev/null
}

# create_autopilot_issue TITLE DESC ADDER AUTHOR — stage a PRD+autopilot issue on
# the fake authored by AUTHOR, then record the initial autopilot-label add by
# ADDER. Echoes the new issue iid.
create_autopilot_issue() {
  local iid
  iid="$(fake_post /_e2e/issues \
    "$(jq -nc --arg t "$1" --arg d "$2" --arg a "$4" '{title:$t,description:$d,labels:["PRD","autopilot"],author:$a}')" \
    | jq -r '.iid')"
  [ -n "$iid" ] && [ "$iid" != null ] || fail "could not stage autopilot issue on the fake"
  add_label_event "$iid" add "$3"
  printf '%s' "$iid"
}

# wait_run_for_issue IID [TIMEOUT] — poll until an autopilot run exists for IID
# (the poller creates it unattended); echoes the run id.
wait_run_for_issue() {
  local iid="$1" deadline=$((SECONDS + ${2:-40})) rid
  while [ $SECONDS -lt $deadline ]; do
    rid="$(apiget "/api/runs?issue_iid=$iid" | jq -r '.runs[0].id // empty')"
    [ -n "$rid" ] && { printf '%s' "$rid"; return 0; }
    sleep 1
  done
  fail "timeout: no autopilot run appeared for issue #$iid"
}

# assert_no_run_for_issue IID [SETTLE] — let a few detector ticks pass, then prove
# the issue only drew a comment, never a run.
assert_no_run_for_issue() {
  sleep "${2:-8}"
  local rid; rid="$(apiget "/api/runs?issue_iid=$1" | jq -r '.runs[0].id // empty')"
  [ -z "$rid" ] || fail "issue #$1 unexpectedly spawned run $rid (autopilot should have only commented)"
}

# wait_autopilot_done RUN WANT [TIMEOUT] — like wait_status, but treats
# awaiting_approval as a hard failure: an autopilot run that parks at the plan
# gate means auto-approve is broken (the run would hang there forever).
wait_autopilot_done() {
  local run="$1" want="$2" deadline=$((SECONDS + ${3:-120})) s
  while [ $SECONDS -lt $deadline ]; do
    s="$(apiget "/api/runs/$run" | jq -r '.run.status')"
    [ "$s" = "$want" ] && return 0
    [ "$s" = awaiting_approval ] && fail "autopilot run $run parked at awaiting_approval — auto-approve did not fire"
    case "$s" in
      failed|cancelled) [ "$s" = "$want" ] || fail "autopilot run $run entered '$s' while waiting for '$want'";;
    esac
    sleep 1
  done
  fail "timeout: autopilot run $run never reached '$want' (last: ${s:-none})"
}

# wait_notes IID WANT [TIMEOUT] — poll until IID has exactly WANT comments; fail
# fast if it ever exceeds WANT (the exactly-once guarantee is the whole point).
wait_notes() {
  local iid="$1" want="$2" deadline=$((SECONDS + ${3:-40})) n
  while [ $SECONDS -lt $deadline ]; do
    n="$(note_count "$iid")"
    [ "$n" = "$want" ] && return 0
    { [ -n "$n" ] && [ "$n" -gt "$want" ] 2>/dev/null; } && fail "issue #$iid has $n comments, expected $want (over-commented)"
    sleep 1
  done
  fail "timeout: issue #$iid never reached $want comment(s) (last: ${n:-none})"
}

# =============================================================================
say "provisioning scratch dir $RUNROOT (project $PROJECT, web $BASE, executor $EXECUTOR)"
mkdir -p "$RUNROOT/certs" "$RUNROOT/worker-secret" "$RUNROOT/agent-gitconfig" "$RUNROOT/fakeremote" "$RUNROOT/forge-fake-state"
chmod a+rwX "$RUNROOT/forge-fake-state"  # forge-fake persists its recorded state here (survives the restart)

# Self-signed cert for forge-fake.e2e (trusted by api/worker/git in the overlay).
cat > "$RUNROOT/certs/openssl.cnf" <<'EOF'
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = forge-fake.e2e
[v3]
basicConstraints = critical,CA:TRUE
subjectAltName = DNS:forge-fake.e2e
EOF
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -keyout "$RUNROOT/certs/key.pem" -out "$RUNROOT/certs/cert.pem" \
  -config "$RUNROOT/certs/openssl.cnf" >/dev/null 2>&1
chmod 0644 "$RUNROOT/certs/"*.pem

# Local bare repo standing in for GitLab's git server, seeded with a main commit.
git init --bare -q "$RUNROOT/fakeremote/repo.git"
git -C "$RUNROOT/fakeremote/repo.git" symbolic-ref HEAD refs/heads/main
# Allow pushes over git smart-HTTP (the E2E_GIT_SMART_HTTP variant); a no-op for
# the default local-path remote.
git -C "$RUNROOT/fakeremote/repo.git" config http.receivepack true
seedwc="$RUNROOT/.seedwc"
git -C "$RUNROOT" clone -q "$RUNROOT/fakeremote/repo.git" .seedwc
git -C "$seedwc" checkout -q -b main
printf '# repo\n\nSeeded by the uzi M6 E2E harness.\n' > "$seedwc/README.md"
git -C "$seedwc" add README.md
git -C "$seedwc" -c user.name=seed -c user.email=seed@uzi.e2e -c commit.gpgsign=false commit -q -m "seed: initial commit"
git -C "$seedwc" push -q origin main
rm -rf "$seedwc"
chmod -R a+rwX "$RUNROOT/fakeremote"

if [ -n "${E2E_GIT_SMART_HTTP:-}" ]; then
  # Fidelity variant: leave the clone/push URL pointing at forge-fake's git
  # smart-HTTP endpoint (no insteadOf), so the worker's git-over-HTTPS Basic auth
  # is genuinely exercised (forge-fake 401s without a valid Authorization: Basic).
  : > "$RUNROOT/agent-gitconfig/gitconfig"
  GIT_MODE="smart-HTTP (Basic auth exercised)"
else
  # Default: rewrite the https clone/push URL to the local bare remote (fast,
  # hermetic; does NOT exercise git-over-HTTPS auth — see README).
  cat > "$RUNROOT/agent-gitconfig/gitconfig" <<'EOF'
[url "/fakeremote/repo.git"]
	insteadOf = https://forge-fake.e2e/group/repo.git
EOF
  GIT_MODE="local bare remote via insteadOf"
fi
say "git remote mode: $GIT_MODE"

# Per-run env-file: strong generated secrets for the base stack + the scratch dir
# the overlay bind-mounts. UZI_WORKER_TOKEN is a placeholder that only satisfies
# the base compose's `:?` guard — the real token is delivered via the file.
cat > "$ENVFILE" <<EOF
E2E_RUN_DIR=$RUNROOT
E2E_WEB_PORT=$WEB_PORT
UZI_E2E_EXECUTOR=$EXECUTOR
POSTGRES_USER=uzi
POSTGRES_DB=uzi
POSTGRES_PASSWORD=$(openssl rand -hex 16)
JWT_SECRET=$(openssl rand -hex 64)
UZI_SECRET_KEY=$(openssl rand -base64 32)
UZI_WORKER_TOKEN=e2e-placeholder-unused
EOF

# --- build + bring up the control plane (no worker yet) ----------------------
say "building images"
"${COMPOSE[@]}" build >/dev/null

say "starting db + api + web + forge-fake"
"${COMPOSE[@]}" up -d --wait db api web forge-fake
wait_http
pass "control plane up; boot seed provisioned admin + forge + repo"

login
REPO_ID="$(apiget /api/repos | jq -r '.repos[0].id // empty')"
[ -n "$REPO_ID" ] || fail "no enabled repo after seed (expected group/repo)"
pass "admin logged in; repo $REPO_ID enabled"

# Open the board once up front so its columns are seeded (as labels on the fake).
# An unopened board has no columns, which would let the run-lifecycle's
# single-column moves leave a card carrying two column labels; seeding first keeps
# the column state clean for the run-lifecycle path and the PRD #24 MR-close phase.
apiget "/api/repos/$REPO_ID/board" >/dev/null
pass "board columns seeded"

# --- cancel path (server-side, before any worker is online) ------------------
say "cancel path: a queued run is cancelled server-side (no live poller)"
IID_C="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E cancel","description":"cancel me — see prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_C="$(apipost "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$IID_C}" | jq -r '.run.id')"
[ "$(apiget "/api/runs/$RUN_C" | jq -r '.run.status')" = queued ] || fail "cancel-path run should start queued"
SS="$(apipost "/api/runs/$RUN_C/inputs" '{"kind":"cancel","body":""}' | jq -r '.server_side')"
[ "$SS" = true ] || fail "cancel of a queued run should be applied server-side (got server_side=$SS)"
[ "$(apiget "/api/runs/$RUN_C" | jq -r '.run.status')" = cancelled ] || fail "queued run did not transition to cancelled"
pass "queued run transitioned to cancelled server-side"

# --- issue the worker join token + bring the worker online -------------------
say "issue a worker join token and bring the worker online"
WTOKEN="$(apipost /api/workers '{"name":"e2e-worker"}' | jq -r '.token')"
[ -n "$WTOKEN" ] && [ "$WTOKEN" != null ] || fail "no worker token minted"
write_token
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "worker registered and is online"

# --- happy path with a mid-run restart ---------------------------------------
say "happy path: create a PRD issue and start a run"
IID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E implement","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN="$(apipost "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$IID}" | jq -r '.run.id')"
[ -n "$RUN" ] && [ "$RUN" != null ] || fail "run was not created"
pass "issue #$IID created; run $RUN queued"

wait_status "$RUN" awaiting_approval
[ "$(apiget "/api/runs/$RUN" | jq -r '.run.plan_md // empty')" != "" ] || fail "awaiting_approval carried no plan"
pass "run reached the plan gate (awaiting_approval) with a plan"

say "restart-resilience: down/up (keep volumes) while parked at the gate"
"${COMPOSE[@]}" down                       # keeps the named volumes (pgdata, agentdata)
write_token                                 # the worker unlinked its token file; restore it
"${COMPOSE[@]}" up -d --wait db api web forge-fake
wait_http
login
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "stack restarted; worker back online"

wait_status "$RUN" awaiting_approval
pass "orphaned run was re-queued, re-claimed, and is back at the gate"

say "approve the plan and let the run finish"
apipost "/api/runs/$RUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "run completed"

# =============================================================================
say "assertions"
FINAL="$(apiget "/api/runs/$RUN")"
[ "$(echo "$FINAL" | jq -r '.run.status')" = completed ] || fail "final status is not completed"
[ "$(echo "$FINAL" | jq -r '.run.branch')" = "agent/issue-$IID" ] || fail "run.branch is not agent/issue-$IID"
MR_IID="$(echo "$FINAL" | jq -r '.run.mr_iid')"
{ [ "$MR_IID" != null ] && [ "$MR_IID" -gt 0 ]; } || fail "run.mr_iid not set (got $MR_IID)"
pass "run row: completed, branch=agent/issue-$IID, mr_iid=$MR_IID"

git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID" \
  || fail "branch agent/issue-$IID was not pushed to the remote"
pass "branch agent/issue-$IID present on the git remote"

STATE_JSON="$("${COMPOSE[@]}" exec -T forge-fake cat /state/state.json)"
[ "$(echo "$STATE_JSON" | jq '.mrs | length')" -ge 1 ] || fail "fake GitLab recorded no merge request"
[ "$(echo "$STATE_JSON" | jq -r '.mrs[-1].source_branch')" = "agent/issue-$IID" ] \
  || fail "recorded MR source_branch mismatch"
[ "$(echo "$STATE_JSON" | jq -r '.mrs[-1].target_branch')" = "main" ] || fail "recorded MR target_branch is not main"
pass "fake GitLab recorded an MR from agent/issue-$IID into main"

# When the authenticated-remote variant ran, prove the git smart-HTTP endpoint
# actually GATES on the Basic credential (not a no-op that accepts anything): no
# credential must 401, the correct Basic header must 200. Probed from inside the
# agent, which resolves forge-fake.e2e and trusts its cert (NODE_EXTRA_CA_CERTS).
if [ -n "${E2E_GIT_SMART_HTTP:-}" ]; then
  refs_url="https://forge-fake.e2e/group/repo.git/info/refs?service=git-upload-pack"
  probe='const https=require("https");const u=new URL(process.argv[1]);const o={hostname:u.hostname,port:443,path:u.pathname+u.search,headers:{}};if(process.argv[2])o.headers.Authorization=process.argv[2];https.get(o,r=>{console.log(r.statusCode);r.resume();}).on("error",e=>{console.error(e.message);process.exit(2);});'
  auth="Basic $(printf 'uzi-bot:%s' "$DUMMY_FORGE_PAT" | base64 | tr -d '\r\n')"
  code_noauth="$("${COMPOSE[@]}" exec -T agent node -e "$probe" "$refs_url" | tr -d '\r\n')"
  [ "$code_noauth" = 401 ] || fail "git smart-HTTP without a credential should 401 (got '$code_noauth')"
  code_auth="$("${COMPOSE[@]}" exec -T agent node -e "$probe" "$refs_url" "$auth" | tr -d '\r\n')"
  [ "$code_auth" = 200 ] || fail "git smart-HTTP with the correct Basic credential should 200 (got '$code_auth')"
  pass "git smart-HTTP auth gate: no credential -> 401, correct Basic -> 200"
fi

MSGS="$(apiget "/api/runs/$RUN/messages")"
echo "$MSGS" | jq -e '.messages | (length > 0) and ([.[].seq] == [range(1; length+1)])' >/dev/null \
  || fail "run_messages seq is not a gapless 1..N sequence (across the restart)"
pass "run_messages seq is gapless 1..$(echo "$MSGS" | jq '.messages | length') across the restart"

say "secret-hygiene assertions"
LOGS="$("${COMPOSE[@]}" logs --no-color 2>&1 || true)"
for sec in "$WTOKEN" "$DUMMY_FORGE_PAT" "$DUMMY_ANTHROPIC"; do
  printf '%s' "$LOGS" | grep -qF "$sec" && fail "a secret leaked into container logs"
done
pass "no PAT / Anthropic token / join token in any container log"

for sec in "$WTOKEN" "$DUMMY_FORGE_PAT" "$DUMMY_ANTHROPIC"; do
  if "${COMPOSE[@]}" exec -T agent sh -c "grep -rlF '$sec' /data 2>/dev/null | head -1" | grep -q .; then
    fail "a secret is present on the worker's /data disk"
  fi
done
pass "no secret on the worker's /data (bare clone cache, worktrees, sessions)"

# /proc hardening (M6): the join token was delivered by file, not env, so it must
# not appear in ANY process's environ; and its delivery file must have been
# unlinked. The token is passed as argv (not env) so the probe can't self-match.
"${COMPOSE[@]}" exec -T agent sh -c 'test ! -e /worker-secret/token' \
  || fail "the worker-token file was not unlinked after read (/proc hardening)"
ENV_HITS="$("${COMPOSE[@]}" exec -T agent sh -c '
  n=0
  for e in /proc/[0-9]*/environ; do
    tr "\0" "\n" < "$e" 2>/dev/null | grep -qF "$1" && n=$((n+1))
  done
  echo "$n"' _ "$WTOKEN")"
[ "$ENV_HITS" = 0 ] || fail "the join token is present in $ENV_HITS process environ(s) — /proc leak NOT closed"
pass "/proc hardening: join token absent from every process environ; delivery file unlinked"

# =============================================================================
# PRD #24 — MR-close watcher: a reviewer closing an agent's MR without merging
# moves the card from Human Review back to In Progress; reopening restores it; a
# manual drag is never fought. The happy-path run above left card #$IID in Human
# Review with an open MR ($MR_IID) — exactly the watcher's precondition.
say "PRD #24: MR-close watcher (Human Review ⇄ In Progress on MR close/reopen)"

# The watcher only ticks inside the poller; the overlay default is 24h. Switch to
# ~2s and recreate the api so the MR-state watcher actually runs.
printf 'E2E_FORGE_POLL_INTERVAL=2s\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login
# The completed run's run-lifecycle move to Human Review is async; tolerate it
# settling (and confirm the fake retained the issue across the restart).
wait_card_column "$IID" "Human Review" 20
pass "poller sped to ~2s; card #$IID in Human Review with open MR !$MR_IID"

# NULL-bootstrap (Decision 9): the first tick records the MR's CURRENT state
# ('opened') WITHOUT moving, so a pre-existing state never triggers a spurious
# move. Give it a few ticks; the card must stay put.
sleep 6
[ "$(card_column "$IID")" = "Human Review" ] \
  || fail "NULL-bootstrap must record MR state without moving the card (Decision 9)"
pass "NULL-bootstrap recorded MR state without moving the card"

# Close edge: reviewer closes the MR without merging → rework → In Progress.
flip_mr "$MR_IID" closed
wait_card_column "$IID" "In Progress" 40
pass "MR closed unmerged → card #$IID moved Human Review → In Progress"

# Reopen edge: reopening the MR restores the card to Human Review, symmetrically.
flip_mr "$MR_IID" opened
wait_card_column "$IID" "Human Review" 40
pass "MR reopened → card #$IID returned In Progress → Human Review"

# Manual-drag pre-emption, exercising the Go source-column guard (not just the SQL
# prefilter): re-close so the card is In Progress AND still a watch candidate
# (mr_state='closed'), drag it to Later, then reopen. The reopen edge's guard sees
# the card is no longer in its expected source column (In Progress) and backs off —
# the human's placement wins.
flip_mr "$MR_IID" closed
wait_card_column "$IID" "In Progress" 40
apipost "/api/repos/$REPO_ID/issues/$IID/move" '{"to_column":"Later"}' >/dev/null
wait_card_column "$IID" "Later" 10   # the move is forge-first; let any in-flight reconcile settle
flip_mr "$MR_IID" opened
# Several ticks must pass with the card LEFT in Later (a fight would yank it to
# Human Review within one tick).
sleep 10
[ "$(card_column "$IID")" = "Later" ] \
  || fail "watcher fought a manual drag: card #$IID left Later after the MR reopened"
pass "manual drag wins: card #$IID stayed in Later despite the MR reopening"

# =============================================================================
# PRD #19 — admin settings + autopilot. The poller is already at ~2s (the phase
# above); make the reconcile cadence tight too so the FullSync-eviction dedup
# assertion has a bounded wait (a full reconcile every 2 ticks). Then map the
# repo owner's forge username and opt them into autopilot: the two consent gates
# an unattended run requires (Decision 4).
say "PRD #19 autopilot: tighten reconcile cadence, map + opt-in the repo owner"
printf 'FORGE_RECONCILE_EVERY=2\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login
wait_worker_online

CONN_ID="$(apiget /api/forge/connections | jq -r '.connections[0].id // empty')"
[ -n "$CONN_ID" ] || fail "no seeded forge connection to map"

# Carry-item (M3): human_username saves verified-or-warned. The fake knows no human
# accounts, so a real forge lookup runs and comes back empty → the value still saves,
# WITH a warning (never a hard reject). This is also the owner→username mapping the
# autopilot attribution needs.
WARN="$(apiput "/api/forge/connections/$CONN_ID" '{"human_username":"owner-alice"}' | jq -r '.warning // empty')"
[ -n "$WARN" ] || fail "human_username save should return a verified-or-warned warning"
[ "$(apiget /api/forge/connections | jq -r '.connections[0].human_username')" = owner-alice ] \
  || fail "human_username did not persist despite the warning"
pass "owner mapped to owner-alice (unverifiable → saved WITH a warning)"

[ "$(apiput /api/me/autopilot '{"enabled":true}' | jq -r '.user.autopilot_enabled')" = true ] \
  || fail "autopilot opt-in did not stick"
pass "repo owner opted into autopilot"

# --- autopilot #1: happy path ------------------------------------------------
say "autopilot #1: happy path — mapped+opted-in owner adds the label → unattended run → MR + success comment"
IID_AP="$(create_autopilot_issue "E2E autopilot happy" \
  "Implements prds/4-agent-runtime-workers.md under autopilot." owner-alice owner-alice)"
RUN_AP="$(wait_run_for_issue "$IID_AP" 40)"
[ "$(apiget "/api/runs/$RUN_AP" | jq -r '.run.auto_approve')" = true ] \
  || fail "autopilot run is not marked auto_approve"
pass "issue #$IID_AP: poller created an unattended auto_approve run ($RUN_AP) — no manual start"

wait_autopilot_done "$RUN_AP" completed 120
AP="$(apiget "/api/runs/$RUN_AP")"
# The plan is recorded as a run MESSAGE (kind:"plan"), emitted+flushed before the
# auto-approve verdict — NOT in run.plan_md, which only the awaiting_approval report
# sets and autopilot deliberately skips (so the run never parks at the gate).
apiget "/api/runs/$RUN_AP/messages" \
  | jq -e '[.messages[]? | select(.kind=="plan")] | length >= 1' >/dev/null \
  || fail "autopilot run recorded no plan message"
[ "$(echo "$AP" | jq -r '.run.branch')" = "agent/issue-$IID_AP" ] || fail "autopilot branch mismatch"
MR_AP="$(echo "$AP" | jq -r '.run.mr_iid')"
{ [ "$MR_AP" != null ] && [ "$MR_AP" -gt 0 ]; } || fail "autopilot run opened no MR (got $MR_AP)"
pass "run completed unattended (never parked at the gate), plan recorded, MR !$MR_AP opened"

wait_notes "$IID_AP" 1 40
notes_text "$IID_AP" | grep -qF "opened a merge request" || fail "expected the success comment with the MR link"
pass "exactly one success comment referencing the MR"

wait_card_column "$IID_AP" "Human Review" 40
pass "board label moved: card #$IID_AP resolved to Human Review"

# --- autopilot #2: no-consent (+ retry re-eval + eviction dedup) -------------
say "autopilot #2: no-consent — label added by an unmapped user → one explanatory comment, no run"
IID_NC="$(create_autopilot_issue "E2E autopilot no-consent" \
  "Implements prds/4-agent-runtime-workers.md" someone-else someone-else)"
wait_notes "$IID_NC" 1 40
notes_text "$IID_NC" | grep -qF "did not start a run" || fail "expected the no-eligible-user comment"
assert_no_run_for_issue "$IID_NC" 6
pass "one 'no eligible user' comment, no run"

# Retry gesture: remove + re-add mints a larger event id → re-evaluated exactly once.
add_label_event "$IID_NC" remove someone-else
add_label_event "$IID_NC" add someone-else
wait_notes "$IID_NC" 2 40
pass "label remove+re-add (new event id) → re-evaluated once → second comment"

# A FullSync (eviction + resync of the issue cache) must NOT re-comment: the dedup
# marker lives in autopilot_triggers, not the evictable issue cache. Several ticks
# (>= one reconcile at FORGE_RECONCILE_EVERY=2) with no new label event → still 2.
sleep 10
[ "$(note_count "$IID_NC")" = 2 ] || fail "a FullSync eviction re-commented (trigger dedup must survive eviction)"
assert_no_run_for_issue "$IID_NC" 0
pass "no re-comment (and still no run) across a FullSync eviction"

# --- autopilot #3: failure path ----------------------------------------------
say "autopilot #3: failure path — the run fails (stub sentinel) → exactly one failure comment"
IID_FL="$(create_autopilot_issue "E2E autopilot failure" \
  "Implements prds/4-agent-runtime-workers.md then fails: UZI_STUB_FAIL" owner-alice owner-alice)"
RUN_FL="$(wait_run_for_issue "$IID_FL" 40)"
wait_status "$RUN_FL" failed 120
wait_notes "$IID_FL" 1 40
notes_text "$IID_FL" | grep -qF "could not complete" || fail "expected the failure comment"
notes_text "$IID_FL" | grep -qF "/runs/$RUN_FL" || fail "failure comment is missing the run link"
sleep 6   # a duplicate would appear within a couple of ticks
[ "$(note_count "$IID_FL")" = 1 ] || fail "failure path posted more than one comment"
pass "exactly one failure comment (fixed template + run link), no failure_reason echoed"

# --- autopilot #5: PRD-link gate ---------------------------------------------
say "autopilot #5: PRD-link gate — autopilot label on an issue with no PRD link → comment, no run"
IID_NP="$(create_autopilot_issue "E2E autopilot no-prd" \
  "This issue points at no plan file whatsoever." owner-alice owner-alice)"
wait_notes "$IID_NP" 1 40
notes_text "$IID_NP" | grep -qF "no PRD link" || fail "expected the no-PRD-link comment"
assert_no_run_for_issue "$IID_NP" 6
pass "one 'no PRD link' comment, no run"

# --- autopilot #4: carry-item e2e (settings race + username collision) -------
say "carry-item: concurrent cross-key settings PUT — the FOR UPDATE serialization rejects the equal-label race"
# Two concurrent single-key PUTs that each pass the cache precheck but together would
# land prd_label == autopilot_label. Exactly one commits; the other is rejected (400),
# whether by the in-tx FOR UPDATE cross-key check (true race) or the cache precheck
# (if it lost the race). Same admin session, two concurrent requests = two txns.
( apiput_code /api/admin/settings '{"settings":{"prd_label":"SHARED"}}'       > "$RUNROOT/race.a" ) &
( apiput_code /api/admin/settings '{"settings":{"autopilot_label":"SHARED"}}' > "$RUNROOT/race.b" ) &
wait
CA="$(cat "$RUNROOT/race.a")"; CB="$(cat "$RUNROOT/race.b")"
ok=0; bad=0
for c in "$CA" "$CB"; do
  case "$c" in 200) ok=$((ok+1));; 400) bad=$((bad+1));; *) fail "unexpected settings PUT status: $c";; esac
done
{ [ "$ok" = 1 ] && [ "$bad" = 1 ]; } \
  || fail "concurrent cross-key PUT: expected one 200 + one 400, got $CA and $CB"
pass "concurrent cross-key settings PUT: exactly one accepted, one rejected (got $CA / $CB)"
# Restore the defaults so nothing downstream sees a half-applied label swap.
apiput /api/admin/settings '{"settings":{"prd_label":"PRD","autopilot_label":"autopilot"}}' >/dev/null

say "carry-item: human_username collision — a second user claiming the same forge username on the same host is 409"
JAR2="$RUNROOT/u2.jar"
# Open registration issues a session (first user is the seeded admin, so this one is
# a normal user). The cookie jar carries both the session and the CSRF token.
curl -fsS -c "$JAR2" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d '{"email":"user2@uzi.e2e","password":"e2e-user2-password-000000","display_name":"User Two"}' >/dev/null
u2csrf="$(awk '$6=="uzi_csrf"{print $7}' "$JAR2")"
CONN2="$(curl -fsS -b "$JAR2" -X POST "$BASE/api/forge/connections" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $u2csrf" \
  -d "{\"forge_type\":\"gitlab\",\"base_url\":\"https://forge-fake.e2e\",\"token\":\"$DUMMY_FORGE_PAT\"}" \
  | jq -r '.connection.id // empty')"
[ -n "$CONN2" ] || fail "user2 could not connect to the fake forge"
# The admin already mapped owner-alice above; user2 claiming it on the same host must 409.
COLLIDE="$(curl -sS -o /dev/null -w '%{http_code}' -b "$JAR2" -X PUT "$BASE/api/forge/connections/$CONN2" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $u2csrf" \
  -d '{"human_username":"owner-alice"}')"
[ "$COLLIDE" = 409 ] || fail "second user mapping owner-alice should be 409, got $COLLIDE"
pass "human_username collision on the same host is rejected (409)"

printf '\n\033[32mAll E2E checks passed.\033[0m (M6 runtime + PRD #24 MR-close + PRD #19 autopilot; executor=%s)\n' "$EXECUTOR"
