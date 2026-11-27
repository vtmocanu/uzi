#!/usr/bin/env bash
# Issue #4416 M3: prove, through the production command path, that a Docker login in one
# sandboxed Codex command persists for the run's later commands. See README.md.
#
# Safety: the worker's Docker daemon is shared. This script creates only containers and images
# named dcp-test-<hex>-* (the hex is unique per run), labels the containers, and removes them by
# exact name after verifying the label. It never prunes, never runs a system command, never
# touches a container or image it did not create, and never prints the environment.
set -euo pipefail

# Pinned by digest. registry:3 and httpd:2-alpine were resolved with `docker pull` +
# `docker inspect --format '{{index .RepoDigests 0}}'` on 2026-11-27; the alpine fixture pin is the
# one e2e/dind-builder-prune/run.sh uses.
readonly REGISTRY_IMAGE='registry@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c'
readonly HTPASSWD_IMAGE='httpd@sha256:32bd0639bcc4917ef1fd9363a1d9dd2dd9c498565118b062a10d51e43a5c3a24'
readonly FIXTURE_IMAGE='alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6'
readonly OWNER_LABEL='dcp-test.owner'
readonly TEST_FILE='agent/test/codex-docker-config-linux.test.ts'

for tool in docker timeout od tr awk grep tee mktemp; do
  command -v "$tool" >/dev/null || { echo "FAIL: $tool is required" >&2; exit 1; }
done
if [[ -z ${DOCKER_HOST:-} ]]; then
  echo "FAIL: DOCKER_HOST must name the Docker daemon the worker container reaches over --network host" >&2
  exit 1
fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
readonly HERE REPO
token="$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
readonly token
[[ $token =~ ^[0-9a-f]{12}$ ]] || exit 1
readonly PREFIX="dcp-test-$token"
readonly REGISTRY_NAME="$PREFIX-registry"
readonly HTPASSWD_NAME="$PREFIX-htpasswd"
readonly RUN_NAME="$PREFIX-run"
readonly WORKER_IMAGE="${DCP_WORKER_IMAGE:-$PREFIX-worker}"
readonly RUN_IMAGE="$PREFIX-runimg"
OUT="${DCP_OUT:-$REPO/.uzi/scratch/dcp-$token}"
mkdir -p "$OUT"
readonly OUT
readonly TEST_TIMEOUT="${DCP_TEST_TIMEOUT:-1500}"
readonly BUILD_TIMEOUT="${DCP_BUILD_TIMEOUT:-3000}"
readonly PORT="${DCP_REGISTRY_PORT:-$((20000 + RANDOM % 20000))}"
readonly REGISTRY_REF="127.0.0.1:$PORT"
readonly PRIVATE_IMAGE="$REGISTRY_REF/private/base:1"

log() { printf '\n### %s\n' "$*"; }

built_worker=0 built_run=0

