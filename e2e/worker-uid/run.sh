#!/usr/bin/env bash
# Run this tree's tests under the real root-start worker entrypoint and cap set.
set -euo pipefail
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="${WORKER_UID_IMAGE:-wuid-2134-base:local}"
NAME="wuid-2134-$$"
mkdir -p "$REPO/.uzi/scratch"
REPORT_DIR="$(mktemp -d "$REPO/.uzi/scratch/worker-uid.XXXXXX")"
cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cd "$REPO"
python3 e2e/worker-uid/check_test.py
node e2e/worker-uid/inventory.mjs > "$REPORT_DIR/expected.json"
PATTERN="$(node e2e/worker-uid/pattern.mjs "$REPORT_DIR/expected.json")"
if [ "${WORKER_UID_SKIP_BUILD:-}" != 1 ]; then
  docker build -f agent/templates/base/Dockerfile -t "$IMAGE" .
fi
# Overlay only test inputs under /app so dependencies resolve from the image.
set +e
timeout --kill-after=30s "${WORKER_UID_TIMEOUT:-600}" docker run --rm --network none \
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
  > "$REPORT_DIR/results.xml"
rc=$?
set -e
printf 'worker UID reports: %s\n' "$REPORT_DIR"
checker_rc=0
python3 e2e/worker-uid/check.py "$REPORT_DIR/expected.json" "$REPORT_DIR/results.xml" || checker_rc=$?
if [ "$rc" -ne 0 ]; then
  printf 'worker UID node/container exit: %s\n' "$rc" >&2
fi
if [ "$rc" -ne 0 ] || [ "$checker_rc" -ne 0 ]; then
  exit 1
fi
