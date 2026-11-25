#!/usr/bin/env bash
# Run the web browser tests (vitest browser mode, then the runtime-web-modules harness) in
# the pinned CI Playwright image (task test:web-browser:in-container).
#
# Why a container: host Chromium cannot open its SingletonSocket under .uzi/scratch (the
# path is too long for a unix socket), and the image is the one CI runs `test-web-browser`
# in, so the result is the CI result.
#
# What is tested: the candidate = `git archive HEAD` plus this worktree's uncommitted and
# untracked (non-ignored) changes, exported to .uzi/scratch/snap-<slot>. web/node_modules is
# copied in from the worktree; only if its native bindings do not load in the image (a musl
# worker's do not) is it replaced by one `npm ci --ignore-scripts` in the container.
#
# Flow: git archive -> docker create --init (PID 1 reaps orphaned Chromium children) -> start
# -> docker cp the export and node_modules -> docker exec the tests -> copy the logs back ->
# remove the container by the id `docker create` printed (EXIT trap, also on failure).
#
# Environment:
#   SLOT     export slot, [A-Za-z0-9_-]+ (default: inrun)
#   NO_INIT  1 = create the container without --init (task test:web-browser:in-container-noinit)
#            so the zombie-tolerant cleanup path is exercised; default keeps --init
# Exit: the test command's status; 2 = the instrument is broken (docker missing, copy failed).
set -euo pipefail

IMAGE="mcr.microsoft.com/playwright:v1.64.1-noble@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f"

CDPATH=''
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd)
SLOT=${SLOT:-inrun}
broken() { echo "BROKEN: $*" >&2; exit 2; }
[[ $SLOT =~ ^[A-Za-z0-9_-]+$ ]] || broken "SLOT must match [A-Za-z0-9_-]+, got '$SLOT'"
command -v docker >/dev/null 2>&1 || broken "docker not on PATH"
[ -d "$ROOT/web/node_modules" ] || broken "web/node_modules is missing; install the web dependencies first"

SNAP="$ROOT/.uzi/scratch/snap-$SLOT"
LOGS="$ROOT/.uzi/scratch/snap-$SLOT-logs"
cid=""
cleanup() {
  if [ -n "$cid" ]; then docker rm -f "$cid" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

# 1. The candidate export.
rm -rf "$SNAP" "$LOGS"
mkdir -p "$SNAP" "$LOGS"
git -C "$ROOT" archive HEAD | tar -x -C "$SNAP"
# Overlay uncommitted work: modified and untracked files are copied, deleted ones removed.
# `git diff HEAD` (working tree vs HEAD, no rename detection) lists staged, unstaged and
# rename-source deletions alike.
(cd "$ROOT" && git diff --name-only --no-renames --diff-filter=D -z HEAD) | while IFS= read -r -d '' f; do rm -f -- "$SNAP/$f"; done
changed="$SNAP.changed"
(cd "$ROOT" && { git diff --name-only -z --diff-filter=d HEAD; git ls-files -o --exclude-standard -z; }) >"$changed"
if [ -s "$changed" ]; then
  tar -C "$ROOT" -c --null -T "$changed" | tar -x -C "$SNAP"
fi
rm -f "$changed"

# 2. The container. `sleep infinity` as PID 1 under --init; --ipc=host for Chromium's shared memory.
create_args=(--init --ipc=host)
if [ "${NO_INIT:-}" = "1" ]; then
  create_args=(--ipc=host)
  echo "NO_INIT=1: container created without --init (PID 1 will not reap zombies, as in CI)"
fi
cid=$(docker create "${create_args[@]}" --name "cdr4686-browser-$$" "$IMAGE" sleep infinity) || broken "docker create failed"
docker start "$cid" >/dev/null || broken "docker start failed"
docker cp "$SNAP/." "$cid:/work" || broken "docker cp of the export failed"
docker cp -L "$ROOT/web/node_modules" "$cid:/work/web/node_modules" || broken "docker cp of node_modules failed"

# The worker's node_modules can be built for another libc (a musl worker carries only the
# musl native bindings), and then rolldown cannot load in this glibc image. In that case, and
# only then, reinstall once in the container (the lockfile pins the same versions; no
# lifecycle scripts, as in web/Dockerfile).
if ! docker exec -w /work/web "$cid" node --input-type=module -e "await import('rolldown')" >/dev/null 2>&1; then
  echo "copied node_modules cannot load rolldown in this image; running npm ci --ignore-scripts in the container"
  docker exec -w /work/web "$cid" npm ci --ignore-scripts || broken "npm ci in the container failed"
fi

# 3. The tests. Output is streamed and kept in the container, then copied back.
rc=0
docker exec -e TZ=UTC -w /work/web "$cid" bash -c '
  set -o pipefail
  (npm run test:browser && npm run test:scripts && npm run test:modules-browser) 2>&1 | tee /tmp/web-browser.log
' || rc=$?
docker cp "$cid:/tmp/web-browser.log" "$LOGS/web-browser.log" >/dev/null 2>&1 || true
echo "EXIT=$rc" >>"$LOGS/web-browser.log"
echo "logs: $LOGS/web-browser.log (exit $rc)"
exit "$rc"
