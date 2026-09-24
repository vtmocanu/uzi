#!/usr/bin/env bash
# Measure per-component ephemeral-storage usage for a representative docker-lane
# hosted-worker workload (issue #1597 M3a).
#
# Kubelet's evictionMessage for a container is Rootfs.UsedBytes + Logs.UsedBytes
# (the container's writable layer + its container logs). The `run-workdir` emptyDir
# (mounted at /data/runner in the docker lane) and the `data`/`nix` PVCs are
# accounted SEPARATELY by kubelet and are NOT part of that per-container figure.
# This script reproduces a representative workload against a real (or proxy)
# worker image and reports which rows land in which bucket, so a demonstrated
# writable-layer/log growth source can be named (or the absence of one reported
# honestly) rather than guessed at.
#
# Usage:
#   scripts/measure-worker-ephemeral.sh [IMAGE]
#
#   IMAGE   an already-built image tag to use as-is. If omitted, the script
#           tries to build the real worker image from
#           agent/templates/base/Dockerfile (repo-root build context, matching
#           docker-compose.yml's `agent` service). If that build is not
#           attempted (see UZI_MEASURE_NO_BUILD) or fails, it falls back to a
#           documented proxy image (node:24-alpine + git + go) and says so.
#
# Env overrides:
#   UZI_MEASURE_NO_BUILD=1   skip the real-image build attempt, go straight to
#                            the proxy image (useful for a fast rerun).
#   UZI_MEASURE_BUILD_TIMEOUT  seconds allowed for the real-image build
#                            (default 900 = 15 min, leaving headroom under the
#                            ~20 min budget for the rest of the script).
#   UZI_MEASURE_KEEP=1       skip cleanup (containers/volumes/image), for
#                            interactive follow-up. Off by default.
#
# All docker objects this script creates are named with the exact prefix
# `eph-1597-<pid>` and are removed BY EXACT NAME on exit (trap). Nothing in the
# `uzi-` namespace is created or touched.
set -euo pipefail

SCRIPT_PID=$$
PREFIX="eph-1597-${SCRIPT_PID}"
CONTAINER="${PREFIX}"
VOL_DATA="${PREFIX}-data"
VOL_RUNWORKDIR="${PREFIX}-runworkdir"
VOL_NIX="${PREFIX}-nix"
BUILT_IMAGE_TAG="${PREFIX}-worker-img"
WORK="$(mktemp -d "/tmp/${PREFIX}.XXXXXX")"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\n=== %s ===\n' "$1"; }
info() { printf '%s\n' "$1"; }

IMAGE_ARG="${1:-}"
IMAGE=""
IMAGE_SOURCE=""
BUILT_IMAGE=0
# IS_PROXY=1 only for the synthetic node:24-alpine fallback this script builds
# itself -- every other path (a real build, or a user-supplied tag) is treated
# as "real-worker-shaped" for the entrypoint/mount handling below, since a
# user-supplied tag is the caller's own responsibility to be the real image.
IS_PROXY=0

cleanup() {
  local rc=$?
  if [ "${UZI_MEASURE_KEEP:-0}" = "1" ]; then
    info "UZI_MEASURE_KEEP=1: leaving ${CONTAINER}, volumes and any built image in place for follow-up."
    return "$rc"
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker volume rm "$VOL_DATA" "$VOL_RUNWORKDIR" "$VOL_NIX" >/dev/null 2>&1 || true
  if [ "$BUILT_IMAGE" = "1" ]; then
    docker rmi -f "$BUILT_IMAGE_TAG" >/dev/null 2>&1 || true
  fi
  # $WORK (phase logs, build log) is deliberately left on disk for the report;
  # it is not a docker object and is not in the uzi- or eph-1597- container/
  # volume namespace, so it is outside this trap's exact-name-removal scope.
  return "$rc"
}
trap cleanup EXIT

# ─── 1. Resolve the image ──────────────────────────────────────────────────
log "Resolving worker image"
if [ -n "$IMAGE_ARG" ]; then
  IMAGE="$IMAGE_ARG"
  IMAGE_SOURCE="user-supplied tag: $IMAGE"
  info "Using $IMAGE_SOURCE"
elif [ "${UZI_MEASURE_NO_BUILD:-0}" = "1" ]; then
  info "UZI_MEASURE_NO_BUILD=1: skipping the real worker-image build."
