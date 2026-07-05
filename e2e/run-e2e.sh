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

# =============================================================================
say "provisioning scratch dir $RUNROOT (project $PROJECT, web $BASE, executor $EXECUTOR)"
mkdir -p "$RUNROOT/certs" "$RUNROOT/worker-secret" "$RUNROOT/agent-gitconfig" "$RUNROOT/fakeremote"

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

# --- PRD #5: least-privilege journey (steps 3-4) -----------------------------
# The forge base the seed + SSRF allowlist use (docker-compose.e2e.yml).
FORGE_BASE="https://forge-fake.e2e"
say "PRD #5 privilege checks: over-privileged connect is rejected + stored nothing; compliant connection is least-privilege"

# Step 3: a fresh, non-admin user (registration is open) connecting an
# OVER-privileged PAT is rejected with 422 + the violation list, and NOTHING is
# stored. A fresh user isolates the "no forge_connections row afterward"
# invariant from the admin's seeded connection.
FRESHJAR="$RUNROOT/fresh.jar"
FRESH_EMAIL="contractor@uzi.e2e"
FRESH_PASS="e2e-fresh-user-password-000"
curl -fsS -c "$FRESHJAR" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$FRESH_EMAIL\",\"password\":\"$FRESH_PASS\"}" >/dev/null \
  || fail "fresh user could not register (is registration open in the e2e overlay?)"

# POST the over-privileged PAT, capturing status + body (no -f: 422 is expected).
OVER_BODY="$RUNROOT/overpriv.json"
OVER_CODE="$(curl -sS -b "$FRESHJAR" -o "$OVER_BODY" -w '%{http_code}' \
  -X POST "$BASE/api/forge/connections" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(awk '$6=="uzi_csrf"{print $7}' "$FRESHJAR")" \
  -d "{\"base_url\":\"$FORGE_BASE\",\"token\":\"e2e-overprivileged-pat-000000\"}")"
[ "$OVER_CODE" = 422 ] || fail "over-privileged connect: expected 422, got $OVER_CODE (body: $(cat "$OVER_BODY"))"
jq -e '.violations | length > 0' "$OVER_BODY" >/dev/null 2>&1 \
  || fail "422 body missing a violations list: $(cat "$OVER_BODY")"
pass "over-privileged PAT rejected with 422 + violations"

# Nothing stored on rejection: the fresh user has zero connections.
FRESH_CONNS="$(curl -fsS -b "$FRESHJAR" "$BASE/api/forge/connections" | jq '.connections | length')"
[ "$FRESH_CONNS" = 0 ] || fail "422 rejection must store nothing; found $FRESH_CONNS connection(s) for the rejected user"
pass "nothing stored on rejection (0 connections for the rejected user)"

# Step 4: the admin's seeded, compliant connection reports least-privilege on an
# on-demand check (api-only PAT, Developer on a protected non-Developer-pushable main).
CONN_ID="$(apiget /api/forge/connections | jq -r '.connections[0].id // empty')"
[ -n "$CONN_ID" ] || fail "no seeded forge connection for the admin"
PRIV_REPORT="$(apipost "/api/forge/connections/$CONN_ID/privilege-check" '')"
echo "$PRIV_REPORT" | jq -e '.report.status == "ok"' >/dev/null 2>&1 \
  || fail "compliant connection privilege-check not ok: $PRIV_REPORT"
pass "compliant connection reports least-privilege ✓"

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

STATE_JSON="$("${COMPOSE[@]}" exec -T forge-fake cat /tmp/forge-fake-state.json)"
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

printf '\n\033[32mAll M6 E2E checks passed.\033[0m (executor=%s)\n' "$EXECUTOR"
