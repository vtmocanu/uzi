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
apipatch() { curl -fsS -b "$JAR" -X PATCH "$BASE$1" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(csrf)" -d "$2"; }
# fresh_code METHOD PATH [BODY] — a non-admin (fresh user) request; prints only the
# HTTP status (no -f), for authz assertions. CSRF from the fresh user's jar.
fresh_code() {
  local method="$1" p="$2" body="${3:-}" tok
  tok="$(awk '$6=="uzi_csrf"{print $7}' "$FRESHJAR")"
  if [ -n "$body" ]; then
    curl -sS -b "$FRESHJAR" -o /dev/null -w '%{http_code}' -X "$method" "$BASE$p" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $tok" -d "$body"
  else
    curl -sS -b "$FRESHJAR" -o /dev/null -w '%{http_code}' -X "$method" "$BASE$p" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $tok"
  fi
}
# apiput_code — like apiput but never -f: echoes the HTTP status and swallows the
# body, so the caller can assert on 200-vs-4xx (the concurrent-PUT race).
apiput_code() { curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" -X PUT "$BASE$1" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" -d "$2"; }
# apipost_code — like apipost but never -f: echoes only the HTTP status, for
# asserting a 422 (the PRDLESS run-create gate and the disabled label endpoint).
apipost_code() { curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" -X POST "$BASE$1" \
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

# wait_board_pipeline STATUS [TIMEOUT] — wait until the board header's default-branch
# CI badge reaches STATUS (the pipeline sync is poller-driven, PRD #6).
wait_board_pipeline() {
  local want="$1" deadline=$((SECONDS + ${2:-30})) s
  while [ $SECONDS -lt $deadline ]; do
    s="$(apiget "/api/repos/$REPO_ID/board" | jq -r '.board.pipeline.status // empty')"
    [ "$s" = "$want" ] && return 0
    sleep 2
  done
  fail "timeout: board pipeline never reached '$want' (last: ${s:-none})"
}

# wait_card_pipeline IID STATUS [TIMEOUT] — wait until the board CARD for issue IID
# shows STATUS on its per-card CI badge (the card's most-recent run branch, PRD #6).
wait_card_pipeline() {
  local iid="$1" want="$2" deadline=$((SECONDS + ${3:-30})) s
  while [ $SECONDS -lt $deadline ]; do
    s="$(apiget "/api/repos/$REPO_ID/board" | jq -r --argjson iid "$iid" '.board.cards[] | select(.iid==$iid) | .pipeline.status // empty')"
    [ "$s" = "$want" ] && return 0
    sleep 2
  done
  fail "timeout: card #$iid pipeline never reached '$want' (last: ${s:-none})"
}

# wait_verdict RUN WANT [TIMEOUT] — wait for a ci_fix run's fix_verdict (the
# verification stamp is poller-driven, PRD #6).
wait_verdict() {
  local run="$1" want="$2" deadline=$((SECONDS + ${3:-30})) v
  while [ $SECONDS -lt $deadline ]; do
    v="$(apiget "/api/runs/$run" | jq -r '.run.fix_verdict // empty')"
    [ "$v" = "$want" ] && return 0
    sleep 2
  done
  fail "timeout: run $run fix_verdict never reached '$want' (last: ${v:-none})"
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

# Stub `devbox` for the PRD #18 tool-provisioning scenario: the isolated stack has
# no substituter egress, so a real `devbox install` is neither possible nor wanted.
# This fake satisfies the worker's provision path — `install` is a no-op; `shellenv`
# prints one allowlisted PATH line filterShellenv keeps — so the provisioning wiring
# is exercised end to end, fast, with no network. Bind-mounted over the baked-in
# binary by the overlay; only invoked when a run has tier-1 packages.
cat > "$RUNROOT/fake-devbox" <<'EOF'
#!/bin/sh
case "$1" in
  install)  exit 0 ;;
  shellenv) printf 'export PATH="/nix/e2e-tools/bin:${PATH}"\n'; exit 0 ;;
  *)        exit 0 ;;
esac
EOF
chmod 0755 "$RUNROOT/fake-devbox"

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
# A repo-borne skill for the PRD #16 M6 opt-in path. Carries a capability key
# (allowed-tools) that MUST be stripped when the flag is on; the worker loads only
# name+description. It stays invisible unless the repo owner enables repo skills.
mkdir -p "$seedwc/.claude/skills/e2e-repo-skill"
printf -- '---\nname: e2e-repo-skill\ndescription: A repo-borne skill for the M6 opt-in E2E.\nallowed-tools: Bash, Write\n---\n\n# E2E repo skill\n\nProves the repo-skill opt-in path end to end.\n' \
  > "$seedwc/.claude/skills/e2e-repo-skill/SKILL.md"