else
  BUILD_TIMEOUT="${UZI_MEASURE_BUILD_TIMEOUT:-900}"
  info "Attempting real worker image build from agent/templates/base/Dockerfile" \
       "(context: repo root, matching docker-compose.yml's agent service)."
  if timeout "$BUILD_TIMEOUT" docker build \
       -f "$REPO_ROOT/agent/templates/base/Dockerfile" \
       -t "$BUILT_IMAGE_TAG" \
       "$REPO_ROOT" >"$WORK/build.log" 2>&1; then
    IMAGE="$BUILT_IMAGE_TAG"
    IMAGE_SOURCE="real worker image, built from agent/templates/base/Dockerfile"
    BUILT_IMAGE=1
    info "Built $IMAGE_SOURCE"
  else
    info "Real-image build failed or timed out after ${BUILD_TIMEOUT}s; see $WORK/build.log."
    info "Falling back to the documented proxy image."
  fi
fi

if [ -z "$IMAGE" ]; then
  PROXY_IMAGE="node:24-alpine"
  info "Using proxy image: $PROXY_IMAGE + git + go (NOT the real worker image;" \
       "no nix/devbox/chromium/setpriv/entrypoint layers -- writable-layer and" \
       "/home numbers from this run are a LOWER BOUND, not the real figure)."
  cat >"$WORK/Dockerfile.proxy" <<'EOF'
FROM node:24-alpine
RUN apk add --no-cache git bash go
ENV HOME=/home/worker
RUN addgroup -g 10001 worker && adduser -u 10001 -G worker -h /home/worker -D worker
EOF
  docker build -f "$WORK/Dockerfile.proxy" -t "$BUILT_IMAGE_TAG" "$WORK" >"$WORK/proxy-build.log" 2>&1
  IMAGE="$BUILT_IMAGE_TAG"
  IMAGE_SOURCE="proxy image (node:24-alpine + git + go), NOT the real worker image"
  BUILT_IMAGE=1
  IS_PROXY=1
fi
info "IMAGE_USED=$IMAGE ($IMAGE_SOURCE)"

# ─── 2. Stand up the container ─────────────────────────────────────────────
# /data/runner MUST be its own mount nested under /data (a separate volume), to
# shadow the PVC subpath exactly like the k8s docker-lane pod: the emptyDir
# `run-workdir` is mounted at dataMountPath + "/runner" INSIDE the `data` PVC's
# mountpoint (controller/internal/kube/render.go, render_dind.go:60-61), so the
# two are logically distinct volumes even though one path is a subdirectory of
# the other.
log "Creating volumes + container"
docker volume create "$VOL_DATA" >/dev/null
docker volume create "$VOL_RUNWORKDIR" >/dev/null
docker volume create "$VOL_NIX" >/dev/null
info "Volumes: $VOL_DATA -> /data (PVC stand-in), $VOL_RUNWORKDIR -> /data/runner (emptyDir stand-in), $VOL_NIX -> /nix"

# Real image: start via its own ENTRYPOINT with CMD overridden to `sleep
# infinity`. The entrypoint still runs as root, seeds /data and /nix from the
# image on first use (agent/templates/entrypoint.sh), sets up the per-uid
# TMPDIRs under the ambient docker-lane TMPDIR, then setpriv-drops root ->
# worker and execs "$@" under tini -- so the container ends up in the exact
# runtime shape a real docker-lane pod reaches, just idling instead of running
# `npm run start`.
#
# Proxy image: no entrypoint/setpriv/uid-split exists, so run as the `worker`
# user directly with a plain sleep.
RUN_ARGS=(
  -d --name "$CONTAINER"
  -v "${VOL_DATA}:/data"
  -v "${VOL_RUNWORKDIR}:/data/runner"
  -e "TMPDIR=/data/runner"
)
if [ "$IS_PROXY" = 0 ]; then
  RUN_ARGS+=(-v "${VOL_NIX}:/nix")
  docker run "${RUN_ARGS[@]}" "$IMAGE" sleep infinity >/dev/null
else
  RUN_ARGS+=(-u worker -e "HOME=/home/worker")
  docker run "${RUN_ARGS[@]}" "$IMAGE" sleep infinity >/dev/null
fi

