#!/usr/bin/env bash
# PRD #1171 m5 — host orchestrator for the PACKAGED DARK CodexExecutor lifecycle proof.
#
# It is the m3b sibling of e2e/codex-m3a/run-lifecycle.sh. It builds the REAL worker image
# (with the m5 supervisor+fileop install), then runs TWO legs INSIDE that image:
#
#   1. PACKAGING controls (controls.sh) through the REAL root entrypoint under the writable-root
#      posture, so the on-disk ownership it proves (supervisor + fileop root-owned 0555) is the
#      true production posture.
#   2. The LIFECYCLE suite (lifecycle.test.ts) under the PRD CONFINEMENT posture — read-only
#      root filesystem, tmpfs runtime mounts (/nix, /data, /tmp), `--network none` (loopback
#      only, for the localhost fake provider), `--cap-drop ALL` + only the entrypoint's startup
#      caps + no-new-privileges. CODEX_M3B_PACKAGED=1 enables the real-launch Block B; the whole
#      docker run is bounded by an OUTER `timeout --kill-after` watchdog because a leaked codex /
#      supervisor handle can outlive Node's own --test-timeout, and --rm tears the container
#      (and any leaked descendant) down on kill.
#
# CI/MAINTAINER-ONLY: in-worker image builds are storage-flaky and arm64 is blocked, so this is
# never run in-worker — the host `tsc` typecheck and the host `node --test` Block A run are.
#
#   ./run-lifecycle.sh                 build (unless skipped) + run both legs
#
# Environment:
#   UZI_M3B_IMAGE       image tag to build/run          (default: uzi-agent-m3b:base)
#   UZI_M3B_DOCKERFILE  Dockerfile to build             (default: agent/templates/base/Dockerfile)
#   UZI_M3B_SKIP_BUILD  set to 1 to reuse an existing image (skip docker build)
#   UZI_M3B_TEST_TIMEOUT  outer watchdog seconds        (default: 600)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${UZI_M3B_IMAGE:-uzi-agent-m3b:base}"
DOCKERFILE="${UZI_M3B_DOCKERFILE:-agent/templates/base/Dockerfile}"
TIMEOUT="${UZI_M3B_TEST_TIMEOUT:-600}"
CONTROLS_NAME="codex-m3b-controls-$$"
LIFECYCLE_NAME="codex-m3b-lifecycle-$$"

log() { printf '\n### %s\n' "$*"; }

# 1. Build the real worker image (unless reusing one).
if [ "${UZI_M3B_SKIP_BUILD:-}" = "1" ]; then
  log "UZI_M3B_SKIP_BUILD=1 — reusing existing image $IMAGE"
else
  log "building worker image $IMAGE from $DOCKERFILE (several minutes on a cold cache)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)" "$REPO"
fi

# 2. Packaging controls — writable-root posture, ROOT start via the real entrypoint (mirrors
#    codex-m3a/run.sh's COMMON cap set). Proves the baked supervisor + fileop ownership/mode.
log "running packaging controls in $IMAGE (writable-root posture, real entrypoint)"
set +e
timeout --kill-after=30s 180 docker run --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -v "$REPO/e2e":/work/e2e:ro \
  --name "$CONTROLS_NAME" \
  "$IMAGE" /bin/sh /work/e2e/codex-m3b/controls.sh
rc_controls=$?
set -e
docker rm -f "$CONTROLS_NAME" >/dev/null 2>&1 || true
log "packaging controls rc=$rc_controls"

# 3. The lifecycle suite — CONFINEMENT posture. CODEX_M3B_SRC pins the packaged /app/src;
#    CODEX_M3B_PACKAGED=1 turns on the real-launch Block B. Output is captured so the
#    per-image positive-count assertion can read the suite's own summary line.
log "running the packaged lifecycle suite in $IMAGE under the confinement posture"
LIFECYCLE_OUT="$HERE/lifecycle-run.$$.log"
set +e
timeout --kill-after=60s "$TIMEOUT" docker run --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /nix:exec --tmpfs /data --tmpfs /tmp:exec \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  -e CODEX_M3B_PACKAGED=1 -e CODEX_M3B_SRC=/app/src -e UZI_R2_COMMAND_ROOT_LINUX=1 \
  -v "$REPO/e2e":/work/e2e:ro \
  -v "$REPO/agent/test":/app/test:ro \
  --name "$LIFECYCLE_NAME" \
  "$IMAGE" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx --test --test-concurrency=1 --test-timeout=120000 \
    /app/test/codex-command-root-linux.test.ts /work/e2e/codex-m3b/lifecycle.test.ts' 2>&1 | tee "$LIFECYCLE_OUT"
rc_lifecycle=${PIPESTATUS[0]}
set -e
docker rm -f "$LIFECYCLE_NAME" >/dev/null 2>&1 || true

# 4. Per-image positive-count assertion: the suite prints one CODEX_M3B_COUNTS summary with the
#    tests/callbacks/delegations/roots it exercised; every count MUST be > 0 so a silently-empty
#    image run (no callback ran, no delegation demuxed, no root reaped) fails HERE rather than
#    reading green. Parsed with node (already on the image but also on the host) — never a /nix
#    jq — reading the LAST such line.
counts_line="$(grep 'CODEX_M3B_COUNTS' "$LIFECYCLE_OUT" | tail -1 || true)"
rc_counts=1
if [ -n "$counts_line" ]; then
  json="${counts_line#*CODEX_M3B_COUNTS }"
  if printf '%s' "$json" | node -e '
    let raw = ""; process.stdin.on("data", (d) => (raw += d)); process.stdin.on("end", () => {
      try {
        const c = JSON.parse(raw.trim());
        const ok = ["tests", "callbacks", "delegations", "roots"].every((k) => Number(c[k]) > 0);
        process.exit(ok ? 0 : 1);
      } catch { process.exit(1); }
    });'; then
    rc_counts=0
    log "per-image counts OK: $json"
  else
    log "per-image counts NOT all positive: $json"
  fi
else
  log "no CODEX_M3B_COUNTS summary found in the lifecycle output"
fi
rm -f "$LIFECYCLE_OUT" 2>/dev/null || true

log "RESULT: controls=$rc_controls lifecycle=$rc_lifecycle counts=$rc_counts (0/0/0 = pass; 124 = watchdog)"
[ "$rc_controls" -eq 0 ] && [ "$rc_lifecycle" -eq 0 ] && [ "$rc_counts" -eq 0 ]