git -C "$seedwc" add README.md .claude
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

# --- PRD #16: skills authz (HTTP-glue status codes, live) ---------------------
# The reviewer's M2 ask: pin the authz boundaries end to end, not just in unit
# tests. Uses the fresh non-admin user registered above (FRESHJAR).
say "PRD #16 skills authz: a non-admin cannot reach admin / other-user surfaces"
TID="$(apiget /api/agent-templates | jq -r '.templates[0].id // empty')"
[ -n "$TID" ] || fail "no agent template to authorize against"

C="$(fresh_code POST /api/skills '{"name":"e2e-nope","description":"x.","body":"b\n","scope":"global"}')"
[ "$C" = 403 ] || fail "non-admin POST /skills scope=global: expected 403, got $C"
pass "non-admin POST /skills scope=global ⇒ 403"

# The admin owns a private (user-scope) skill; a non-admin GET of it must 404
# (existence hidden), never 403.
PRIV_ID="$(apipost /api/skills '{"name":"e2e-admin-private","description":"x.","body":"b\n","scope":"user"}' | jq -r '.skill.id')"
[ -n "$PRIV_ID" ] && [ "$PRIV_ID" != null ] || fail "admin could not create a private skill"
C="$(fresh_code GET "/api/skills/$PRIV_ID")"
[ "$C" = 404 ] || fail "non-admin GET of another user's private skill: expected 404, got $C"
pass "non-admin GET of another user's private skill ⇒ 404"

C="$(fresh_code PUT "/api/agent-templates/$TID/skills" '{"shared_skill_ids":[]}')"
[ "$C" = 403 ] || fail "non-admin shared allocation: expected 403, got $C"
pass "non-admin PUT shared allocation half ⇒ 403"

C="$(fresh_code PATCH "/api/repos/$REPO_ID" '{"repo_skills_enabled":true}')"
[ "$C" = 404 ] || fail "non-owner repo PATCH: expected 404, got $C"
pass "non-owner non-admin PATCH /repos/{id} ⇒ 404"

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
# PRD #16 — skill delivery + repo-skill opt-in, end to end. The stub executor
# synthesizes the plugin dir the SAME way the SDK executor does (shared
# prepareSkillPlugin) and reports the skill dirs it materialized on disk, so
# delivery is observable without a live Anthropic session.
say "PRD #16: skill delivery (builtin allocated → claim → synthesized plugin dir)"

# Allocate the builtin ci-cd-norms (shared) to a template. Claim assembly unions
# every template's allocations for the run's user, so this reaches every run.
SKILL_CICD_ID="$(apiget /api/skills | jq -r '.skills[] | select(.name=="ci-cd-norms") | .id')"
[ -n "$SKILL_CICD_ID" ] && [ "$SKILL_CICD_ID" != null ] || fail "builtin ci-cd-norms skill was not seeded"
apiput "/api/agent-templates/$TID/skills" "{\"shared_skill_ids\":[\"$SKILL_CICD_ID\"]}" >/dev/null
pass "allocated builtin ci-cd-norms (shared) to template $TID"

# plugin_skills RUN — the flattened plugin_skills arrays the stub reported (exactly
# the skill dirs materialized under the synthesized plugin dir).
plugin_skills() { apiget "/api/runs/$1/messages" | jq -c '[.messages[].payload.plugin_skills // empty] | flatten'; }

# skill_run TITLE — new issue+run, park at the gate (the stub reports skills at run
# START, before the gate), approve, and drive to completed (freeing the worker).
skill_run() {
  local iid run
  iid="$(apipost "/api/repos/$REPO_ID/issues" "{\"title\":\"$1\",\"description\":\"skill e2e — prds/16-agent-skills.md\"}" | jq -r '.card.iid')"
  run="$(apipost "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$iid}" | jq -r '.run.id')"
  wait_status "$run" awaiting_approval
  echo "$run"
}

# Repo skills OFF (default): the delivered builtin is materialized; the repo skill
# (seeded in the clone's .claude/skills) is NOT.
RUN_S1="$(skill_run 'E2E skill delivery (repo off)')"
PS1="$(plugin_skills "$RUN_S1")"
echo "$PS1" | jq -e 'index("ci-cd-norms") != null' >/dev/null \
  || fail "delivered builtin absent from the synthesized plugin dir: $PS1"
