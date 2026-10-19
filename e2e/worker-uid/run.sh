#!/usr/bin/env bash
# Run this tree's tests under the real root-start worker entrypoint and cap set.
set -euo pipefail
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="${WORKER_UID_IMAGE:-wuid-2134-base:local}"
TOKEN="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
NAME="wuid-2134-$TOKEN"
COMMAND_NAME="$NAME-command"
mkdir -p "$REPO/.uzi/scratch"
REPORT_DIR="$(mktemp -d "$REPO/.uzi/scratch/worker-uid.XXXXXX")"
cleanup() {
  local rc=$? cleanup_rc=0
  python3 "$REPO/e2e/worker-uid/bounded.py" cleanup "$REPORT_DIR" "$TOKEN" || cleanup_rc=$?
  if [ "$cleanup_rc" -ne 0 ]; then
    printf 'worker UID cleanup exit: %s\n' "$cleanup_rc" >&2
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
  printf 'worker UID phase start: %s\n' "$label" >&2
  "$@" || rc=$?
  printf 'worker UID phase end: %s elapsed=%ss exit=%s\n' "$label" "$((SECONDS-started))" "$rc" >&2
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
    printf 'worker UID deadline: phase=%s command=%s\n' "$label" "$*" >&2
  fi
  return "$rc"
}
capture() {
  local output=$1 seconds=$2
  shift 2
  python3 "$REPO/e2e/worker-uid/bounded.py" capture "$REPORT_DIR" "$TOKEN" "$output" "$seconds" "$@"
}
cd "$REPO"
python3 e2e/worker-uid/check_test.py
node e2e/worker-uid/inventory.mjs > "$REPORT_DIR/expected.json"
PATTERN="$(node e2e/worker-uid/pattern.mjs "$REPORT_DIR/expected.json")"
node e2e/worker-uid/inventory.mjs command > "$REPORT_DIR/command-expected.json"
COMMAND_PATTERN="$(node e2e/worker-uid/pattern.mjs "$REPORT_DIR/command-expected.json")"
if [ "${WORKER_UID_SKIP_BUILD:-}" != 1 ]; then
  phase image-build timeout --kill-after=5s 60s docker build -f agent/templates/base/Dockerfile -t "$IMAGE" .
fi
# Metadata (10s), optional build (65s), layout/dependency check (25s): <=100s.
# Never seed a dependency volume: use the immutable image's existing /app/node_modules.
# Mask image-declared volumes here: this Node-only check needs no Nix store.
phase image-metadata timeout --kill-after=5s 5s docker image inspect "$IMAGE" >/dev/null
prepared=$(phase image-layout timeout --kill-after=5s 20s python3 "$REPO/e2e/worker-uid/tool_image.py" "$REPORT_DIR" "$TOKEN" "$IMAGE")
read -r IMAGE COMMAND_IMAGE GO_ROOT TASK_BIN extra <<< "$prepared"
if [ -n "${extra:-}" ] || [ -z "$TASK_BIN" ]; then
  printf 'worker UID invalid prepared tool fields
' >&2
  exit 1
