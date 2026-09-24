#!/usr/bin/env bash
# PRD #1598 M5 boundary measurement.
#
# Drives the REAL uzi-codex-supervisor + uzi-codex-command-sandbox, as uid
# 10003, over a scripted Go-heavy command sequence (`go build ./...` then
# `go test -run XXX_none ./...` over this repo's `api/` module -- compiles and
# links everything, runs nothing) against a given git ref, and reports:
#
#   - the /tmp residue (writable-layer bytes under /tmp) after each command
#     and at the end,
#   - the per-run Codex command cache's peak size and whether it is retained
#     after release,
#   - the module bytes downloaded (via GOMODCACHE `cache/download` growth AND
#     a count of `go: downloading` lines), comparing command #1 (cold) against
#     the rest.
#
# This is a BOUNDARY-LEVEL PROXY, not the hosted Codex-run measurement (a
# post-deploy maintainer check against the real k8s worker fleet). See
# README.md for exactly what is proxied and what is bypassed.
#
# Usage:
#   ./run.sh <git-ref>              # e.g. ./run.sh main, ./run.sh HEAD~1, a sha
#   UZI_M1598_N=5 ./run.sh <ref>    # commands per ref (default 5)
#   UZI_M1598_KEEP=1 ./run.sh <ref> # keep the throwaway worktree + image for inspection
#
# Prints the human-readable log to stderr and one NDJSON measurement line per
# event to stdout; redirect stdout to a file to keep the machine-readable
# record (RESULTS.md was built this way; see its header for the exact
# commands run).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"

REF="${1:?usage: run.sh <git-ref> (UZI_M1598_N=<count> to set N, default 5)}"
N="${UZI_M1598_N:-5}"
KEEP="${UZI_M1598_KEEP:-0}"

log() { printf '%s\n' "$*" >&2; }

SLUG=$(printf '%s' "$REF" | tr -c 'a-zA-Z0-9' '-' | tr '[:upper:]' '[:lower:]')
SHA=$(git -C "$REPO" rev-parse "$REF")
SHORT=${SHA:0:12}
WORKDIR=$(mktemp -d "${TMPDIR:-/tmp}/uzi-m1598-$SLUG-XXXXXX")
CTX="$WORKDIR/ctx"
IMAGE="m1598-img-$SLUG-$SHORT"
# Deterministic, unique-per-invocation container name (also passed to `docker
# run --name` below) so the cleanup trap can target exactly the container this
# invocation started, never a foreign one, even if it is still running (e.g.
# this script was killed mid-run).
CONTAINER="m1598-measure-$SLUG-$$"
# Only set true once THIS invocation's `docker build` has actually succeeded.
# Gating the image removal on it means a failed build (or a build that never
# ran) never issues `docker image rm` against an image this invocation did
# not create -- e.g. a same-named image left over from a prior, still-wanted
# `UZI_M1598_KEEP=1` run at the same ref/sha.
BUILT=0

cleanup() {
  # Always stop/remove OUR OWN container by its exact deterministic name,
  # regardless of KEEP: a kept image/worktree is for inspecting the build
  # artifacts, not for leaving a (possibly still-running) container behind.
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  if [ "$KEEP" != "1" ]; then
    git -C "$REPO" worktree remove --force "$WORKDIR/wt" >/dev/null 2>&1 || true
    rm -rf "$WORKDIR"
    git -C "$REPO" worktree prune >/dev/null 2>&1 || true
    if [ "$BUILT" = "1" ]; then
      docker image rm -f "$IMAGE" >/dev/null 2>&1 || true
    fi
  else
    log "UZI_M1598_KEEP=1: leaving worktree at $WORKDIR/wt and image $IMAGE"
  fi
}
trap cleanup EXIT

log "=== ref $REF ($SHORT) -> detached worktree ==="
git -C "$REPO" worktree add --detach "$WORKDIR/wt" "$SHA" >&2

# A synthetic build context, not the worktree root: the repo's root
# .dockerignore excludes agent/, api/ and e2e/ for the WEB image (see its own
# header comment) and BuildKit resolves that ignore for ANY build whose
# context is the repo root, this one included. A fresh directory containing
# only what this Dockerfile COPYs has no .dockerignore of its own to fight.
mkdir -p "$CTX/agent/codex/supervisor" "$CTX/api" "$CTX/e2e/codex-tmp-measure"
cp -a "$WORKDIR/wt/agent/codex/supervisor/." "$CTX/agent/codex/supervisor/"
cp -a "$WORKDIR/wt/api/." "$CTX/api/"
cp "$HERE/measure-inner.sh" "$CTX/e2e/codex-tmp-measure/measure-inner.sh"

log "=== building $IMAGE (fallback minimal image; see README.md deviation) ==="
docker build -f "$HERE/Dockerfile.fallback" -t "$IMAGE" "$CTX" >&2
BUILT=1

# The docker CLI here may talk to a remote/sibling daemon (DOCKER_HOST): no
# bind mount is used anywhere (a host path is not guaranteed to be the
# daemon's path -- measured directly during development, see README.md "Why
# everything is baked into the image"). Everything the measurement needs is
# baked into the image at build time; results travel back over the docker
# CLI's own stdout/stderr, which the run.sh caller redirects.
#
# The capability set: SETUID/SETGID/SETPCAP let setpriv actually clear the
# bounding set to empty for the command-uid supervisor invocations (SETPCAP is
# needed for the bounding-set drop itself, not just the uid/gid change);
# CHOWN/DAC_OVERRIDE/FOWNER let root's setup step (the writable API copy, the
# cache root) run without depending on the image's file modes.
CAPS=(--cap-drop ALL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER)

log "=== running $N supervised commands in $IMAGE (uid 10003) ==="
docker run --rm "${CAPS[@]}" -e "UZI_M1598_N=$N" --name "$CONTAINER" \
  "$IMAGE" bash /measure-inner.sh

log "=== done: $REF ($SHORT) ==="
