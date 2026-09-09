#!/usr/bin/env bash
# PRD #1171 m6 (items 8-9) — host orchestrator for the PACKAGED DARK CodexExecutor lifecycle proof.
#
# It is the m3b sibling of e2e/codex-m3a/run-lifecycle.sh. It builds the REAL worker image (with
# the m5 supervisor+fileop install) and runs the lifecycle suite INSIDE that image through the REAL
# root entrypoint, now driven against a REAL codex worker-route API test server on an INTERNAL,
# no-egress docker network:
#
#   1. PACKAGING controls (controls.sh) through the real root entrypoint under the writable-root
#      posture (unchanged), so the on-disk supervisor+fileop ownership it proves is production-true.
#      Runs under `--network none`.
#   2. A throwaway migrated Postgres container + the api/cmd/codexm3btestserver container, both on a
#      `docker network create --internal` network (no external egress), so the packaged executor's
#      REAL WorkerClient can hit the Bearer codex/release + codex/refresh routes over real HTTP.
#   3. The LIFECYCLE suite (lifecycle.test.ts) under the PRD CONFINEMENT posture — read-only root,
#      tmpfs runtime mounts, cap-drop, no-new-privileges — but on the SAME internal network instead
#      of `--network none` (the loopback fake OpenAI provider lives INSIDE the worker container; the
#      internal network reaches only the api+PG containers, never the internet). CODEX_M3B_PACKAGED=1
#      turns on Block B, and the server-contract env points the real WorkerClient at the api
#      container. The whole docker run is bounded by an OUTER `timeout --kill-after` watchdog.
#
# It then parses BOTH the Block-A `CODEX_M3B_COUNTS` summary and the Block-B
# `CODEX_M3B_PACKAGED_REAL_COUNTS` / `CODEX_M3B_PACKAGED_REAL_APIKEY_COUNTS` summaries and requires
# every real-path count > 0 (and the api_key refresh count == 0).
#
# ISOLATION (CLAUDE.md destructive-ops rules): every throwaway resource is named OUTSIDE the uzi-
# namespace and torn down by its EXACT name in a `trap cleanup EXIT` — never a uzi-* glob, never
# `docker compose down`, never `-p uzi`, never `-v`.
#
# CI/MAINTAINER-ONLY: in-worker image builds are storage-flaky and arm64 is blocked, so this is
# never run in-worker — the host `tsc` typecheck and the host `node --test` Block A run are.
#
#   ./run-lifecycle.sh                 build (unless skipped) + run the legs
#
# Environment:
#   UZI_M3B_IMAGE        worker image tag to build/run     (default: uzi-agent-m3b:base)
#   UZI_M3B_DOCKERFILE   worker Dockerfile to build        (default: agent/templates/base/Dockerfile)
#   UZI_M3B_SKIP_BUILD   set to 1 to reuse an existing worker image (skip docker build)
#   UZI_M3B_TEST_TIMEOUT outer watchdog seconds            (default: 600)
#   CDR_M3B_PG_IMAGE     throwaway Postgres image          (default: postgres:17)
#   CDR_M3B_API_PORT     port the api container binds/advertises (default: 8080)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${UZI_M3B_IMAGE:-uzi-agent-m3b:base}"
DOCKERFILE="${UZI_M3B_DOCKERFILE:-agent/templates/base/Dockerfile}"
TIMEOUT="${UZI_M3B_TEST_TIMEOUT:-600}"
PG_IMAGE="${CDR_M3B_PG_IMAGE:-postgres:17}"
API_PORT="${CDR_M3B_API_PORT:-8080}"

# Every throwaway resource carries the PID suffix and is OUTSIDE the uzi- namespace.
CONTROLS_NAME="codex-m3b-controls-$$"
LIFECYCLE_NAME="codex-m3b-lifecycle-$$"
NET="cdr-m3b-net-$$"
PG_NAME="cdr-m3b-pg-$$"
API_NAME="cdr-m3b-api-$$"
API_IMAGE="cdr-m3b-api-img-$$:local"
LIFECYCLE_OUT="$HERE/lifecycle-run.$$.log"

