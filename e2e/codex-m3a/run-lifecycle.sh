#!/usr/bin/env bash
# PRD #1156 M3a — host orchestrator for the credential-free real-code-mode-host LIFECYCLE
# + ISOLATION suites, run under the PRD CONFINEMENT posture.
#
# This is the sibling of run.sh (which runs the shell profile controls under the
# writable-root immutability posture). Here the four TypeScript suites
# (lifecycle.test.ts / production-launcher.test.ts / isolation.test.ts /
# code-mode-host-run-posture.test.ts) run INSIDE the real worker image through its real
# root-start entrypoint, but under the PRD confinement posture that is DISTINCT from run.sh's:
#
#   * read-only root filesystem (`--read-only`), read-only mounted fixtures,
#   * exactly three explicit writable runtime mounts: runner-owned /nix, owned /data and
#     per-uid /tmp (the entrypoint chowns/creates all three while the image root stays ro),
#     plus the worker's dedicated command-cache carveout /var/cache/uzi-codex-cmd, which the
#     entrypoint chowns to runner-cmd since #1619 (the same mount the m3b runner gained in #3114),
#   * `--network none` (loopback only, for the localhost fake provider),
#   * `--cap-drop ALL` + only the startup caps the A1 entrypoint needs, no-new-privileges.
#
# The suites run as the WORKER uid (10001) after the entrypoint establishes the split, and
# drive the runner uid (10002) via the production setpriv wrapper — exactly as the launcher
# does. Node's own --test-timeout is not enough (a leaked codex/host handle can outlive it),
# so the docker run is bounded by an OUTER `timeout --kill-after` watchdog; --rm tears the
# whole container (and any leaked descendant) down on kill.
#
#   ./run-lifecycle.sh                 build (unless skipped) + run all four suites
#
# Environment:
#   UZI_M3A_IMAGE       image tag to build/run          (default: uzi-agent-m3a:base)
#   UZI_M3A_DOCKERFILE  Dockerfile to build             (default: agent/templates/base/Dockerfile)
#   UZI_M3A_SKIP_BUILD  set to 1 to reuse an existing image (skip docker build)
#   UZI_M3A_TEST_TIMEOUT  outer watchdog seconds        (default: 600)
#   UZI_CODEX_PROOF_STRICT  set to 1 to also fail on any skipped, todo or missing test: node
#                         --test passes a skipped test or describe with rc 0, so the scheduled
#                         proof (#3148) adds a TAP stream checked by e2e/codex-proof/check-node-tap.mjs
#   UZI_CODEX_PROOF_TAP   strict mode's TAP file          (default: lifecycle-tap.<pid>.log here)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${UZI_M3A_IMAGE:-uzi-agent-m3a:base}"
DOCKERFILE="${UZI_M3A_DOCKERFILE:-agent/templates/base/Dockerfile}"
TIMEOUT="${UZI_M3A_TEST_TIMEOUT:-600}"
NAME="codex-m3a-lifecycle-$$"
STRICT="${UZI_CODEX_PROOF_STRICT:-}"
TAP_OUT="${UZI_CODEX_PROOF_TAP:-$HERE/lifecycle-tap.$$.log}"
REPORTERS=""
if [ "$STRICT" = "1" ]; then
  # spec stays the human-readable stdout; TAP goes to stderr, which the host captures.
  REPORTERS="--test-reporter=spec --test-reporter-destination=stdout --test-reporter=tap --test-reporter-destination=stderr"
fi

log() { printf '\n### %s\n' "$*"; }

# 1. Build the real worker image (unless reusing one).
if [ "${UZI_M3A_SKIP_BUILD:-}" = "1" ]; then
  log "UZI_M3A_SKIP_BUILD=1 — reusing existing image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi

# 2. Run the four suites under the CONFINEMENT posture. The tests + frozen M0 protocol
#    helpers are mounted read-only under /work/e2e. Controls B/C and the run-posture suite
#    dynamically import the IMAGE-BAKED production code from /app/src (the run-posture suite
#    drives the REAL launchCodexRoot + config.ts builder with codeModeHost on/off); control A
#    drives installed supervisor/Codex.
#    Type-only source imports are erased and cannot substitute the host tree at runtime.
log "running lifecycle + isolation suites in $IMAGE under the confinement posture"
# The single-quoted command is expanded by the inner container shell, not this host shell;
# UZI_TEST_REPORTERS is empty unless strict mode is on.
# shellcheck disable=SC2016
run_suites() {
  timeout --kill-after=60s "$TIMEOUT" docker run --rm --network none \
    --cap-drop ALL \
    --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
    --security-opt no-new-privileges \
    --read-only \
    --tmpfs /nix:exec --tmpfs /data --tmpfs /tmp:exec \
    --tmpfs /var/cache/uzi-codex-cmd:rw,exec,nosuid,nodev,mode=0700 \
    --entrypoint /usr/local/sbin/uzi-entrypoint \
    --mount "type=bind,src=$REPO/e2e,dst=/work/e2e,readonly" \
    -e "UZI_TEST_REPORTERS=$REPORTERS" \
    --name "$NAME" \
    "$IMAGE" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=120000 \
      ${UZI_TEST_REPORTERS:-} \
      /work/e2e/codex-m3a/lifecycle.test.ts \
      /work/e2e/codex-m3a/production-launcher.test.ts \
      /work/e2e/codex-m3a/isolation.test.ts \
      /work/e2e/codex-m3a/code-mode-host-run-posture.test.ts'
}
set +e
if [ "$STRICT" = "1" ]; then
  run_suites 2>"$TAP_OUT"
else
  run_suites
fi
rc=$?
set -e
# Best-effort removal of exactly this named container (never a broad sweep).
docker rm -f "$NAME" >/dev/null 2>&1 || true

rc_strict=0
if [ "$STRICT" = "1" ]; then
  log "strict mode: checking the TAP stream $TAP_OUT"
  node "$REPO/e2e/codex-proof/check-node-tap.mjs" "$TAP_OUT" || rc_strict=$?
  if [ "$rc" -ne 0 ] || [ "$rc_strict" -ne 0 ]; then
    log "last TAP/stderr lines"
    tail -n 40 "$TAP_OUT" || true
  fi
fi

log "RESULT: rc=$rc strict=$rc_strict (0/0 = all suites passed; 124 = outer watchdog fired)"
[ "$rc" -eq 0 ] || exit "$rc"
exit "$rc_strict"