# Wait for the entrypoint's root setup window to actually FINISH, not merely
# for the container to accept an exec. `docker exec` succeeds the instant the
# container namespace exists -- it does not wait for PID 1's own script to
# reach any particular point, so a bare readiness probe races entrypoint.sh's
# migrate/seed pass and was observed (2026-09-24) to let a chown below land
# mid-migration and fail EPERM. Poll for the marker file the migration itself
# drops (`/data/.uzi-migrated-worker-worker`) as the real completion signal;
# the proxy image never creates it, so cap the wait there on plain readiness.
for _ in $(seq 1 60); do
  STATE="$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || echo false)"
  [ "$STATE" = "true" ] || break
  if [ "$IS_PROXY" = 0 ]; then
    if docker exec "$CONTAINER" sh -c '[ -e /data/.uzi-migrated-worker-worker ]' >/dev/null 2>&1; then
      break
    fi
  else
    if docker exec "$CONTAINER" sh -c 'true' >/dev/null 2>&1; then
      break
    fi
  fi
  sleep 1
done

RUNTIME_HOME="$(docker exec -u worker "$CONTAINER" sh -c 'echo "$HOME"' 2>/dev/null || echo /home/worker)"
info "Container up. Runtime default HOME=$RUNTIME_HOME"

# The nested run-workdir mount (/data/runner) comes up root:root on a bare
# docker/compose start: entrypoint.sh's migrate/seed pass only touches /data's
# own root and /nix, not a volume nested inside /data, and there is no kubelet
# here to apply the pod's fsGroup recursively to a freshly-created emptyDir
# (the real k8s path: an emptyDir starts empty, so kubelet's fsGroup walk over
# it is cheap and DOES run, unlike the PVC roots entrypoint.sh's align_pvc_root
# works around). Emulate that one-time fsGroup application so the workload
# below hits the same writable, group-owned run-workdir a real pod would.
if [ "$IS_PROXY" = 0 ]; then
  docker exec "$CONTAINER" sh -c 'chown worker:runner /data/runner && chmod 2775 /data/runner' >/dev/null 2>&1 || true
fi

# exec_sparse CMD... — run CMD inside the container under the SPARSE agent/
# child env exactly as buildSdkEnv (agent/src/sdk-env.ts) and the Codex
# launcher (agent/src/codex/launcher.ts) construct it for a docker-lane run:
#   HOME    = /data/agent-home/<runId>      (per-run root on the data PVC)
#   TMPDIR  = /data/runner                  (render.go's ambient TMPDIR, the
#                                             dind-shared emptyDir)
#   PATH    = the container's own PATH (runnerPath() falls back to it)
# This is deliberately SPARSE (no forge PAT, no join token, no inherited
# worker env) -- the same trust boundary the real agent subprocess gets.
RUN_ID="run1"
SPARSE_HOME="/data/agent-home/${RUN_ID}"
# `docker exec` with no `-u` resolves the IMAGE's configured user (root, since
# the Dockerfile carries no USER directive -- the entrypoint's setpriv drop to
# `worker` is a runtime exec, not an image config change). The real agent/
# check/worker-process subprocesses all run as `worker` (uid 10001) after that
# drop, so every exec below is pinned to `-u worker` to match; exec'ing as the
# default (root) would both misrepresent the real trust boundary and can trip
# permission edge cases the setgid run-workdir tree does not hit for `worker`.
exec_sparse() {
  docker exec -u worker -e "HOME=${SPARSE_HOME}" -e "TMPDIR=/data/runner" "$CONTAINER" "$@"
}
docker exec -u worker "$CONTAINER" mkdir -p "$SPARSE_HOME"

# exec_worker CMD... — run CMD under the container's OWN default env
# (HOME=/home/worker, no TMPDIR override), modelling anything the worker
# process / entrypoint itself does OUTSIDE the sparse agent child env (git
# config, npm's own state, docker CLI config, etc), still as the `worker` uid.
exec_worker() {
  docker exec -u worker "$CONTAINER" "$@"
}

# ─── 3. Workload phases ─────────────────────────────────────────────────────
log "Phase 1: git clone (local bundle, offline) into \$TMPDIR-rooted run workdir"
BUNDLE="$WORK/repo.bundle"
git -C "$REPO_ROOT" bundle create "$BUNDLE" HEAD >"$WORK/bundle.log" 2>&1
docker cp "$BUNDLE" "$CONTAINER:/data/runner/repo.bundle"
CLONE_DIR="/data/runner/${RUN_ID}"
exec_sparse sh -c "git clone -q /data/runner/repo.bundle '$CLONE_DIR'" || info "WARN: clone phase failed (continuing to measurement)"

