#!/usr/bin/env bash
# Issue #1783: run fixture.test.ts INSIDE the real worker image, through its root-start
# entrypoint and compose's hardened cap set, so the run-quiescence reaper is exercised as the
# worker uid (10001) under the real worker/runner/runner-cmd uid split: the scan and kills run
# in the runner-uid helper started through the production setpriv wrapper. A host or
# single-uid run cannot reproduce the uid split, and a run wholly as root passes vacuously.
#
#   ./run.sh            build the base worker image (unless skipped), then run the suite
#
# Environment:
#   CQ_IMAGE        image tag to build/run            (default: uzi-agent-clone-quiescence:base)
#   CQ_DOCKERFILE   Dockerfile to build               (default: agent/templates/base/Dockerfile)
#   CQ_SKIP_BUILD   set to 1 to reuse an existing image
#   CQ_MOUNT_SRC    set to 1 to test this tree's agent/src (mounted read-only under /app, so
#                   the helper still resolves the image's tsx) instead of the image's /app/src
#   CQ_BUILD_ARGS   extra `docker build` arguments, e.g. "--network host"
#   CQ_TIMEOUT      outer watchdog seconds            (default: 300)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${CQ_IMAGE:-uzi-agent-clone-quiescence:base}"
DOCKERFILE="${CQ_DOCKERFILE:-agent/templates/base/Dockerfile}"
TIMEOUT="${CQ_TIMEOUT:-300}"
# Outside the uzi- container namespace on purpose (CLAUDE.md, Destructive operations).
NAME="cq-$$"

log() { printf '\n### %s\n' "$*"; }

if [ "${CQ_SKIP_BUILD:-}" = "1" ]; then
  log "CQ_SKIP_BUILD=1: reusing existing image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (several minutes on a cold cache)"
  build_args=()
  if [ -n "${CQ_BUILD_ARGS:-}" ]; then
    read -r -a build_args <<<"$CQ_BUILD_ARGS"
  fi
  DOCKER_BUILDKIT=1 docker build "${build_args[@]}" -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi

src_args=()
if [ "${CQ_MOUNT_SRC:-}" = "1" ]; then
  src_args=(--mount "type=bind,src=$REPO/agent/src,dst=/app/cq-src,readonly" -e CQ_SRC=/app/cq-src)
  log "testing the mounted agent/src, not the image's /app/src"
fi

log "running the #1783 clone-quiescence suite in $IMAGE as the worker uid"
set +e
# --init: a root-owned pid 1 (docker-init) is part of the process table the reaper must ignore.
timeout --kill-after=30s "$TIMEOUT" docker run --rm --init --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --tmpfs /data \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  --mount "type=bind,src=$REPO/e2e,dst=/work/e2e,readonly" \
  "${src_args[@]}" \
  --name "$NAME" \
  "$IMAGE" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=120000 \
    /work/e2e/clone-quiescence/fixture.test.ts'
rc=$?
set -e
# Best-effort removal of exactly this named container (never a broad sweep).
docker rm -f "$NAME" >/dev/null 2>&1 || true
exit "$rc"