log() { printf '\n### %s\n' "$*"; }

if [ "$(uname -m)" != "x86_64" ]; then
  log "FAILURE: packaged lifecycle requires a native x86_64 host"
  exit 1
fi

require_linux_amd64_image() {
  _image="$1"
  _platform="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$_image" 2>/dev/null || true)"
  if [ "$_platform" != "linux/amd64" ]; then
    log "FAILURE: image $_image platform is ${_platform:-unknown}, want linux/amd64"
    exit 1
  fi
  log "image $_image platform verified: $_platform"
}

# cleanup removes EXACTLY the resources this run created, by exact name only. Idempotent and safe
# on any partial start. Never a uzi-* glob, never `docker compose down`, never `-p uzi`, never -v.
cleanup() {
  docker rm -f "$LIFECYCLE_NAME" "$CONTROLS_NAME" "$API_NAME" "$PG_NAME" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm -f "$API_IMAGE" >/dev/null 2>&1 || true
  rm -f "$LIFECYCLE_OUT" 2>/dev/null || true
}
trap cleanup EXIT

# 1. Build the real worker image (unless reusing one).
if [ "${UZI_M3B_SKIP_BUILD:-}" = "1" ]; then
  log "UZI_M3B_SKIP_BUILD=1 — reusing existing worker image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi
require_linux_amd64_image "$IMAGE"

# 2. Packaging controls — writable-root posture, ROOT start via the real entrypoint. Proves the
#    baked supervisor + fileop ownership/mode. Independent of the api server, so `--network none`.
log "running packaging controls in $IMAGE (writable-root posture, real entrypoint, --network none)"
set +e
timeout --kill-after=30s 180 docker run --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -v "$REPO/e2e":/work/e2e:ro \
  --name "$CONTROLS_NAME" \
  "$IMAGE" /bin/sh /work/e2e/codex-m3b/controls.sh
rc_controls=$?
set -e
docker rm -f "$CONTROLS_NAME" >/dev/null 2>&1 || true
log "packaging controls rc=$rc_controls"

# 3. The internal (no external egress) network the PG + api + worker containers share.
log "creating internal network $NET (no external egress)"
docker network create --internal "$NET" >/dev/null

# 4. Throwaway Postgres on the internal network. The api server migrates it (store.Migrate) on boot.
PGPASS="$(openssl rand -hex 16)"
DSN="postgres://uzi:${PGPASS}@${PG_NAME}:5432/uzi?sslmode=disable"
if ! docker image inspect "$PG_IMAGE" >/dev/null 2>&1; then
  log "pulling $PG_IMAGE (not present locally)"
  docker pull "$PG_IMAGE"
fi
log "starting throwaway Postgres $PG_NAME on $NET"
docker run -d --name "$PG_NAME" --network "$NET" \
  -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD="$PGPASS" \
  "$PG_IMAGE" >/dev/null

log "waiting for Postgres to accept connections (up to 120s)"
pg_ready=0
for _ in $(seq 1 120); do
  if docker exec "$PG_NAME" pg_isready -U uzi -d uzi >/dev/null 2>&1; then
    pg_ready=1
    break
  fi
  sleep 1
done
if [ "$pg_ready" -ne 1 ]; then
  log "INFRASTRUCTURE FAILURE: throwaway Postgres never became ready"
  docker logs "$PG_NAME" 2>&1 | tail -20 || true
  exit 1
fi

# 5. Build + start the test-server container on the internal network. It binds 0.0.0.0:API_PORT and
#    advertises the container-name origin OTHER containers dial, then prints ONE JSON contract line
#    on stdout (all diagnostics go to stderr), which we read back from its logs.
ADVERTISE="http://${API_NAME}:${API_PORT}"
log "building test-server image $API_IMAGE from e2e/codex-m3b/testserver.Dockerfile"
DOCKER_BUILDKIT=1 docker build -f "$HERE/testserver.Dockerfile" -t "$API_IMAGE" "$REPO/api"
require_linux_amd64_image "$API_IMAGE"
log "starting test server $API_NAME on $NET (bind 0.0.0.0:$API_PORT, advertise $ADVERTISE)"
docker run -d --name "$API_NAME" --network "$NET" \
  -e "UZI_TEST_DATABASE_URL=$DSN" \
  -e "CODEX_M3B_LISTEN=0.0.0.0:${API_PORT}" \
  -e "CODEX_M3B_ADVERTISE_URL=$ADVERTISE" \
  "$API_IMAGE" >/dev/null

