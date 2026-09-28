#!/usr/bin/env bash
# test-linux.sh — run the uzi-lander hermetic tests on Linux before pushing a script change.
#
# CI and the uzi workers are Linux; a macOS-only run misses Linux-only failures such as the
# 128 KiB per-argv cap (E2BIG) and C-locale byte counting. This copies the WORKING TREE's
# .agents/ and Taskfile.yml (uncommitted edits included) into a throwaway container, commits
# them to a fresh git repo there (a mounted worktree's .git points at a host path), and runs
# the test:uzi-lander list from Taskfile.yml, or the named tests.
#
# Usage: test-linux.sh [--image IMAGE] [TEST.sh ...]
#   --image   default ubuntu:24.04: jq 1.7 like the CI runners, and mawk as its awk, stricter
#             than the runners' gawk, so gawk-only regexes fail here. Debian bookworm ships
#             jq 1.6, which fails some tests spuriously.
#   TEST.sh   test paths relative to the repo root; default: every test:uzi-lander entry.
#
# The container is named lander-linux-test-<pid> (outside the uzi- namespace) and removed
# on exit. Needs docker.
#
# Exit codes: 0 all passed; 1 a test failed (its name and last lines printed); 2 usage, or
# docker / the tree unavailable.
set -euo pipefail

IMAGE=ubuntu:24.04
TESTS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 2 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) TESTS+=("$1"); shift ;;
  esac
done

command -v docker >/dev/null 2>&1 || { echo "BROKEN: docker not on PATH" >&2; exit 2; }
docker info >/dev/null 2>&1 || { echo "BROKEN: docker daemon unreachable" >&2; exit 2; }
ROOT=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel 2>/dev/null) \
  || { echo "BROKEN: not inside a git checkout" >&2; exit 2; }
if [ "${#TESTS[@]}" -eq 0 ]; then
  while IFS= read -r t; do TESTS+=("$t"); done < <(
    awk '/^  test:uzi-lander:/{f=1; next} f && /^  [^ ]/{f=0} f && /\.test\.sh$/{sub(/^ *- *\.\//, ""); print}' \
      "$ROOT/Taskfile.yml")
fi
[ "${#TESTS[@]}" -gt 0 ] || { echo "BROKEN: no tests found in Taskfile.yml test:uzi-lander" >&2; exit 2; }

WORK=$(mktemp -d)
NAME="lander-linux-test-$$"
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$WORK"; }
trap cleanup EXIT INT TERM
COPYFILE_DISABLE=1 tar --no-xattrs -C "$ROOT" -cf "$WORK/tree.tar" .agents Taskfile.yml   # no macOS xattr headers

# The test list travels as positional arguments; the script inside reads them with "$@".
# The container ends with a LANDER_TESTS_RC=<n> line; without it the harness broke (docker
# refused the run, the image pull or package install failed), which is exit 2, never a test
# failure.
# shellcheck disable=SC2016
docker run --rm --name "$NAME" -v "$WORK":/in:ro "$IMAGE" bash -c '
  set -u
  export DEBIAN_FRONTEND=noninteractive
  { apt-get update -qq && apt-get install -y -qq jq git curl ca-certificates; } >/dev/null 2>&1 \
    || { echo "BROKEN: package install failed in the container" >&2; exit 2; }
  mkdir -p /r && tar -C /r -xf /in/tree.tar && cd /r
  git init -q && git add -A && git -c user.name=t -c user.email=t@example.com commit -qm tree
  echo "image: $(. /etc/os-release; echo "$PRETTY_NAME"), $(jq --version), $(bash --version | head -1)"
  rc=0
  for t in "$@"; do
    if bash "$t" > /tmp/out 2>&1; then echo "PASS $t"
    else rc=1; echo "FAIL $t"; tail -5 /tmp/out | sed "s/^/    /"; fi
  done
  echo "LANDER_TESTS_RC=$rc"
' test-linux "${TESTS[@]}" 2>&1 | tee "$WORK/out" || true
rc=$(sed -n 's/^LANDER_TESTS_RC=\([01]\)$/\1/p' "$WORK/out" | tail -1)
[ -n "$rc" ] || { echo "BROKEN: the container finished without a test result" >&2; exit 2; }
exit "$rc"