echo "$PS1" | jq -e 'index("e2e-repo-skill") == null' >/dev/null \
  || fail "repo skill loaded while the opt-in flag is OFF: $PS1"
apipost "/api/runs/$RUN_S1/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_S1" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "flag OFF: plugin dir has ci-cd-norms, NOT the repo skill ($PS1)"

# Flip the repo-skills opt-in ON (repo owner = the seed admin) and confirm it stuck.
apipatch "/api/repos/$REPO_ID" '{"repo_skills_enabled":true}' | jq -e '.repo.repo_skills_enabled == true' >/dev/null \
  || fail "PATCH repo_skills_enabled=true did not stick"
pass "repo owner enabled repo skills"

# Repo skills ON: the repo skill now loads too, at lowest precedence, alongside the
# delivered builtin.
RUN_S2="$(skill_run 'E2E skill delivery (repo on)')"
PS2="$(plugin_skills "$RUN_S2")"
echo "$PS2" | jq -e '(index("ci-cd-norms") != null) and (index("e2e-repo-skill") != null)' >/dev/null \
  || fail "repo skill not loaded at lowest precedence after opt-in: $PS2"
apipost "/api/runs/$RUN_S2/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_S2" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "flag ON: plugin dir has BOTH ci-cd-norms and e2e-repo-skill ($PS2)"

# =============================================================================
# PRD #18 — agent template scopes/allocation + tier-1 tool provisioning, end to
# end. The stub executor reports the claim's delivered template set
# (payload.agents) and runs the SAME provisioning path as the SDK executor
# (against the stubbed devbox), so both are observable with no live Anthropic
# session and no substituter egress.
say "PRD #18: user-scoped template + allocation → claim delivers only the owner's set"

# delivered_agents RUN — the flattened, deduped agent names the stub reported.
delivered_agents() { apiget "/api/runs/$1/messages" | jq -c '[.messages[].payload.agents // empty] | flatten | unique'; }
# run_texts RUN — every status/text line, newline-joined (fixed-string greppable).
run_texts() { apiget "/api/runs/$1/messages" | jq -r '.messages[].payload.text // empty'; }

# Create a private (scope=user) template as the seed admin, then allocate it to the
# admin's own runs (my_overrides enabled). A uniquely-named user template rides the
# owner's claim; another user would never see it (proven at the SQL layer).
UT_ID="$(apipost /api/agent-templates '{"name":"e2e-mine","description":"a private e2e helper.","prompt_body":"You help with e2e things.\n","model":null,"tools":null,"scope":"user"}' | jq -r '.template.id')"
{ [ -n "$UT_ID" ] && [ "$UT_ID" != null ]; } || fail "user-scoped template create failed"
apiput /api/agent-templates/allocations "{\"my_overrides\":[{\"template_id\":\"$UT_ID\",\"enabled\":true}]}" >/dev/null
pass "created + allocated a user-scoped template (e2e-mine)"

# A reserved lead name is refused for a user template (Decision 8, the no-two-leads pin).
C="$(fresh_code POST /api/agent-templates '{"name":"orchestrator","description":"x.","prompt_body":"b\n","model":null,"tools":null,"scope":"user"}')"
[ "$C" = 400 ] || fail "reserved lead name (orchestrator) should be 400, got $C"
pass "reserved lead name refused for a user template (400)"

RUN_UT="$(skill_run 'E2E user-template delivery')"
DA="$(delivered_agents "$RUN_UT")"
echo "$DA" | jq -e 'index("e2e-mine") != null' >/dev/null \
  || fail "allocated user template absent from the delivered claim: $DA"
echo "$DA" | jq -e '(index("lead") != null) and (index("coder") != null)' >/dev/null \
  || fail "builtin lead/coder missing from the delivered claim: $DA"
apipost "/api/runs/$RUN_UT/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_UT" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "user template e2e-mine delivered to its owner's run alongside the builtins ($DA)"

say "PRD #18: tier-1 tool provisioning → claim carries tool_packages → worker provisions (devbox stubbed)"

# Set an allowlisted package as the repo's tier-1 tool profile (the M4 seed allowlist).
PKG="$(apiget /api/tool-allowlist | jq -r '.allowlist[0].name // empty')"
{ [ -n "$PKG" ] && [ "$PKG" != null ]; } || fail "tool allowlist was not seeded"
apiput "/api/repos/$REPO_ID/tool-profile" "{\"packages\":[\"$PKG\"]}" \
  | jq -e --arg p "$PKG" '.packages | index($p) != null' >/dev/null \
  || fail "repo tool profile did not save $PKG"
