#!/usr/bin/env bash
# Opt-in Issue #1685 worker-image PATH fixture. Build and fixture are separate steps.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
image="${PROVISION_SPLIT_IMAGE:-uzi-agent-provision-path-split:base}"
action="${1:-fixture}"
name="provision-split-$$"

if ! command -v docker >/dev/null 2>&1 || ! timeout 10s docker info >/dev/null 2>&1; then
  echo "SKIP: Docker CLI or daemon is unavailable"
  exit 77
fi

case "$action" in
  build)
    DOCKER_BUILDKIT=1 timeout --kill-after=10s "${PROVISION_SPLIT_BUILD_TIMEOUT:-600}" docker build -f "$repo/agent/templates/base/Dockerfile" -t "$image" \
      --build-arg "UZI_SRC_SHA=$(git -c safe.directory="$repo" -C "$repo" rev-parse HEAD)" "$repo"
    ;;
  fixture)
    if ! timeout 10s docker image inspect "$image" >/dev/null 2>&1; then
      echo "SKIP: image $image is absent; run task test:provision-path-split:build"
      exit 77
    fi
    source_args=()
    if [ "${PROVISION_SPLIT_MOUNT_SRC:-0}" = 1 ]; then
      source_args=(-v "$repo/agent/src:/app/src:ro" -e PROVISION_SPLIT_SRC=/app/src)
    fi
    set +e
    timeout --kill-after=10s "${PROVISION_SPLIT_TIMEOUT:-120}" docker run --rm --network none \
      --cap-drop ALL \
      --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
      --security-opt no-new-privileges --tmpfs /data \
      --entrypoint /usr/local/sbin/uzi-entrypoint \
      -v "$here:/work/provision-path-split:ro" "${source_args[@]}" \
      --name "$name" "$image" /bin/sh -c \
      'cd /app && exec /usr/local/bin/node --import tsx /work/provision-path-split/fixture.ts'
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
