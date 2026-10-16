#!/usr/bin/env bash
# Disposable #3074 experiment. No compose, paid provider, native shell authority,
# image build, production credentials or execution-isolation relaxation.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
IMAGE=ghcr.io/vtmocanu/uzi/agent-base@sha256:ac273a81c9c6ec1e79c046e31fd2627be1c9a472a958e05f182b679bfe17b3da
NAME="aud-l02-${GITHUB_RUN_ID:-local}-$$"
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT
[ "$(uname -m)" = x86_64 ] || { echo 'native amd64 Linux required' >&2; exit 2; }
[ "$(uname -s)" = Linux ] || { echo 'native amd64 Linux required' >&2; exit 2; }
docker pull "$IMAGE"
# Match the M3a confinement posture. rc.17 additionally needs its command-cache
# tmpfs so entrypoint can establish the cache's uid-10003 ownership.
timeout --kill-after=5s 420 docker run --pull=never --name "$NAME" --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --read-only \
  --tmpfs /nix:exec --tmpfs /data --tmpfs /tmp:exec --tmpfs /var/cache/uzi-codex-cmd \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  --mount "type=bind,src=$ROOT/e2e,dst=/work/e2e,readonly" \
  --mount "type=bind,src=$ROOT/e2e/codex-m3a/l02-spike,dst=/work/spike,readonly" \
  "$IMAGE" /bin/sh -c 'cd /app && /opt/uzi-codex/0.160.1/bin/codex --version &&
    /usr/local/bin/node --import tsx /work/spike/measure.ts &&
    /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=120000 /work/e2e/codex-m3a/lifecycle.test.ts'