log "Phase 2: npm ci in agent/ under the sparse env"
if exec_sparse sh -c "[ -f '$CLONE_DIR/agent/package.json' ]"; then
  exec_sparse sh -c "cd '$CLONE_DIR/agent' && npm ci --ignore-scripts --no-audit --no-fund" \
    >"$WORK/npm-ci.log" 2>&1 || info "WARN: npm ci failed or npm unavailable in this image (see $WORK/npm-ci.log)"
else
  info "SKIP: agent/package.json not found in cloned tree (unshallow bundle?)"
fi

log "Phase 3: go build + test of a small package under the sparse env"
if exec_sparse sh -c "command -v go >/dev/null 2>&1"; then
  GO_PKG=""
  for cand in "api/internal/config" "controller/internal/config"; do
    if exec_sparse sh -c "[ -d '$CLONE_DIR/$cand' ]"; then GO_PKG="$cand"; break; fi
  done
  if [ -n "$GO_PKG" ]; then
    # GOTOOLCHAIN=local: the image's provisioned go is pinned older than this
    # repo's go.mod toolchain directive, so an unpinned build would fetch a
    # fresh toolchain zip (a multi-hundred-MB one-off download unrelated to
    # the ephemeral-storage question this script asks) into the module cache
    # under the sparse HOME. Pin to what's provisioned, same as a real run's
    # devbox-provisioned toolchain would be used as-is.
    exec_sparse sh -c "cd '$CLONE_DIR/$GO_PKG' && GOFLAGS=-buildvcs=false GOTOOLCHAIN=local go build ./... && GOFLAGS=-buildvcs=false GOTOOLCHAIN=local go test ./... -count=1" \
      >"$WORK/go-build.log" 2>&1 || info "WARN: go build/test failed (see $WORK/go-build.log)"
  else
    info "SKIP: no small Go package found to build"
  fi
else
  info "SKIP: no 'go' toolchain on PATH in this image"
fi

log "Phase 4: worker-process phase under the container's DEFAULT env (HOME=$RUNTIME_HOME)"
exec_worker sh -c 'git config --global user.email "eph@example.com" && git config --global user.name "eph"' \
  >"$WORK/worker-git-config.log" 2>&1 || true
exec_worker sh -c 'command -v npm >/dev/null 2>&1 && npm config set fund false 2>/dev/null; true' \
  >"$WORK/worker-npm-config.log" 2>&1 || true
exec_worker sh -c 'command -v docker >/dev/null 2>&1 && docker version >/dev/null 2>&1; true' \
  >"$WORK/worker-docker-version.log" 2>&1 || true

# ─── 4. Measure ─────────────────────────────────────────────────────────────
log "Measuring"

SIZE_LINE="$(docker ps -a --filter "name=^/${CONTAINER}\$" --format '{{.Size}}')"
SIZE_RW_BYTES="$(docker inspect --size --format '{{.SizeRw}}' "$CONTAINER" 2>/dev/null || echo 0)"

LOG_PATH="$(docker inspect --format '{{.LogPath}}' "$CONTAINER" 2>/dev/null || echo '')"
LOG_BYTES=0
if [ -n "$LOG_PATH" ] && [ -r "$LOG_PATH" ]; then
  LOG_BYTES="$(stat -c '%s' "$LOG_PATH" 2>/dev/null || echo 0)"
else
  LOG_BYTES="$(docker logs "$CONTAINER" 2>&1 | wc -c | tr -d ' ')"
fi

du_in_container() {
  # $1 = path inside the container. Prints bytes, or 0 if the path is absent.
  docker exec "$CONTAINER" sh -c "[ -e '$1' ] && du -sx -k '$1' 2>/dev/null | cut -f1 || echo 0" 2>/dev/null \
    | awk '{print ($1+0)*1024}'
}

HOME_BYTES="$(du_in_container "$RUNTIME_HOME")"
TMP_BYTES="$(du_in_container /tmp)"
ROOT_BYTES="$(du_in_container /root)"
VARTMP_BYTES="$(du_in_container /var/tmp)"
USRLOCAL_BYTES="$(du_in_container /usr/local)"
RUNWORKDIR_BYTES="$(du_in_container /data/runner)"
DATA_BYTES="$(du_in_container /data)"
AGENTHOME_BYTES="$(du_in_container /data/agent-home)"
NIX_BYTES="$(du_in_container /nix)"

