#!/usr/bin/env bash
# PRD #1171 m5 — host orchestrator for the standalone PACKAGING controls.
#
# The lighter sibling of run-lifecycle.sh: it builds the REAL worker image and runs ONLY the
# packaging controls (controls.sh) inside it, through the real root entrypoint under the
# writable-root posture — so the on-disk ownership it proves (the supervisor + the m5 fileop
# helper root-owned 0555) is the true production posture. run-lifecycle.sh runs this same
# controls leg AND the lifecycle suite; use this when you only want the binary-install proof.
#
#   ./run.sh                          build (unless skipped) + run the packaging controls
#
# Environment:
#   UZI_M3B_IMAGE       image tag to build/run          (default: uzi-agent-m3b:base)
#   UZI_M3B_DOCKERFILE  Dockerfile to build             (default: agent/templates/base/Dockerfile)
#   UZI_M3B_SKIP_BUILD  set to 1 to reuse an existing image (skip docker build)
#   UZI_M3B_TIMEOUT     wall timeout (s)                (default: 180)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${UZI_M3B_IMAGE:-uzi-agent-m3b:base}"
DOCKERFILE="${UZI_M3B_DOCKERFILE:-agent/templates/base/Dockerfile}"
TIMEOUT="${UZI_M3B_TIMEOUT:-180}"
NAME="codex-m3b-controls-$$"

log() { printf '\n### %s\n' "$*"; }

if [ "${UZI_M3B_SKIP_BUILD:-}" = "1" ]; then
  log "UZI_M3B_SKIP_BUILD=1 — reusing existing image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi

log "running packaging controls in $IMAGE (writable-root posture, real entrypoint)"
set +e
timeout --kill-after=30s "$TIMEOUT" docker run --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -v "$REPO/e2e":/work/e2e:ro \
  --name "$NAME" \
  "$IMAGE" /bin/sh /work/e2e/codex-m3b/controls.sh
rc=$?
set -e
docker rm -f "$NAME" >/dev/null 2>&1 || true

log "RESULT: rc=$rc (0 = all packaging controls passed; 124 = watchdog fired)"
exit "$rc"