# remove_owned_container removes a container by exact name, only when it carries this run's label.
remove_owned_container() {
  local name="$1" owner
  owner="$(docker inspect --type container --format "{{ index .Config.Labels \"$OWNER_LABEL\" }}" "$name" 2>/dev/null || true)"
  if [[ $owner == "$token" ]]; then
    docker rm -f "$name" >/dev/null 2>&1 || true
  fi
}

# cleanup removes only what this run created, by exact name or by this run's unique name prefix.
cleanup() {
  local ref
  remove_owned_container "$RUN_NAME"
  remove_owned_container "$REGISTRY_NAME"
  remove_owned_container "$HTPASSWD_NAME"
  # Images the test built inside the worker image carry this run's prefix.
  while read -r ref; do
    if [[ $ref == "$PREFIX"-* ]]; then
      docker rmi -f "$ref" >/dev/null 2>&1 || true
    fi
  done < <(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null || true)
  # The private tag is unique to this run (its registry port is part of the name), so it is always
  # attempted, whatever step failed; it only untags when it shares an image id with the pinned fixture.
  docker rmi "$PRIVATE_IMAGE" >/dev/null 2>&1 || true
  if [[ $built_run == 1 ]]; then docker rmi "$RUN_IMAGE" >/dev/null 2>&1 || true; fi
  if [[ $built_worker == 1 && -z ${DCP_WORKER_IMAGE:-} ]]; then docker rmi "$WORKER_IMAGE" >/dev/null 2>&1 || true; fi
  if [[ -n ${SEED_CONFIG:-} ]]; then rm -rf "$SEED_CONFIG"; fi
  if [[ -n ${HTPASSWD_DIR:-} ]]; then rm -rf "$HTPASSWD_DIR"; fi
  if [[ -n ${RUN_CTX:-} ]]; then rm -rf "$RUN_CTX"; fi
  return 0
}
trap cleanup EXIT

# Pinned public images are pulled when absent and then left in place: another job on the shared
# daemon may use them, so this run never removes an image only because it pulled it.
ensure_image() {
  local ref="$1"
  if ! docker image inspect "$ref" >/dev/null 2>&1; then
    docker pull -q "$ref" >/dev/null
  fi
}

# Credentials: assembled at runtime from fragments, never a literal.
UZI_DCP_USER="dcp-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
UZI_DCP_PASS="$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
export UZI_DCP_USER UZI_DCP_PASS
export UZI_DCP_REGISTRY="$REGISTRY_REF" UZI_DCP_IMAGE="$PRIVATE_IMAGE" UZI_DCP_PREFIX="$PREFIX"
export UZI_DOCKER_CONFIG_LINUX=1

# 1. The worker image at the current tree.
if [[ -n ${DCP_WORKER_IMAGE:-} ]]; then
  log "reusing worker image $WORKER_IMAGE (DCP_WORKER_IMAGE)"
else
  log "building worker image $WORKER_IMAGE from agent/templates/base/Dockerfile (several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 timeout --kill-after=60s "$BUILD_TIMEOUT" docker build \
    -f "$REPO/agent/templates/base/Dockerfile" -t "$WORKER_IMAGE" \
    --label "$OWNER_LABEL=$token" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
  built_worker=1
fi

# 2. A thin layer carrying the test file. The base image copies agent/src but not agent/test, and a
#    bind-mount source would resolve on the daemon host, so the file travels in the build context.
RUN_CTX="$(mktemp -d "$OUT/runctx.XXXXXX")"
cp "$REPO/$TEST_FILE" "$RUN_CTX/test.ts"
cat >"$RUN_CTX/Dockerfile" <<EOF
FROM $WORKER_IMAGE
COPY test.ts /app/test/codex-docker-config-linux.test.ts
RUN chmod 0755 /app/test && chmod 0644 /app/test/codex-docker-config-linux.test.ts
EOF
log "building test layer $RUN_IMAGE"
DOCKER_BUILDKIT=1 docker build -q -t "$RUN_IMAGE" --label "$OWNER_LABEL=$token" "$RUN_CTX" >/dev/null
built_run=1

# 3. A password-protected private registry published on the daemon host's loopback.
ensure_image "$REGISTRY_IMAGE"
ensure_image "$HTPASSWD_IMAGE"
HTPASSWD_DIR="$(mktemp -d "$OUT/htpasswd.XXXXXX")"
docker create --name "$HTPASSWD_NAME" --label "$OWNER_LABEL=$token" -i --entrypoint htpasswd "$HTPASSWD_IMAGE" -Bin "$UZI_DCP_USER" >/dev/null
printf '%s' "$UZI_DCP_PASS" | docker start -a -i "$HTPASSWD_NAME" >"$HTPASSWD_DIR/htpasswd"
remove_owned_container "$HTPASSWD_NAME"
[[ -s $HTPASSWD_DIR/htpasswd ]] || { echo "FAIL: htpasswd generation produced nothing" >&2; exit 1; }

log "starting private registry $REGISTRY_NAME on 127.0.0.1:$PORT of the daemon host"
docker create --name "$REGISTRY_NAME" --label "$OWNER_LABEL=$token" \
  -p "127.0.0.1:$PORT:5000" \
  -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=dcp-test -e REGISTRY_AUTH_HTPASSWD_PATH=/htpasswd \
  "$REGISTRY_IMAGE" >/dev/null
docker cp "$HTPASSWD_DIR/htpasswd" "$REGISTRY_NAME:/htpasswd"
docker start "$REGISTRY_NAME" >/dev/null

# 4. Seed: push a small pinned image into the private registry with a throwaway client config.
SEED_CONFIG="$(mktemp -d "$OUT/seedcfg.XXXXXX")"
seed_login() {
  printf '%s' "$UZI_DCP_PASS" | DOCKER_CONFIG="$SEED_CONFIG" docker login "$REGISTRY_REF" -u "$UZI_DCP_USER" --password-stdin >/dev/null 2>&1
}
ready=0
for _ in $(seq 1 60); do
  if seed_login; then ready=1; break; fi
  sleep 1
done
if [[ $ready != 1 ]]; then
  echo "FAIL: the daemon could not reach the private registry on $REGISTRY_REF" >&2
  docker logs "$REGISTRY_NAME" 2>&1 | tail -20 >&2 || true
  exit 1
fi
ensure_image "$FIXTURE_IMAGE"
docker tag "$FIXTURE_IMAGE" "$PRIVATE_IMAGE"
DOCKER_CONFIG="$SEED_CONFIG" docker push -q "$PRIVATE_IMAGE" >/dev/null
docker rmi "$PRIVATE_IMAGE" >/dev/null
# Without credentials the registry must refuse the seeded image (the positive control for the refused-pull steps).
if DOCKER_CONFIG="$OUT/nocreds-$token" docker pull -q "$PRIVATE_IMAGE" >/dev/null 2>&1; then
  echo "FAIL: the private registry served an image without credentials" >&2
  exit 1
fi

# 5. The suite, as the worker uid, through the real entrypoint and the production cap set.
log "running the suite in $RUN_IMAGE (--network host: the daemon and the registry share the daemon host's loopback)"
LOG="$OUT/suite.log"
set +e
timeout --kill-after=60s "$TEST_TIMEOUT" docker run --name "$RUN_NAME" --label "$OWNER_LABEL=$token" \
  --network host \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /nix:exec --tmpfs /data --tmpfs /tmp:exec \
  --tmpfs /var/cache/uzi-codex-cmd:rw,exec,nosuid,nodev,mode=0700 \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -e DOCKER_HOST -e UZI_DOCKER_CONFIG_LINUX -e UZI_DCP_REGISTRY -e UZI_DCP_USER -e UZI_DCP_PASS -e UZI_DCP_IMAGE -e UZI_DCP_PREFIX \
  "$RUN_IMAGE" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=900000 /app/test/codex-docker-config-linux.test.ts' \
  2>&1 | tee "$LOG"
rc=${PIPESTATUS[0]}
set -e

# 6. Verdicts.
log "step verdicts"
grep -F 'DCP_STEP ' "$LOG" || true
verdict=0
for want in \
  'persist-off a-pull-refused-before-login' 'persist-off b-login' 'persist-off c-later-command-pulls' \
  'persist-off d-later-command-builds-FROM-private' 'race-off e-concurrent-first-commands' \
  'degrade-off f-planted-foreign-docker-dir' \
  'claude a-pull-refused-before-login' 'claude b-login' 'claude c-later-process-pulls'; do
  if ! grep -qF "DCP_STEP $want PASS" "$LOG"; then
    echo "MISSING/FAILED: $want" >&2
    verdict=1
  fi
done
if grep -qF 'DCP_STEP required ALL NOT PROVEN' "$LOG"; then
  echo "required: NOT PROVEN (Landlock unavailable in this container)"
else
  for want in \
    'persist-required a-pull-refused-before-login' 'persist-required b-login' 'persist-required c-later-command-pulls' \
    'persist-required d-later-command-builds-FROM-private' 'race-required e-concurrent-first-commands' \
    'degrade-required f-planted-foreign-docker-dir' 'isolation-required g-sibling-docker-dir-denied'; do
    if ! grep -qF "DCP_STEP $want PASS" "$LOG"; then
      echo "MISSING/FAILED: $want" >&2
      verdict=1
    fi
  done
fi
if grep -qE '^DCP_STEP .* FAIL$' "$LOG"; then verdict=1; fi
log "RESULT: suite rc=$rc verdict=$verdict (0/0 = pass)"
[[ $rc -eq 0 && $verdict -eq 0 ]]