# docker diff highlights: what actually changed on the container's own
# filesystem (union of the writable layer's changed paths), independent of du.
DIFF_OUT="$(docker diff "$CONTAINER" 2>/dev/null || true)"
DIFF_TOP="$(printf '%s\n' "$DIFF_OUT" | awk '{print $2}' | awk -F/ '{if (NF>=3) print "/"$2"/"$3; else print $0}' | sort | uniq -c | sort -rn | head -20)"

bytes_h() {
  # human-readable bytes without depending on `numfmt` (not always present).
  local b=$1
  awk -v b="$b" 'BEGIN {
    split("B KB MB GB TB", u, " ");
    i = 1;
    while (b >= 1024 && i < 5) { b /= 1024; i++ }
    printf "%.1f%s", b, u[i]
  }'
}

log "Component table"
printf '%-28s %14s  %-22s  %s\n' "component" "bytes" "human" "kubelet bucket"
printf '%-28s %14s  %-22s  %s\n' "----------------------------" "--------------" "----------------------" "--------------"
printf '%-28s %14s  %-22s  %s\n' "writable layer (SizeRw)" "$SIZE_RW_BYTES" "$(bytes_h "$SIZE_RW_BYTES")" "COUNTED (Rootfs.UsedBytes)"
printf '%-28s %14s  %-22s  %s\n' "container log bytes" "$LOG_BYTES" "$(bytes_h "$LOG_BYTES")" "COUNTED (Logs.UsedBytes)"
printf '%-28s %14s  %-22s  %s\n' "  home ($RUNTIME_HOME)" "$HOME_BYTES" "$(bytes_h "$HOME_BYTES")" "  (subset of writable layer, unless HOME is a volume)"
printf '%-28s %14s  %-22s  %s\n' "  /tmp" "$TMP_BYTES" "$(bytes_h "$TMP_BYTES")" "  (subset of writable layer)"
printf '%-28s %14s  %-22s  %s\n' "  /root" "$ROOT_BYTES" "$(bytes_h "$ROOT_BYTES")" "  (subset of writable layer)"
printf '%-28s %14s  %-22s  %s\n' "  /var/tmp" "$VARTMP_BYTES" "$(bytes_h "$VARTMP_BYTES")" "  (subset of writable layer)"
printf '%-28s %14s  %-22s  %s\n' "  /usr/local" "$USRLOCAL_BYTES" "$(bytes_h "$USRLOCAL_BYTES")" "  (subset of writable layer)"
printf '%-28s %14s  %-22s  %s\n' "run-workdir (/data/runner)" "$RUNWORKDIR_BYTES" "$(bytes_h "$RUNWORKDIR_BYTES")" "SEPARATE (emptyDir)"
printf '%-28s %14s  %-22s  %s\n' "data PVC (/data total)" "$DATA_BYTES" "$(bytes_h "$DATA_BYTES")" "SEPARATE (PVC)"
printf '%-28s %14s  %-22s  %s\n' "  agent-home (/data/agent-home)" "$AGENTHOME_BYTES" "$(bytes_h "$AGENTHOME_BYTES")" "  (subset of data PVC)"
printf '%-28s %14s  %-22s  %s\n' "nix PVC (/nix)" "$NIX_BYTES" "$(bytes_h "$NIX_BYTES")" "SEPARATE (PVC)"
printf '\n%s\n' "docker ps -s size column: $SIZE_LINE"

log "docker diff (top changed dirs on the container's own filesystem)"
printf '%s\n' "$DIFF_TOP"

log "Summary"
info "IMAGE_USED=$IMAGE_SOURCE"
info "writable layer (SizeRw) = $(bytes_h "$SIZE_RW_BYTES")"
info "container log bytes     = $(bytes_h "$LOG_BYTES")"
info "run-workdir (emptyDir)  = $(bytes_h "$RUNWORKDIR_BYTES")"
info "data PVC                = $(bytes_h "$DATA_BYTES")"
info "nix PVC                 = $(bytes_h "$NIX_BYTES")"
info "Logs kept under: $WORK (not removed by this script's cleanup)"