pass "set repo tier-1 tool profile: [$PKG]"

RUN_TP="$(skill_run 'E2E tool provisioning')"
# The claim carried tool_packages=[$PKG]; the worker provisions against the stubbed
# devbox (install no-op, shellenv one PATH line — no substituter egress).
TP_TEXTS="$(run_texts "$RUN_TP")"
echo "$TP_TEXTS" | grep -qF "provisioning 1 tool(s): $PKG" \
  || fail "claim did not carry tool_packages / provisioning not started for $PKG"
echo "$TP_TEXTS" | grep -qxF "tools provisioned" \
  || fail "provisioning path not exercised (no 'tools provisioned' message)"
apipost "/api/runs/$RUN_TP/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_TP" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "tier-1 tool [$PKG] provisioned against the stubbed devbox, run completed"
# Clear the profile so later scenarios' runs aren't perturbed by provisioning.
apiput "/api/repos/$REPO_ID/tool-profile" '{"packages":[]}' >/dev/null

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

# =============================================================================
# PRD #6 — CI status sync, Fix CI (plan-gated ci_fix run), and verification. The
# poller is already at ~2s (the MR-close phase sped it up), so the pipeline sync
# ticks fast; forge-fake serves the pipeline endpoints and the /_e2e/pipelines
# mutator seeds/flips a ref's status.
say "PRD #6: CI status sync + Fix CI + the verification stamp"

# 1) A red pipeline on main becomes visible on the board header within a tick.
fake_post "/_e2e/pipelines" '{"ref":"main","status":"failed","jobs":[{"name":"unit","stage":"test","status":"failed","trace":"=== RUN TestFoo\n--- FAIL: TestFoo (nil guard removed)\nFAIL\n"}]}' >/dev/null
wait_board_pipeline failed 20
pass "red main pipeline is visible on the board header within a poll interval"

# 2) Fix CI queues a plan-gated ci_fix run; a second one on the same ref is a 409.
FIXRUN="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}' | jq -r '.run.id')"
{ [ -n "$FIXRUN" ] && [ "$FIXRUN" != null ]; } || fail "ci_fix run was not created"
[ "$(apiget "/api/runs/$FIXRUN" | jq -r '.run.kind')" = ci_fix ] || fail "run kind is not ci_fix"
DUP="$(apipost_code "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}')"
[ "$DUP" = 409 ] || fail "a second active Fix CI on main should be 409, got $DUP"
pass "Fix CI queued ci_fix run $FIXRUN; a duplicate on the same ref is 409"

# 3) Plan gate → approve → the worker pushes the fix branch + opens an MR.
wait_status "$FIXRUN" awaiting_approval
[ "$(apiget "/api/runs/$FIXRUN" | jq -r '.run.plan_md // empty')" != "" ] || fail "ci_fix run carried no plan"
apipost "/api/runs/$FIXRUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$FIXRUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
FIXBRANCH="$(apiget "/api/runs/$FIXRUN" | jq -r '.run.branch')"
case "$FIXBRANCH" in ci-fix/pipeline-*) : ;; *) fail "ci_fix fix branch not ci-fix/pipeline-* (got $FIXBRANCH)";; esac
FIXMR="$(apiget "/api/runs/$FIXRUN" | jq -r '.run.mr_iid')"
{ [ "$FIXMR" != null ] && [ "$FIXMR" -gt 0 ]; } || fail "ci_fix run.mr_iid not set (got $FIXMR)"
[ "$(fake_state | jq -r --arg b "$FIXBRANCH" '[.mrs[] | select(.source_branch==$b)] | length')" -ge 1 ] \
  || fail "fake GitLab recorded no MR from $FIXBRANCH"
pass "ci_fix completed on $FIXBRANCH with MR !$FIXMR (default-branch fix)"

# 4) uzi verifies its work: the fix branch's post-fix pipeline passes → verdict.
fake_post "/_e2e/pipelines" "{\"ref\":\"$FIXBRANCH\",\"status\":\"success\"}" >/dev/null
wait_verdict "$FIXRUN" verified 20
pass "fix branch pipeline passed → run $FIXRUN stamped verified"

