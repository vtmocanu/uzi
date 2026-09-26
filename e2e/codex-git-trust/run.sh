#!/usr/bin/env bash
# Opt-in Issue #1716 worker-image fixture: git in the run's checkout from a real Codex command
# root (runner-cmd under the supervisor + command sandbox). Build and fixture are separate steps.
# Exit 77 is SKIP (no Docker, no image, or no uid split), never a pass.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
image="${CODEX_GIT_TRUST_IMAGE:-uzi-agent-codex-git-trust:base}"
action="${1:-fixture}"
name="codex-git-trust-$$"

if ! command -v docker >/dev/null 2>&1 || ! timeout 10s docker info >/dev/null 2>&1; then
  echo "SKIP: Docker CLI or daemon is unavailable"
  exit 77
fi

case "$action" in
  build)
    DOCKER_BUILDKIT=1 timeout --kill-after=10s "${CODEX_GIT_TRUST_BUILD_TIMEOUT:-600}" docker build -f "$repo/agent/templates/base/Dockerfile" -t "$image" \
      --build-arg "UZI_SRC_SHA=$(git -c safe.directory="$repo" -C "$repo" rev-parse HEAD)" "$repo"
    ;;
  fixture)
    if ! timeout 10s docker image inspect "$image" >/dev/null 2>&1; then
      echo "SKIP: image $image is absent; run task test:codex-git-trust:build"
      exit 77
    fi
    # CODEX_GIT_TRUST_SRC_DIR overrides the mounted source tree (e.g. a base-commit worktree's
    # agent/src for the before run); it implies CODEX_GIT_TRUST_MOUNT_SRC=1.
    src_dir="${CODEX_GIT_TRUST_SRC_DIR:-$repo/agent/src}"
    source_args=()
    if [ "${CODEX_GIT_TRUST_MOUNT_SRC:-0}" = 1 ] || [ -n "${CODEX_GIT_TRUST_SRC_DIR:-}" ]; then
      source_args=(-v "$src_dir:/app/src:ro" -e CODEX_GIT_TRUST_SRC=/app/src)
    fi
    # CODEX_GIT_TRUST_BIN_DIR mounts this tree's statically built uzi-codex-supervisor,
    # uzi-codex-fileop and uzi-codex-command-sandbox over an OLDER image's copies, whose launch
    # protocol can lag a mounted agent/src. The image's base layers (git, node) stay the image's.
    if [ -n "${CODEX_GIT_TRUST_BIN_DIR:-}" ]; then
      for bin in uzi-codex-supervisor uzi-codex-fileop uzi-codex-command-sandbox; do
        source_args+=(-v "$CODEX_GIT_TRUST_BIN_DIR/$bin:/usr/local/bin/$bin:ro")
      done
    fi
    set +e
    timeout --kill-after=10s "${CODEX_GIT_TRUST_TIMEOUT:-180}" docker run --rm --network none \
      --cap-drop ALL \
      --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
      --security-opt no-new-privileges --tmpfs /data \
      --entrypoint /usr/local/sbin/uzi-entrypoint \
      -v "$here:/work/codex-git-trust:ro" "${source_args[@]}" \
      --name "$name" "$image" /bin/sh -c \
      'cd /app && exec /usr/local/bin/node --import tsx /work/codex-git-trust/fixture.ts'
    rc=$?
    set -e
    timeout --kill-after=2s 10s docker rm -f "$name" >/dev/null 2>&1 || true
    if [ "$rc" -eq 77 ]; then
      echo "SKIP: image entrypoint did not establish uid split"
      exit 77
    fi
    exit "$rc"
    ;;
  *)
    echo "usage: $0 {build|fixture}" >&2
    exit 2
    ;;
esac
