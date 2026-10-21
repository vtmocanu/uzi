#!/usr/bin/env bash
# #3148: the packaged Codex O proof for ONE worker template, on ONE image, in strict mode.
#
# The M3a/M3b packaged harnesses only run when a maintainer starts them, so they rotted for
# weeks between the PRs that needed them. This is the single entry point the scheduled
# workflow (.github/workflows/codex-packaged-proof.yml) and a maintainer both call:
#
#   1. Build the template's real worker image once (or reuse a prebuilt one) and record its
#      immutable local image ID.
#   2. Require Landlock to be AVAILABLE inside that image under the confinement posture
#      (`uzi-codex-command-sandbox --probe` exits 0). The probe only queries the ABI; the
#      enforcement proof is the M3b `required`-mode command-root test, which runs below.
#      Host `/sys/kernel/security/lsm` is printed as a diagnostic, never as the gate.
#   3. Run every leg on that same image with UZI_*_SKIP_BUILD=1, in strict mode (no skipped,
#      todo or missing test passes): the M3b packaged lifecycle, the M3a profile controls
#      (main + nnp, each with PASS > 0 and FAIL = 0), and the four M3a confinement suites.
#      Every leg runs even after an earlier one fails, so one run reports all of them.
#   4. Re-read the image ID; it must equal step 1's.
#
# Outputs in $OUT: one log per leg, the strict TAP streams, status.json (once the legs have run;
# an early build or image-inspection failure exits before it) and, only
# when every check passed, receipt.json plus a CANDIDATE_RECEIPT line on stdout. A locally
# loaded image has no registry digest, so the receipt records the image ID and never invents
# a digest.
#
#   ./run-proof.sh <base|jvm>
#
# Environment:
#   UZI_CODEX_PROOF_IMAGE       image tag              (default: cdr-codex-proof-<template>:local)
#   UZI_CODEX_PROOF_SKIP_BUILD  1 = reuse an existing image (the workflow builds it with buildx)
#   UZI_CODEX_PROOF_OUT         output directory       (default: e2e/codex-proof/out-<template>)
#   UZI_CODEX_PROOF_COMMIT      checked-out commit recorded in the receipt (default: git HEAD)
#   UZI_CODEX_PROOF_HEAD_SHA    PR head commit, recorded beside a PR merge commit (optional)
#
# Throwaway names stay outside the uzi- namespace and are removed by exact name (root
# CLAUDE.md, "Destructive operations"); this script never removes the image it was given.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"

TEMPLATE="${1:-}"
case "$TEMPLATE" in
  base|jvm) ;;
  *) echo "usage: $0 <base|jvm>" >&2; exit 2 ;;
esac
DOCKERFILE="agent/templates/${TEMPLATE}/Dockerfile"
IMAGE="${UZI_CODEX_PROOF_IMAGE:-cdr-codex-proof-${TEMPLATE}:local}"
OUT="${UZI_CODEX_PROOF_OUT:-$HERE/out-${TEMPLATE}}"
COMMIT="${UZI_CODEX_PROOF_COMMIT:-$(git -C "$REPO" rev-parse HEAD)}"
HEAD_SHA="${UZI_CODEX_PROOF_HEAD_SHA:-}"
PROBE_NAME="cdr-codex-proof-probe-$$"

case "$COMMIT" in
  *[!0-9a-f]*|"") echo "run-proof: commit '$COMMIT' is not lowercase hex" >&2; exit 2 ;;
esac
case "$HEAD_SHA" in
  *[!0-9a-f]*) echo "run-proof: head sha '$HEAD_SHA' is not lowercase hex" >&2; exit 2 ;;
esac

mkdir -p "$OUT"
rm -f "$OUT/status.json" "$OUT/receipt.json"

log() { printf '\n### run-proof[%s]: %s\n' "$TEMPLATE" "$*"; }

cleanup() {
  docker rm -f "$PROBE_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

image_id() { docker image inspect --format '{{.Id}}' "$IMAGE"; }

# 1. One image for every leg.
if [ "${UZI_CODEX_PROOF_SKIP_BUILD:-}" = "1" ]; then
  log "reusing prebuilt image $IMAGE"
else
  log "building $IMAGE from $DOCKERFILE"
  DOCKER_BUILDKIT=1 docker build -f "$REPO/$DOCKERFILE" -t "$IMAGE" \
    --build-arg "UZI_SRC_SHA=$COMMIT" "$REPO"
fi
ID_BEFORE="$(image_id)"
case "$ID_BEFORE" in
  sha256:*) ;;
  *) log "FAILURE: image $IMAGE has no local image ID (${ID_BEFORE:-none})"; exit 1 ;;
esac
log "image $IMAGE id $ID_BEFORE"

# 2. Landlock availability in the confinement posture (diagnostics first, never gating).
log "host kernel $(uname -r); LSMs: $(cat /sys/kernel/security/lsm 2>/dev/null || echo unreadable)"
set +e
timeout --kill-after=10s 120 docker run --rm --network none \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /nix:exec --tmpfs /data --tmpfs /tmp:exec \
  --tmpfs /var/cache/uzi-codex-cmd:rw,exec,nosuid,nodev,mode=0700 \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  --name "$PROBE_NAME" \
  "$IMAGE" /usr/local/bin/uzi-codex-command-sandbox --probe > "$OUT/landlock-probe.log" 2>&1