# 5) not_code path: a red main whose log says it is not a code problem. Flip main
#    GREEN first so the following red is unambiguously the NEW, sentinel-bearing
#    pipeline the sync must cache (a stale "still failed" would let the ci_fix
#    snapshot the previous, clean pipeline).
fake_post "/_e2e/pipelines" '{"ref":"main","status":"success"}' >/dev/null
wait_board_pipeline success 20
fake_post "/_e2e/pipelines" '{"ref":"main","status":"failed","jobs":[{"name":"deploy","stage":"deploy","status":"failed","trace":"runner disk full UZI_STUB_NOT_CODE"}]}' >/dev/null
wait_board_pipeline failed 20
NCRUN="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}' | jq -r '.run.id')"
wait_status "$NCRUN" awaiting_approval
apipost "/api/runs/$NCRUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$NCRUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
[ "$(apiget "/api/runs/$NCRUN" | jq -r '.run.fix_verdict')" = not_code ] || fail "not_code run verdict is not not_code"
[ "$(apiget "/api/runs/$NCRUN" | jq -r '.run.mr_iid')" = null ] || fail "a not_code run must open no MR"
pass "not_code path: run $NCRUN completed with fix_verdict=not_code and no MR"

# 6) Agent-MR fix + cross-kind race. An issue run leaves an open MR on
#    agent/issue-N; a red pipeline on that MR gets a ci_fix run whose commits land
#    on the SAME branch (the existing MR updates, no second MR) and whose
#    verification stamps on it. While that ci_fix is active, an issue run on the
#    same issue is refused (they would share the worktree).
say "PRD #6: agent-MR same-branch fix + cross-kind race"

AIID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E agent-MR fix","description":"implements prds/6-ci-status-integration.md"}' | jq -r '.card.iid')"
ARUN="$(apipost "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$AIID}" | jq -r '.run.id')"
wait_status "$ARUN" awaiting_approval
apipost "/api/runs/$ARUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$ARUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
AGENTBRANCH="agent/issue-$AIID"
AMR="$(apiget "/api/runs/$ARUN" | jq -r '.run.mr_iid')"
{ [ "$AMR" != null ] && [ "$AMR" -gt 0 ]; } || fail "issue run left no MR on $AGENTBRANCH (got $AMR)"
pass "issue run left an open MR !$AMR on $AGENTBRANCH"

# Red pipeline on the agent branch's MR (LatestMRPipeline resolves MR->source_branch).
fake_post "/_e2e/pipelines" "{\"ref\":\"$AGENTBRANCH\",\"status\":\"failed\",\"jobs\":[{\"name\":\"unit\",\"stage\":\"test\",\"status\":\"failed\",\"trace\":\"--- FAIL: TestBar\\n\"}]}" >/dev/null
wait_card_pipeline "$AIID" failed 20
pass "red pipeline on $AGENTBRANCH is visible on card #$AIID"

AFIX="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" "{\"ref\":\"$AGENTBRANCH\"}" | jq -r '.run.id')"
wait_status "$AFIX" awaiting_approval

# Cross-kind race: an issue run on the SAME issue must 409 while the ci_fix holds
# the agent branch/worktree.
RACE="$(apipost_code "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$AIID}")"
[ "$RACE" = 409 ] || fail "issue run on #$AIID while a ci_fix holds $AGENTBRANCH must 409, got $RACE"
pass "cross-kind race: an issue run on #$AIID is refused (409) while a ci_fix holds $AGENTBRANCH"

MRS_BEFORE="$(fake_state | jq --arg b "$AGENTBRANCH" '[.mrs[] | select(.source_branch==$b)] | length')"
apipost "/api/runs/$AFIX/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$AFIX" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
[ "$(apiget "/api/runs/$AFIX" | jq -r '.run.branch')" = "$AGENTBRANCH" ] \
  || fail "agent-branch ci_fix did not land on $AGENTBRANCH"
[ "$(apiget "/api/runs/$AFIX" | jq -r '.run.mr_iid')" = "$AMR" ] \
  || fail "agent-branch ci_fix must reuse the existing MR !$AMR, no second MR"
MRS_AFTER="$(fake_state | jq --arg b "$AGENTBRANCH" '[.mrs[] | select(.source_branch==$b)] | length')"
[ "$MRS_AFTER" = "$MRS_BEFORE" ] || fail "agent-branch ci_fix opened a SECOND MR ($MRS_BEFORE -> $MRS_AFTER)"
pass "agent-branch ci_fix landed on $AGENTBRANCH, reused MR !$AMR, opened no second MR"

# Verification stamps on the agent branch too (the fix pipeline outranks the failure).
fake_post "/_e2e/pipelines" "{\"ref\":\"$AGENTBRANCH\",\"status\":\"success\"}" >/dev/null
wait_verdict "$AFIX" verified 20
pass "agent-branch fix pipeline passed -> run $AFIX stamped verified"