log "waiting for the test server to migrate+seed and print its JSON contract"
contract=""
for _ in $(seq 1 120); do
  # The server prints exactly one stdout line (the JSON contract) starting with {"base_url":...;
  # all diagnostics go to stderr. Match the fixed substring (host grep is ugrep — avoid a regex
  # anchor with a bare `{`, which its POSIX modes can mishandle).
  contract="$(docker logs "$API_NAME" 2>/dev/null | grep -m1 -F '"base_url"' || true)"
  if [ -n "$contract" ]; then
    break
  fi
  if [ "$(docker inspect -f '{{.State.Running}}' "$API_NAME" 2>/dev/null || echo false)" != "true" ]; then
    log "FAILURE: test server exited before printing its contract"
    docker logs "$API_NAME" 2>&1 | tail -30 || true
    exit 1
  fi
  sleep 1
done
if [ -z "$contract" ]; then
  log "FAILURE: test server did not print its contract in time"
  docker logs "$API_NAME" 2>&1 | tail -30 || true
  exit 1
fi

# Parse the contract on the host with node (the same tool the counts parser uses; no /nix jq).
read_contract() {
  printf '%s' "$contract" | node -e '
    let raw = ""; process.stdin.on("data", (d) => (raw += d)); process.stdin.on("end", () => {
      try {
        const c = JSON.parse(raw.trim());
        const out = [
          c.base_url, c.worker_token,
          c.subscription.run_id, c.subscription.capability, c.subscription.chatgpt_account_id, String(c.subscription.generation),
          c.api_key.run_id, c.api_key.capability,
        ];
        if (out.some((v) => v === undefined || v === null || v === "")) { process.exit(2); }
        process.stdout.write(out.join("\n"));
      } catch { process.exit(1); }
    });'
}
if ! CONTRACT_FIELDS="$(read_contract)"; then
  log "FAILURE: could not parse the server contract: $contract"
  exit 1
fi
{
  IFS= read -r WORKER_BASE_URL
  IFS= read -r WORKER_TOKEN
  IFS= read -r SUB_RUN_ID
  IFS= read -r SUB_CAP
  IFS= read -r SUB_ACCOUNT
  IFS= read -r SUB_GEN
  IFS= read -r APIKEY_RUN_ID
  IFS= read -r APIKEY_CAP
} <<EOF
$CONTRACT_FIELDS
EOF
log "server up; contract base_url=$WORKER_BASE_URL sub_run=$SUB_RUN_ID api_run=$APIKEY_RUN_ID"

# 6. The lifecycle suite — CONFINEMENT posture, but on the INTERNAL network (loopback fake provider
#    inside the container; the network reaches only api+PG). CODEX_M3B_SRC pins the packaged
#    /app/src; CODEX_M3B_PACKAGED=1 turns on Block B; the server-contract env points the real
#    WorkerClient at the api container. Output is captured for the per-image count assertions.
log "running the packaged lifecycle suite in $IMAGE under the confinement posture (network $NET)"
set +e
timeout --kill-after=60s "$TIMEOUT" docker run --rm --network "$NET" \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /nix:exec --tmpfs /data --tmpfs /tmp:exec \
  --tmpfs /work/repo:rw,exec,nosuid,size=64m,uid=10001,gid=10002,mode=2770 \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -e CODEX_M3B_PACKAGED=1 -e CODEX_M3B_SRC=/app/src -e UZI_R2_COMMAND_ROOT_LINUX=1 \
  -e "CODEX_M3B_WORKER_BASE_URL=$WORKER_BASE_URL" \
  -e "CODEX_M3B_WORKER_TOKEN=$WORKER_TOKEN" \
  -e "CODEX_M3B_SUB_RUN_ID=$SUB_RUN_ID" \
  -e "CODEX_M3B_SUB_CAP=$SUB_CAP" \
  -e "CODEX_M3B_SUB_ACCOUNT=$SUB_ACCOUNT" \
  -e "CODEX_M3B_SUB_GEN=$SUB_GEN" \
  -e "CODEX_M3B_APIKEY_RUN_ID=$APIKEY_RUN_ID" \
  -e "CODEX_M3B_APIKEY_CAP=$APIKEY_CAP" \
  -v "$REPO/e2e":/work/e2e:ro \
  -v "$REPO/agent/test":/app/test:ro \
  --name "$LIFECYCLE_NAME" \
  "$IMAGE" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=120000 \
    /app/test/codex-command-root-linux.test.ts /work/e2e/codex-m3b/lifecycle.test.ts' 2>&1 | tee "$LIFECYCLE_OUT"
