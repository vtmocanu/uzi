#!/usr/bin/env bash
# Run this tree's tests under the real root-start worker entrypoint and cap set.
set -euo pipefail
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="${WORKER_UID_IMAGE:-wuid-2134-base:local}"
NAME="wuid-2134-$$"
COMMAND_NAME="$NAME-command"
DEPS="$NAME-deps"
mkdir -p "$REPO/.uzi/scratch"
REPORT_DIR="$(mktemp -d "$REPO/.uzi/scratch/worker-uid.XXXXXX")"
cleanup() {
  local rc=$? cleanup_rc=0
  # Two calls, each at most 15 seconds including kill grace; try both on failure.
  timeout --kill-after=5s 10s docker rm -f "$NAME" "$COMMAND_NAME" "$NAME-provision" >/dev/null 2>&1 || cleanup_rc=$?
  timeout --kill-after=5s 10s docker volume rm "$DEPS" >/dev/null 2>&1 || cleanup_rc=$?
  if [ "$cleanup_rc" -ne 0 ]; then
    printf 'worker UID cleanup exit: %s\n' "$cleanup_rc" >&2
    exit 1
  fi
  exit "$rc"
}
trap cleanup EXIT
cd "$REPO"
python3 e2e/worker-uid/check_test.py
node e2e/worker-uid/inventory.mjs > "$REPORT_DIR/expected.json"
PATTERN="$(node e2e/worker-uid/pattern.mjs "$REPORT_DIR/expected.json")"
node e2e/worker-uid/inventory.mjs command > "$REPORT_DIR/command-expected.json"
COMMAND_PATTERN="$(node e2e/worker-uid/pattern.mjs "$REPORT_DIR/command-expected.json")"
if [ "${WORKER_UID_SKIP_BUILD:-}" != 1 ]; then
  timeout --kill-after=5s 60s docker build -f agent/templates/base/Dockerfile -t "$IMAGE" .
fi
# Metadata (15s), optional build (65s), provisioning (35s): <=115s with kill grace.
# SKIP_BUILD=1 requires an already-present hosted image; never pull or install at runtime.
timeout --kill-after=5s 10s docker image inspect "$IMAGE" >/dev/null
# Docker seeds this owned volume from the image, preserving its locked dependencies.
timeout --kill-after=5s 30s docker run --pull never --network none \
  --name "$NAME-provision" --user 10003:10003 --cap-drop ALL \
  --security-opt no-new-privileges \
  --mount "type=volume,src=$DEPS,dst=/app/node_modules" \
  --entrypoint /usr/local/bin/node "$IMAGE" -e 'require.resolve("tsx")'
# Overlay only test inputs under /app so dependencies resolve from the image.
set +e
timeout --kill-after=30s "${WORKER_UID_TIMEOUT:-600}" docker run --pull never --network none \
  --name "$NAME" --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --tmpfs /data --tmpfs /nix \
  --mount "type=bind,src=$REPO/agent/src,dst=/app/src,readonly" \
  --mount "type=bind,src=$REPO/agent/test,dst=/app/test,readonly" \
  --mount "type=bind,src=$REPO/agent/templates,dst=/app/templates,readonly" \
  --entrypoint /usr/local/sbin/uzi-entrypoint "$IMAGE" \
  /usr/local/bin/node --import tsx --import /app/test/setup/hermetic-proc.ts \
  --test --test-concurrency=1 --test-timeout=120000 --test-reporter=junit \
  --test-name-pattern "$PATTERN" \
  /app/test/codex-launcher.test.ts /app/test/codex-executor.test.ts \
  /app/test/codex-shared-dir.test.ts /app/test/entrypoint-migration.test.ts \
  /app/test/git-planning-diff.test.ts \
  > "$REPORT_DIR/results.xml"
rc=$?
set -e
printf 'worker UID reports: %s\n' "$REPORT_DIR"
checker_rc=0
python3 e2e/worker-uid/check.py "$REPORT_DIR/expected.json" "$REPORT_DIR/results.xml" || checker_rc=$?
if [ "$rc" -ne 0 ]; then
  printf 'worker UID node/container exit: %s\n' "$rc" >&2
fi
# Separate cap-less command UID proof. It invokes the real supervisor and sandbox;
# starting directly at UID 10003 does not prove the production worker UID transition.
command_rc=0
timeout --kill-after=5s 115s docker run --pull never --network none \
  --name "$COMMAND_NAME" --user 10003:10003 --cap-drop ALL \
  --security-opt no-new-privileges --workdir /proof \
  --tmpfs /proof/.uzi/scratch:exec,uid=10003,gid=10003,mode=0700 \
  --env HOME=/proof/.uzi/scratch --env TMPDIR=/proof/.uzi/scratch \
  --env GOPATH=/proof/.uzi/scratch/go --env GOCACHE=/proof/.uzi/scratch/go-cache \
  --env GOTOOLCHAIN=local --env GOPROXY=off --env GOSUMDB=off \
  --mount "type=bind,src=$REPO/e2e/worker-uid,dst=/proof/e2e/worker-uid,readonly" \
  --mount "type=bind,src=$REPO/agent/src,dst=/proof/agent/src,readonly" \
  --mount "type=bind,src=$REPO/agent/tsconfig.json,dst=/proof/agent/tsconfig.json,readonly" \
  --mount "type=bind,src=$REPO/agent/package.json,dst=/proof/agent/package.json,readonly" \
  --mount "type=bind,src=$REPO/agent/codex/supervisor,dst=/proof/agent/codex/supervisor,readonly" \
  --mount "type=volume,src=$DEPS,dst=/proof/agent/node_modules,readonly" \
  --entrypoint /usr/local/bin/node "$IMAGE" \
  --import /proof/agent/node_modules/tsx/dist/loader.mjs \
  --test --test-concurrency=1 --test-timeout=110000 --test-reporter=junit \
  --test-name-pattern "$COMMAND_PATTERN" /proof/e2e/worker-uid/memory-command-real.test.mts \
  > "$REPORT_DIR/command-results.xml" || command_rc=$?
command_checker_rc=0
python3 e2e/worker-uid/check.py "$REPORT_DIR/command-expected.json" "$REPORT_DIR/command-results.xml" || command_checker_rc=$?
if [ "$command_rc" -ne 0 ]; then
  printf 'worker UID command node/container exit: %s\n' "$command_rc" >&2
fi
if [ "$rc" -ne 0 ] || [ "$checker_rc" -ne 0 ] || [ "$command_rc" -ne 0 ] || [ "$command_checker_rc" -ne 0 ]; then
  exit 1
fi
