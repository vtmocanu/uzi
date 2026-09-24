#!/usr/bin/env bash
# Issue #1607: run fixture.test.ts INSIDE the real worker image, through its root-start
# entrypoint and compose's hardened cap set, so the suite runs as the worker uid (10001)
# under the real worker/runner uid split. A host or single-uid run cannot reproduce the
# ownership split the bug needs, and a run wholly as root would pass vacuously.
#
#   ./run.sh            build the base worker image (unless skipped), then run the suite
#
# Environment:
#   HOME_SPLIT_IMAGE       image tag to build/run            (default: uzi-agent-home-split:base)
#   HOME_SPLIT_DOCKERFILE  Dockerfile to build               (default: agent/templates/base/Dockerfile)
#   HOME_SPLIT_SKIP_BUILD  set to 1 to reuse an existing image
#   HOME_SPLIT_MOUNT_SRC   set to 1 to test this tree's agent/src (mounted read-only) instead
#                          of the image's /app/src: validates a change against an older image
#                          without a rebuild
#   HOME_SPLIT_TIMEOUT     outer watchdog seconds            (default: 300)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${HOME_SPLIT_IMAGE:-uzi-agent-home-split:base}"
DOCKERFILE="${HOME_SPLIT_DOCKERFILE:-agent/templates/base/Dockerfile}"
TIMEOUT="${HOME_SPLIT_TIMEOUT:-300}"
# Outside the uzi- container namespace on purpose (CLAUDE.md, Destructive operations).
NAME="home-split-$$"

log() { printf '\n### %s\n' "$*"; }

if [ "${HOME_SPLIT_SKIP_BUILD:-}" = "1" ]; then
  log "HOME_SPLIT_SKIP_BUILD=1: reusing existing image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi

src_args=()
if [ "${HOME_SPLIT_MOUNT_SRC:-}" = "1" ]; then
  src_args=(-v "$REPO/agent/src":/work/agent/src:ro -e HOME_SPLIT_SRC=/work/agent/src)
  log "testing the mounted agent/src, not the image's /app/src"
fi

log "running the #1607 HOME cleanup suite in $IMAGE as the worker uid"
set +e
timeout --kill-after=30s "$TIMEOUT" docker run --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --tmpfs /data \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -v "$REPO/e2e":/work/e2e:ro \
  "${src_args[@]}" \
  --name "$NAME" \
  "$IMAGE" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=120000 \
    /work/e2e/home-uid-split/fixture.test.ts'
rc=$?
set -e
# Best-effort removal of exactly this named container (never a broad sweep).
docker rm -f "$NAME" >/dev/null 2>&1 || true
exit "$rc"