rc_lifecycle=${PIPESTATUS[0]}
set -e
docker rm -f "$LIFECYCLE_NAME" >/dev/null 2>&1 || true

# 7. Per-image count assertions (belt-and-braces; the suite itself already asserts these). Parse the
#    Block-A CODEX_M3B_COUNTS AND the Block-B real-path summaries with node (never a /nix jq),
#    reading the LAST occurrence of each, and require every real-path count > 0 + api_key refresh 0.
rc_counts=1
if node - "$LIFECYCLE_OUT" <<'NODE'
const fs = require("node:fs");
const text = fs.readFileSync(process.argv[2], "utf8");
const last = (marker) => {
  let found;
  for (const line of text.split("\n")) {
    const i = line.indexOf(marker + " ");
    if (i >= 0) found = line.slice(i + marker.length + 1).trim();
  }
  return found;
};
const fail = (m) => { console.error("count check FAILED: " + m); process.exit(1); };
const parse = (marker) => {
  const raw = last(marker);
  if (!raw) fail("no " + marker + " summary found");
  try { return JSON.parse(raw); } catch { fail("could not parse " + marker + ": " + raw); }
};
// Block A (injected fakes).
const a = parse("CODEX_M3B_COUNTS");
for (const k of ["tests", "callbacks", "delegations", "roots"]) {
  if (!(Number(a[k]) > 0)) fail(`Block A ${k} not > 0 (${a[k]})`);
}
// Block B subscription real-path counts.
const b = parse("CODEX_M3B_PACKAGED_REAL_COUNTS");
if (b.ran !== true) fail(`Block B subscription ran !== true (${b.ran})`);
for (const k of [
  "login", "providerTurns", "callbacks", "delegation", "submitPlanSignals", "doneSignals",
  "checkpoints", "providerRootsRegistered", "providerRootsReaped", "commandRootsRegistered",
  "commandRootsReaped", "refreshAdvanced", "refreshReplayed", "finalization",
]) {
  if (!(Number(b[k]) > 0)) fail(`Block B ${k} not > 0 (${b[k]})`);
}
// Block B api_key real-path counts (ZERO refresh is the load-bearing invariant).
const ak = parse("CODEX_M3B_PACKAGED_REAL_APIKEY_COUNTS");
if (ak.ran !== true) fail(`Block B api_key ran !== true (${ak.ran})`);
if (Number(ak.refresh) !== 0) fail(`Block B api_key refresh must be 0 (${ak.refresh})`);
if (!(Number(ak.release) > 0)) fail(`Block B api_key release not > 0 (${ak.release})`);
console.log("per-image counts OK (Block A + Block B subscription + api_key)");
NODE
then
  rc_counts=0
else
  rc_counts=1
fi

log "RESULT: controls=$rc_controls lifecycle=$rc_lifecycle counts=$rc_counts (0/0/0 = pass; 124 = watchdog)"
[ "$rc_controls" -eq 0 ] && [ "$rc_lifecycle" -eq 0 ] && [ "$rc_counts" -eq 0 ]