# =============================================================================
# PRD #22 — PRDLESS escape hatch: an issue carrying the PRDLESS label runs with no
# prds/*.md link, gated by the admin toggle; the label is applied/removed from
# uzi's own UI (forge-first) via POST .../prdless. Exercises the run-create gate
# (422 when disabled, run when enabled) and the toggle endpoint (apply/remove on
# the fake forge + 422 when the feature is off).
say "PRD #22: PRDLESS escape hatch (gate bypass + UI label toggle)"

# Stage a PRD+PRDLESS issue with NO prds/*.md link (a human labelling it for the
# escape hatch — uzi's own CreateIssue only stamps the PRD label), then FullSync so
# uzi caches it (has_prd_link=false). The fresh GetIssue the run-create path reads
# returns the PRDLESS label from the fake, which is what the bypass decides on.
IID_PL="$(fake_post /_e2e/issues \
  "$(jq -nc '{title:"E2E prdless run",description:"tiny fix, no plan file here",labels:["PRD","PRDLESS"]}')" | jq -r '.iid')"
[ -n "$IID_PL" ] && [ "$IID_PL" != null ] || fail "could not stage the PRDLESS issue on the fake"
apipost "/api/repos/$REPO_ID/sync" '' >/dev/null
apiget "/api/repos/$REPO_ID/board" | jq -e --argjson iid "$IID_PL" \
  '.board.cards[] | select(.iid==$iid) | .has_prd_link==false' >/dev/null \
  || fail "staged PRDLESS issue #$IID_PL not cached with has_prd_link=false"
pass "staged PRD+PRDLESS issue #$IID_PL (no PRD link), cached"

# A uzi-created issue for the toggle path — starts with only the PRD label.
IID_TG="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E prdless toggle","description":"no plan file here either"}' | jq -r '.card.iid')"
[ -n "$IID_TG" ] && [ "$IID_TG" != null ] || fail "could not create the toggle issue"

# --- feature OFF: the label bypasses nothing, and the endpoint refuses ---------
apiput /api/admin/settings '{"settings":{"prdless_enabled":"false"}}' >/dev/null
C="$(apipost_code "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$IID_PL}")"
[ "$C" = 422 ] || fail "prdless disabled: run-create on the labelled no-PRD issue should 422, got $C"
pass "feature off: run-create on the PRDLESS-labelled no-PRD issue → 422"

C="$(apipost_code "/api/repos/$REPO_ID/issues/$IID_TG/prdless" '{"apply":true}')"
[ "$C" = 422 ] || fail "prdless disabled: the label endpoint should 422, got $C"
pass "feature off: POST .../prdless → 422"

# --- feature ON: the label bypasses the gate; the endpoint applies/removes ------
apiput /api/admin/settings '{"settings":{"prdless_enabled":"true"}}' >/dev/null

RUN_PL="$(apipost "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$IID_PL}" | jq -r '.run.id')"
[ -n "$RUN_PL" ] && [ "$RUN_PL" != null ] || fail "prdless-enabled run was not created (gate bypass failed)"
wait_status "$RUN_PL" awaiting_approval
pass "feature on: run $RUN_PL started with no PRD link and reached the plan gate"
apipost "/api/runs/$RUN_PL/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_PL" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
[ "$(apiget "/api/runs/$RUN_PL" | jq -r '.run.branch')" = "agent/issue-$IID_PL" ] \
  || fail "PRDLESS run did not push agent/issue-$IID_PL"
MR_PL="$(apiget "/api/runs/$RUN_PL" | jq -r '.run.mr_iid')"
{ [ "$MR_PL" != null ] && [ "$MR_PL" -gt 0 ]; } || fail "PRDLESS run opened no MR (got $MR_PL)"
pass "PRDLESS run completed the normal lifecycle (branch agent/issue-$IID_PL, MR !$MR_PL)"

# UI toggle apply: the label lands on the fake forge and the returned card reflects it.
CARD="$(apipost "/api/repos/$REPO_ID/issues/$IID_TG/prdless" '{"apply":true}')"
echo "$CARD" | jq -e '.card.labels | index("PRDLESS") != null' >/dev/null \
  || fail "apply: returned card labels missing PRDLESS: $(echo "$CARD" | jq -c '.card.labels')"
fake_state | jq -e --argjson iid "$IID_TG" \
  '.issues[] | select(.iid==$iid) | .labels | index("PRDLESS") != null' >/dev/null \
  || fail "apply: PRDLESS label not written to the fake forge issue #$IID_TG"
pass "toggle apply: PRDLESS on the fake forge + reflected in the card"

