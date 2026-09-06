#!/usr/bin/env bash
# PRD #1156 M3a — host-side orchestrator for the image-profile primitive validation.
#
# Builds the tiny static Go stub child, (optionally) builds the REAL worker image with the
# M2 launcher/supervisor changes, then runs the five container controls INSIDE that image
# through its real root entrypoint under compose's security options — writable root fs, NO
# --read-only, NO --user override — so the on-disk ownership and uid-split the controls
# prove are the true production posture. See README.md.
#
#   ./run.sh [all|main|nnp]        which container(s) to run (default: all)
#
# Environment:
#   UZI_M3A_IMAGE       image tag to build/run           (default: uzi-agent-m3a:base)
#   UZI_M3A_DOCKERFILE  Dockerfile to build              (default: agent/templates/base/Dockerfile)
#   UZI_M3A_SKIP_BUILD  set to 1 to reuse an existing image (skip docker build)
#   UZI_M3A_TIMEOUT     per-container wall timeout (s)    (default: 360)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${UZI_M3A_IMAGE:-uzi-agent-m3a:base}"
DOCKERFILE="${UZI_M3A_DOCKERFILE:-agent/templates/base/Dockerfile}"
WHICH="${1:-all}"
TIMEOUT="${UZI_M3A_TIMEOUT:-360}"

log() { printf '\n### %s\n' "$*"; }

# 1. Build the static stub child from committed source (never committed as a binary).
log "building stub child (static, host toolchain)"
mkdir -p "$HERE/.bin"
( cd "$HERE/stub" && GOTOOLCHAIN=local CGO_ENABLED=0 GOPROXY=off go build -trimpath -o "$HERE/.bin/stub-child" . )
chmod 0755 "$HERE/.bin/stub-child"
chmod 0755 "$HERE/controls.sh" "$HERE/run.sh" 2>/dev/null || true
file "$HERE/.bin/stub-child" || true

# 2. Build the real worker image (unless reusing one).
if [ "${UZI_M3A_SKIP_BUILD:-}" = "1" ]; then
  log "UZI_M3A_SKIP_BUILD=1 — reusing existing image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (this can take several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi

# 3. Common hardened run options: ROOT start via the real entrypoint, compose cap set, no
#    --read-only and no --user (those would MASK the real ownership these controls prove).
COMMON=(
  --rm --network none
  --cap-drop ALL
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID
  --entrypoint /usr/local/sbin/uzi-entrypoint
  -v "$HERE":/m3a:ro
)

run_container() {
  # $1 = mode (main|nnp), $2.. = extra docker args
  mode="$1"; shift
  name="codex-m3a-$mode-$$"
  log "container [$mode]: docker run ${*}"
  set +e
  timeout "$TIMEOUT" docker run "${COMMON[@]}" "$@" --name "$name" "$IMAGE" /bin/sh /m3a/controls.sh "$mode"
  rc=$?
  set -e
  docker rm -f "$name" >/dev/null 2>&1 || true
  return "$rc"
}

RC_MAIN=0
RC_NNP=0

if [ "$WHICH" = "all" ] || [ "$WHICH" = "main" ]; then
  # The mandatory hardened container: no-new-privileges ON (the supported production posture).
  run_container main --security-opt no-new-privileges || RC_MAIN=$?
fi
if [ "$WHICH" = "all" ] || [ "$WHICH" = "nnp" ]; then
  # The negative-posture variant: no-new-privileges OFF, to prove the fail-closed check.
  run_container nnp || RC_NNP=$?
fi

log "RESULT: main=$RC_MAIN nnp=$RC_NNP (0 = all controls passed)"
[ "$RC_MAIN" -eq 0 ] && [ "$RC_NNP" -eq 0 ]
