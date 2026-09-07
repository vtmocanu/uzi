#!/usr/bin/env bash
# PRD #1156 M3a — host orchestrator for the credential-free real-code-mode-host LIFECYCLE
# + ISOLATION suites, run under the PRD CONFINEMENT posture.
#
# This is the sibling of run.sh (which runs the shell profile controls under the
# writable-root immutability posture). Here the three TypeScript suites
# (lifecycle.test.ts / production-launcher.test.ts / isolation.test.ts) run INSIDE the
# real worker image through its real root-start entrypoint, but under the PRD confinement
# posture that is DISTINCT from run.sh's:
#
#   * read-only root filesystem (`--read-only`), read-only mounted fixtures,
#   * ONLY explicit owned writable data + tmp mounts: writable tmpfs /nix, /data, /tmp
#     (the entrypoint's root window chowns /nix + /data and creates the per-uid /tmp trees;
#     those three need to be writable while the image root fs stays read-only),
#   * `--network none` (loopback only, for the localhost fake provider),
#   * `--cap-drop ALL` + only the startup caps the A1 entrypoint needs, no-new-privileges.
#
# The suites run as the WORKER uid (10001) after the entrypoint establishes the split, and
# drive the runner uid (10002) via the production setpriv wrapper — exactly as the launcher
# does. Node's own --test-timeout is not enough (a leaked codex/host handle can outlive it),
# so the docker run is bounded by an OUTER `timeout --kill-after` watchdog; --rm tears the
# whole container (and any leaked descendant) down on kill.
#
#   ./run-lifecycle.sh                 build (unless skipped) + run all three suites
#
# Environment:
#   UZI_M3A_IMAGE       image tag to build/run          (default: uzi-agent-m3a:base)
#   UZI_M3A_DOCKERFILE  Dockerfile to build             (default: agent/templates/base/Dockerfile)
#   UZI_M3A_SKIP_BUILD  set to 1 to reuse an existing image (skip docker build)
#   UZI_M3A_TEST_TIMEOUT  outer watchdog seconds        (default: 600)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${UZI_M3A_IMAGE:-uzi-agent-m3a:base}"
DOCKERFILE="${UZI_M3A_DOCKERFILE:-agent/templates/base/Dockerfile}"
TIMEOUT="${UZI_M3A_TEST_TIMEOUT:-600}"
NAME="codex-m3a-lifecycle-$$"

log() { printf '\n### %s\n' "$*"; }

# 1. Build the real worker image (unless reusing one).
if [ "${UZI_M3A_SKIP_BUILD:-}" = "1" ]; then
  log "UZI_M3A_SKIP_BUILD=1 — reusing existing image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi

# 2. Run the three suites under the CONFINEMENT posture. The tests + the frozen M0
#    protocol helpers are mounted read-only under /work/e2e; the ACTUAL packaged launcher
#    source is mounted read-only under /work/agent/src (control B/C import it, control A
#    drives the installed supervisor + codex directly). tsx resolves from /app/node_modules
#    (cwd /app); the test files resolve ../../agent/src/... and ../codex-m0/... under /work.
log "running lifecycle + isolation suites in $IMAGE under the confinement posture"
set +e
timeout --kill-after=60s "$TIMEOUT" docker run --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /nix:exec --tmpfs /data --tmpfs /tmp:exec \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -v "$REPO/e2e":/work/e2e:ro \
  -v "$REPO/agent/src":/work/agent/src:ro \
  --name "$NAME" \
  "$IMAGE" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=120000 \
    /work/e2e/codex-m3a/lifecycle.test.ts \
    /work/e2e/codex-m3a/production-launcher.test.ts \
    /work/e2e/codex-m3a/isolation.test.ts'
rc=$?
set -e
# Best-effort removal of exactly this named container (never a broad sweep).
docker rm -f "$NAME" >/dev/null 2>&1 || true

log "RESULT: rc=$rc (0 = all suites passed; 124 = outer watchdog fired)"
exit "$rc"