rc_probe=$?
set -e
case "$rc_probe" in
  0) log "Landlock available in the image (probe exit 0)" ;;
  10) log "FAILURE: Landlock UNAVAILABLE in the image (probe exit 10: ENOSYS/EOPNOTSUPP)" ;;
  11) log "FAILURE: Landlock probe error in the image (probe exit 11)" ;;
  *) log "FAILURE: Landlock probe could not run (exit $rc_probe)"; tail -n 20 "$OUT/landlock-probe.log" || true ;;
esac

# 3. The legs, all on the same image, all strict.
rc_m3b=0
log "leg m3b: packaged lifecycle"
UZI_M3B_IMAGE="$IMAGE" UZI_M3B_DOCKERFILE="$DOCKERFILE" UZI_M3B_SKIP_BUILD=1 \
  UZI_CODEX_PROOF_STRICT=1 UZI_CODEX_PROOF_TAP="$OUT/m3b.tap" \
  "$REPO/e2e/codex-m3b/run-lifecycle.sh" > "$OUT/m3b.log" 2>&1 || rc_m3b=$?
tail -n 5 "$OUT/m3b.log" || true

rc_profile=0
log "leg m3a-profile: main + nnp controls"
UZI_M3A_IMAGE="$IMAGE" UZI_M3A_DOCKERFILE="$DOCKERFILE" UZI_M3A_SKIP_BUILD=1 \
  "$REPO/e2e/codex-m3a/run.sh" all > "$OUT/m3a-profile.log" 2>&1 || rc_profile=$?
# Each controls container prints `SUMMARY mode=<m> PASS=<n> FAIL=<n>`; a mode that never
# reported, reported zero passes or any failure fails the leg even when run.sh exited 0.
for mode in main nnp; do
  if ! awk -v mode="$mode" '
      $0 ~ ("SUMMARY mode=" mode " PASS=") {
        for (i = 1; i <= NF; i++) {
          if ($i ~ /^PASS=/) { p = substr($i, 6) }
          if ($i ~ /^FAIL=/) { f = substr($i, 6) }
        }
        seen = 1
      }
      END { exit !(seen && p ~ /^[0-9]+$/ && p + 0 > 0 && f == "0") }' "$OUT/m3a-profile.log"; then
    log "FAILURE: m3a profile mode $mode has no clean SUMMARY (PASS > 0, FAIL = 0)"
    [ "$rc_profile" -ne 0 ] || rc_profile=1
  fi
done
tail -n 5 "$OUT/m3a-profile.log" || true

rc_m3a=0
log "leg m3a-lifecycle: four confinement suites"
UZI_M3A_IMAGE="$IMAGE" UZI_M3A_DOCKERFILE="$DOCKERFILE" UZI_M3A_SKIP_BUILD=1 \
  UZI_CODEX_PROOF_STRICT=1 UZI_CODEX_PROOF_TAP="$OUT/m3a-lifecycle.tap" \
  "$REPO/e2e/codex-m3a/run-lifecycle.sh" > "$OUT/m3a-lifecycle.log" 2>&1 || rc_m3a=$?
tail -n 5 "$OUT/m3a-lifecycle.log" || true

# 4. Identity: every leg ran on the image recorded in step 1.
ID_AFTER="$(image_id 2>/dev/null || echo missing)"
rc_identity=0
if [ "$ID_AFTER" != "$ID_BEFORE" ]; then
  log "FAILURE: image $IMAGE changed during the proof ($ID_BEFORE -> $ID_AFTER)"
  rc_identity=1
fi

status=fail
if [ "$rc_probe" -eq 0 ] && [ "$rc_m3b" -eq 0 ] && [ "$rc_profile" -eq 0 ] \
   && [ "$rc_m3a" -eq 0 ] && [ "$rc_identity" -eq 0 ]; then
  status=pass
fi

# Every value below is a validated hex string, an integer or a fixed word, so printf is safe.
printf '{"template":"%s","status":"%s","commit":"%s","head_sha":"%s","image_id":"%s","landlock_probe":%d,"m3b":%d,"m3a_profile":%d,"m3a_lifecycle":%d,"identity":%d}\n' \
  "$TEMPLATE" "$status" "$COMMIT" "$HEAD_SHA" "$ID_BEFORE" \
  "$rc_probe" "$rc_m3b" "$rc_profile" "$rc_m3a" "$rc_identity" > "$OUT/status.json"
log "status: $(cat "$OUT/status.json")"

if [ "$status" != pass ]; then
  exit 1
fi
printf '{"template":"%s","commit":"%s","head_sha":"%s","image_id":"%s"}\n' \
  "$TEMPLATE" "$COMMIT" "$HEAD_SHA" "$ID_BEFORE" > "$OUT/receipt.json"
printf 'CANDIDATE_RECEIPT template=%s commit=%s head_sha=%s image_id=%s\n' \
  "$TEMPLATE" "$COMMIT" "${HEAD_SHA:-none}" "$ID_BEFORE"