# UI toggle remove: the label is gone from the fake forge and the card.
CARD="$(apipost "/api/repos/$REPO_ID/issues/$IID_TG/prdless" '{"apply":false}')"
echo "$CARD" | jq -e '.card.labels | index("PRDLESS") == null' >/dev/null \
  || fail "remove: returned card still carries PRDLESS"
fake_state | jq -e --argjson iid "$IID_TG" \
  '.issues[] | select(.iid==$iid) | .labels | index("PRDLESS") == null' >/dev/null \
  || fail "remove: PRDLESS label still on the fake forge issue #$IID_TG"
pass "toggle remove: PRDLESS gone from the fake forge + the card"

# =============================================================================
# PRD #32 — per-user vault (password-wrapped secrets). Proves: the seeded token is
# DEK-sealed at boot; saving through the handler writes 'dek'; a locked owner's
# runs stay queued (never claimed, never failed) and claim after unlock; an API
# restart boot-unlocks the seed admin while a normal user stays locked (JWT
# survives, the in-memory DEK cache does not); lazy rewrap flips a legacy
# 'master' row to 'dek' on unlock; and the admin migration count reflects it.
say "PRD #32: per-user vault (dek sealing, claim gating, restart lock, lazy rewrap)"
login   # fresh admin session; login also unlocks the admin's vault

# --- vault helpers -----------------------------------------------------------
PGPW="$(grep '^POSTGRES_PASSWORD=' "$ENVFILE" | cut -d= -f2-)"
# db_psql SQL — a bare scalar out of the e2e db (PGPASSWORD from the env-file).
db_psql() { "${COMPOSE[@]}" exec -T -e PGPASSWORD="$PGPW" db psql -U uzi -d uzi -tAc "$1" | tr -d '\r\n'; }
# sealed_of EMAIL — the sealed_with of a user's anthropic_token row.
sealed_of() { db_psql "SELECT s.sealed_with FROM user_secrets s JOIN users u ON u.id = s.user_id WHERE u.email = '$1' AND s.kind = 'anthropic_token'"; }
# vault_status_jar JAR — GET /api/vault/status with JAR's cookie; a read that never unlocks.
vault_status_jar() { curl -fsS -b "$1" "$BASE/api/vault/status" | jq -r '.unlocked'; }
# master_seal_hex TEXT — AES-256-GCM seal a plaintext under UZI_SECRET_KEY, matching
# secretbox's nonce||ciphertext||tag layout, so the Go master box can open it. Run in
# forge-fake (a node image) with the key passed via env, not argv.
master_seal_hex() {
  "${COMPOSE[@]}" exec -T -e SK="$SECRET_KEY_B64" forge-fake node -e '
    const crypto = require("crypto");
    const key = Buffer.from(process.env.SK, "base64");
    const nonce = crypto.randomBytes(12);
    const c = crypto.createCipheriv("aes-256-gcm", key, nonce);
    const ct = Buffer.concat([c.update(Buffer.from(process.argv[1])), c.final()]);
    process.stdout.write(Buffer.concat([nonce, ct, c.getAuthTag()]).toString("hex"));
  ' "$1" | tr -d '\r\n'
}

# 1) The seeded token is DEK-sealed at boot; /api/me reports the vault unlocked.
[ "$(sealed_of admin@uzi.e2e)" = dek ] || fail "seeded admin token is not sealed_with='dek'"
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = true ] || fail "admin /api/auth/me vault.unlocked should be true after login"
pass "seeded admin token sealed_with='dek'; /api/me reports the vault unlocked"

# 2) Saving through the handler stores a 'dek' row.
apiput /api/me/secrets/anthropic_token "{\"token\":\"$DUMMY_ANTHROPIC\"}" >/dev/null
[ "$(sealed_of admin@uzi.e2e)" = dek ] || fail "handler save did not write sealed_with='dek'"
pass "PUT /me/secrets/anthropic_token stored sealed_with='dek'"

# 3) Lock → a new run stays queued (the worker gate withholds it) → unlock → it claims.
apipost /api/vault/lock '' >/dev/null
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = false ] || fail "vault should report locked after POST /api/vault/lock"
IID_V="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E vault gated","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_V="$(apipost "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$IID_V}" | jq -r '.run.id')"
sleep 8   # several worker poll cycles (2s each) must pass with the run LEFT queued
[ "$(apiget "/api/runs/$RUN_V" | jq -r '.run.status')" = queued ] \
  || fail "a locked owner's run must stay queued (never claimed, never failed)"
pass "vault locked: run $RUN_V stayed queued across several poll cycles"

