#!/usr/bin/env bash
# Worker execution-environment lane (issue #3894, PR1: M0 preflight + M1 focused proofs).
#
# Runs the proofs in e2e/worker-env/worker-env-real.test.mts inside the built worker image, started
# through the real root entrypoint, over the image's REAL Nix store (a docker volume Docker seeds
# from the image; never a tmpfs mask, unlike e2e/worker-uid/run.sh) and with anonymous network for
# tool provisioning. No secret, token or credential enters the container: it gets no host
# environment, and the repository is mounted read-only from a checkout that kept no credentials.
#
# Fail closed: a docker failure, a deadline, or any proof that did not execute and pass fails the
# lane (check.py compares the JUnit report against proofs.json).
#
# Every Docker resource is journaled before creation and removed by e2e/worker-uid/bounded.py after
# an ownership-label check, exactly as e2e/worker-uid/run.sh does. Resources are named outside the
# `uzi-` namespace (repository rule: throwaway names never glob the dev stack).
#
# Time budget, every step bounded: image metadata 5s, volume 20s, Nix seed 120s, proofs 600s
# (WORKER_ENV_TIMEOUT), cleanup 30s: under 800s worst case, so the existing worker-uid job
# (~785s measured) keeps its 30 minute cap with margin. Each phase prints its elapsed time.
set -euo pipefail
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
HARNESS="$REPO/e2e/worker-env"
BOUNDED="$REPO/e2e/worker-uid/bounded.py"
IMAGE="${WORKER_ENV_IMAGE:?set WORKER_ENV_IMAGE to an existing local worker image tag (never pulled)}"
PROOF_SECONDS="${WORKER_ENV_TIMEOUT:-600}"
TOKEN="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
NAME="wenv-3894-$TOKEN"
VOLUME="$NAME-nix"
LABEL="io.uzi.worker-uid.invocation=$TOKEN"
LANE_STARTED=$SECONDS
mkdir -p "$REPO/.uzi/scratch"
REPORT_DIR="$(mktemp -d "$REPO/.uzi/scratch/worker-env.XXXXXX")"
cleanup() {
  local rc=$? cleanup_rc=0
  python3 "$BOUNDED" cleanup "$REPORT_DIR" "$TOKEN" || cleanup_rc=$?
  if [ "$cleanup_rc" -ne 0 ]; then
    printf 'worker env cleanup exit: %s\n' "$cleanup_rc" >&2
    exit 1
  fi
  exit "$rc"
}
trap cleanup EXIT
owned_request() {
  printf '%s %s\n' "$1" "$2" >> "$REPORT_DIR/attempted"
}
phase() {
  local label=$1 started=$SECONDS rc=0
  shift
  printf 'worker env phase start: %s\n' "$label" >&2
  "$@" || rc=$?
  printf 'worker env phase end: %s elapsed=%ss exit=%s\n' "$label" "$((SECONDS-started))" "$rc" >&2
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
    printf 'worker env deadline: phase=%s\n' "$label" >&2
  fi
  return "$rc"
}
cd "$REPO"

# M0 (host side): the image identity the proofs run against. The tag must already exist locally.
IMAGE_ID="$(phase image-identity timeout --kill-after=5s 5s docker image inspect --format '{{.Id}}' "$IMAGE")"
if ! [[ "$IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  printf 'worker env invalid image identity for %s\n' "$IMAGE" >&2
  exit 1
fi
printf 'worker env image: %s id=%s\n' "$IMAGE" "$IMAGE_ID" >&2

# The image's real /nix in an invocation-owned volume. Docker seeds an empty named volume from the
# image content at its mount point on first use; that copy is the Nix store initialisation cost,
# measured by its own phase. The proof container then starts through the entrypoint, which
# re-owns the store for the runner uid, exactly as it does for a fresh worker volume.
owned_request volume "$VOLUME"
phase nix-volume-create timeout --kill-after=5s 15s docker volume create --label "$LABEL" "$VOLUME" > /dev/null
phase nix-volume-owner python3 "$BOUNDED" verify-volume "$REPORT_DIR" "$TOKEN" "$VOLUME"
owned_request container "$NAME-seed"
phase nix-seed timeout --kill-after=5s 115s docker run --pull never --network none \
  --label "$LABEL" --name "$NAME-seed" --user 0:0 --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=volume,src=$VOLUME,dst=/nix" --entrypoint /bin/true "$IMAGE_ID"

# The proof container: the production capability set of the root-start worker proof, the seeded
# real /nix, a tmpfs /data (state only; the store is on the volume), default bridge network for the
# anonymous Nix/Go fetches. The repository is read-only at /src; the test copies it as the runner.
rc=0
owned_request container "$NAME"
set +e
phase worker-env-proof python3 "$BOUNDED" capture "$REPORT_DIR" "$TOKEN" "$REPORT_DIR/results.xml" "$((PROOF_SECONDS + 5))" \
  timeout --kill-after=5s "$PROOF_SECONDS" docker run --pull never \
  --label "$LABEL" --name "$NAME" --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --mount "type=volume,src=$VOLUME,dst=/nix" --tmpfs /data \
  --workdir /app/uzi-worker-uid-proof \
  --tmpfs /app/uzi-worker-uid-proof:exec,uid=10001,gid=10001,mode=0755 \
  --env "WORKER_ENV_IMAGE_ID=$IMAGE_ID" \
  --mount "type=bind,src=$REPO,dst=/src,readonly" \
  --mount "type=bind,src=$REPO/e2e/worker-env,dst=/app/uzi-worker-uid-proof/e2e/worker-env,readonly" \
  --mount "type=bind,src=$REPO/agent/src,dst=/app/uzi-worker-uid-proof/agent/src,readonly" \
  --mount "type=bind,src=$REPO/agent/test,dst=/app/uzi-worker-uid-proof/agent/test,readonly" \
  --mount "type=bind,src=$REPO/agent/templates,dst=/app/uzi-worker-uid-proof/agent/templates,readonly" \
  --mount "type=bind,src=$REPO/controller/internal/kube/testdata/runner-shared-init.sh,dst=/app/uzi-worker-uid-proof/controller/internal/kube/testdata/runner-shared-init.sh,readonly" \
  --entrypoint /usr/local/sbin/uzi-entrypoint "$IMAGE_ID" \
  /usr/local/bin/node --import /app/node_modules/tsx/dist/loader.mjs \
  --import /app/uzi-worker-uid-proof/agent/test/setup/hermetic-proc.ts \
  --test --test-concurrency=1 --test-timeout=350000 --test-reporter=junit \
  /app/uzi-worker-uid-proof/e2e/worker-env/worker-env-real.test.mts
rc=$?
set -e
printf 'worker env reports: %s\n' "$REPORT_DIR" >&2
checker_rc=0
python3 "$HARNESS/check.py" "$HARNESS/proofs.json" "$REPORT_DIR/results.xml" || checker_rc=$?
if [ "$rc" -ne 0 ]; then
  printf 'worker env node/container exit: %s\n' "$rc" >&2
fi
printf 'worker env lane elapsed: %ss\n' "$((SECONDS-LANE_STARTED))" >&2
if [ "$rc" -ne 0 ] || [ "$checker_rc" -ne 0 ]; then
  exit 1
fi