fi
# Overlay only test inputs under /app so dependencies resolve from the image.
set +e
owned_request container "$NAME" || exit 1
phase worker-proof capture "$REPORT_DIR/results.xml" "$((${WORKER_UID_TIMEOUT:-600} + 5))" \
  timeout --kill-after=5s "${WORKER_UID_TIMEOUT:-600}" docker run --pull never --network none \
  --label "io.uzi.worker-uid.invocation=$TOKEN" --name "$NAME" --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --tmpfs /data --tmpfs /nix --workdir /proof \
  --tmpfs /app/uzi-worker-uid-proof:exec,uid=10001,gid=10001,mode=0755 \
  --tmpfs /proof/.uzi/scratch:exec,uid=10001,gid=10001,mode=0700 \
  --mount "type=bind,src=$REPO/e2e/worker-uid,dst=/app/uzi-worker-uid-proof/e2e/worker-uid,readonly" \
  --mount "type=bind,src=$REPO/agent/src,dst=/app/uzi-worker-uid-proof/agent/src,readonly" \
  --mount "type=bind,src=$REPO/agent/test,dst=/app/uzi-worker-uid-proof/agent/test,readonly" \
  --mount "type=bind,src=$REPO/agent/templates,dst=/app/uzi-worker-uid-proof/agent/templates,readonly" \
  --entrypoint /usr/local/sbin/uzi-entrypoint "$IMAGE" \
  /usr/local/bin/node --import /app/node_modules/tsx/dist/loader.mjs --import /app/uzi-worker-uid-proof/e2e/worker-uid/image-layout.mjs --import /app/uzi-worker-uid-proof/agent/test/setup/hermetic-proc.ts \
  --test --test-concurrency=1 --test-timeout=120000 --test-reporter=junit \
  --test-name-pattern "$PATTERN" \
  /app/uzi-worker-uid-proof/agent/test/codex-launcher.test.ts /app/uzi-worker-uid-proof/agent/test/codex-executor.test.ts \
  /app/uzi-worker-uid-proof/agent/test/codex-shared-dir.test.ts /app/uzi-worker-uid-proof/agent/test/entrypoint-migration.test.ts \
  /app/uzi-worker-uid-proof/agent/test/git-planning-diff.test.ts \
  /app/uzi-worker-uid-proof/agent/test/codex-capture-reaper-real.test.ts
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
owned_request container "$COMMAND_NAME"
phase command-proof capture "$REPORT_DIR/command-results.xml" 120 \
  timeout --kill-after=5s 115s docker run --pull never --network none \
  --label "io.uzi.worker-uid.invocation=$TOKEN" --name "$COMMAND_NAME" --user 10003:10003 --cap-drop ALL \
  --security-opt no-new-privileges --workdir /proof --tmpfs /data --tmpfs /nix \
  --tmpfs /proof/.uzi/scratch:exec,uid=10003,gid=10003,mode=0700 \
  --tmpfs /app/uzi-worker-uid-proof:exec,uid=10003,gid=10003,mode=0755 \
  --env HOME=/proof/.uzi/scratch --env TMPDIR=/proof/.uzi/scratch \
  --mount "type=volume,src=$NAME-go,dst=$GO_ROOT,readonly" \
  --mount "type=volume,src=$NAME-task,dst=$TASK_BIN,readonly" \
  --env "GOROOT=$GO_ROOT" --env "PATH=$GO_ROOT/bin:$TASK_BIN:/usr/local/bin:/usr/bin:/bin" \
  --env GOPATH=/proof/.uzi/scratch/go --env GOCACHE=/proof/.uzi/scratch/go-cache \
  --env GOTOOLCHAIN=local --env GOPROXY=off --env GOSUMDB=off \
  --mount "type=bind,src=$REPO/Taskfile.yml,dst=/app/uzi-worker-uid-proof/Taskfile.yml,readonly" \
  --mount "type=bind,src=$REPO/homebrew-tap.yml,dst=/app/uzi-worker-uid-proof/homebrew-tap.yml,readonly" \
  --mount "type=bind,src=$REPO/e2e/worker-uid,dst=/app/uzi-worker-uid-proof/e2e/worker-uid,readonly" \
  --mount "type=bind,src=$REPO/agent/src,dst=/app/uzi-worker-uid-proof/agent/src,readonly" \
  --mount "type=bind,src=$REPO/agent/tsconfig.json,dst=/app/uzi-worker-uid-proof/agent/tsconfig.json,readonly" \
  --mount "type=bind,src=$REPO/agent/package.json,dst=/app/uzi-worker-uid-proof/agent/package.json,readonly" \
  --mount "type=bind,src=$REPO/agent/codex/supervisor,dst=/app/uzi-worker-uid-proof/agent/codex/supervisor,readonly" \
  --entrypoint /usr/local/bin/node "$COMMAND_IMAGE" \
  --import /app/node_modules/tsx/dist/loader.mjs --import /app/uzi-worker-uid-proof/e2e/worker-uid/image-layout.mjs \
  --test --test-concurrency=1 --test-timeout=110000 --test-reporter=junit \
  --test-name-pattern "$COMMAND_PATTERN" /app/uzi-worker-uid-proof/e2e/worker-uid/memory-command-real.test.mts || command_rc=$?
command_checker_rc=0
python3 e2e/worker-uid/check.py "$REPORT_DIR/command-expected.json" "$REPORT_DIR/command-results.xml" || command_checker_rc=$?
if [ "$command_rc" -ne 0 ]; then
  printf 'worker UID command node/container exit: %s\n' "$command_rc" >&2
fi
if [ "$rc" -ne 0 ] || [ "$checker_rc" -ne 0 ] || [ "$command_rc" -ne 0 ] || [ "$command_checker_rc" -ne 0 ]; then
  exit 1
fi