apipost /api/vault/unlock "{\"password\":\"$ADMIN_PASS\"}" >/dev/null
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = true ] || fail "vault should report unlocked after POST /api/vault/unlock"
wait_status "$RUN_V" awaiting_approval 30
pass "after unlock, run $RUN_V claimed and reached the plan gate within a poll cycle"
apipost "/api/runs/$RUN_V/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_V" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "the unlocked run $RUN_V completed (worker freed)"

# 4) Stage a legacy master-sealed row for a NON-seed user (user2, registered
# earlier) and confirm the admin migration count sees it. Re-login user2 first so
# its session + CSRF are fresh enough to survive the api restart below.
curl -fsS -c "$JAR2" -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"user2@uzi.e2e","password":"e2e-user2-password-000000"}' >/dev/null
U2ID="$(db_psql "SELECT id FROM users WHERE email = 'user2@uzi.e2e'")"
[ -n "$U2ID" ] || fail "user2 not found in the db"
# Seal the legacy row with the api's ACTUAL master key: Compose ranks the
# developer's shell UZI_SECRET_KEY above --env-file (CLAUDE.md), so the key inside
# the container may differ from the env-file — read it from the running api so the
# staged ciphertext is one the api's master box can actually open (else rewrap
# would skip it as undecryptable). The api is distroless, so read via inspect.
SECRET_KEY_B64="$(docker inspect "$("${COMPOSE[@]}" ps -q api)" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^UZI_SECRET_KEY=//p' | head -1)"
[ -n "$SECRET_KEY_B64" ] || fail "could not read the api container's UZI_SECRET_KEY"
LEGACY_HEX="$(master_seal_hex 'sk-ant-e2e-legacy-master-000000')"
[ -n "$LEGACY_HEX" ] || fail "could not master-seal a legacy ciphertext"
db_psql "INSERT INTO user_secrets (user_id, kind, ciphertext, sealed_with)
         VALUES ('$U2ID', 'anthropic_token', decode('$LEGACY_HEX','hex'), 'master')
         ON CONFLICT (user_id, kind) DO UPDATE SET ciphertext = decode('$LEGACY_HEX','hex'), sealed_with = 'master'" >/dev/null
[ "$(sealed_of user2@uzi.e2e)" = master ] || fail "could not stage a master-sealed row for user2"
[ "$(apiget /api/admin/vault-migration | jq -r '.master_sealed')" -ge 1 ] \
  || fail "admin migration count did not see the master-sealed row"
pass "staged a legacy master-sealed row for user2; admin migration count >= 1"

# 5) Restart the api (recreate the process → the in-memory DEK cache is gone): the
# seed admin is boot-unlocked, a normal user is not. Both JWTs survive the restart,
# so the difference is purely who the boot path re-unlocks.
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
[ "$(vault_status_jar "$JAR")"  = true  ] || fail "the seed admin must be boot-unlocked after a restart (no interactive login)"
[ "$(vault_status_jar "$JAR2")" = false ] || fail "a normal user's vault must be LOCKED after a restart (JWT survives, DEK cache does not)"
pass "after the api restart: seed admin boot-unlocked (true); user2 locked (false)"

# 6) user2 unlocks (no re-login) → lazy rewrap flips their legacy row to 'dek', and
# the admin migration count drops back to zero.
u2csrf="$(awk '$6=="uzi_csrf"{print $7}' "$JAR2")"
curl -fsS -b "$JAR2" -X POST "$BASE/api/vault/unlock" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $u2csrf" -d '{"password":"e2e-user2-password-000000"}' >/dev/null \
  || fail "user2 unlock (no re-login) failed"
[ "$(vault_status_jar "$JAR2")" = true ] || fail "user2 vault should be unlocked after POST /api/vault/unlock"
[ "$(sealed_of user2@uzi.e2e)" = dek ] || fail "lazy rewrap did not flip user2's row 'master' -> 'dek' on unlock"
[ "$(apiget /api/admin/vault-migration | jq -r '.master_sealed')" = 0 ] \
  || fail "admin migration count should be 0 after the last master row rewrapped"
pass "lazy rewrap on unlock: user2's row flipped 'master' -> 'dek'; admin count back to 0"

printf '\n\033[32mAll E2E checks passed.\033[0m (M6 runtime + PRD #24 MR-close + PRD #16 skills + PRD #18 templates/tools + PRD #19 autopilot + PRD #6 CI-fix + PRD #22 PRDLESS + PRD #32 vault; executor=%s)\n' "$EXECUTOR"
