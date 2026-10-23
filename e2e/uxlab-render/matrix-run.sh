#!/usr/bin/env bash
# Sequential fresh stores/data; this script never builds an image.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"
action="${1:-matrix}"
case "$action" in matrix|local) ;; *) exit 2 ;; esac
: "${UXLAB_RENDER_CANDIDATE_DIR:?absolute exported full candidate repository required}"
if [ "$action" = matrix ]; then
  : "${UXLAB_RENDER_BASE_DIR:?absolute full export of frozen base 3fb1fdbf required}"
fi
case "${UXLAB_RENDER_NETWORK:-bridge}" in host|none|container:*)
  echo "ERROR: use a disconnectable Docker network for the offline handoff" >&2; exit 2 ;;
esac
command -v docker >/dev/null 2>&1 || { echo "ERROR: matrix requires Docker; absence is not red"; exit 1; }
# Resolve once so every cell uses exactly the same image even if a tag moves.
UXLAB_RENDER_IMAGE="$(timeout 10s docker image inspect "${UXLAB_RENDER_IMAGE:-ghcr.io/vtmocanu/uzi/agent-base:0.86.0-rc.18}" --format '{{.Id}}')"
export UXLAB_RENDER_IMAGE
if [ "$action" = local ]; then
  UXLAB_RENDER_CELL=local UXLAB_RENDER_MODE=off UXLAB_RENDER_REPO_DIR="$UXLAB_RENDER_CANDIDATE_DIR" bash "$here/run.sh" cell
else
  failed=0
  for cell in base candidate; do
    for mode in off required; do
      source_dir="$UXLAB_RENDER_CANDIDATE_DIR"
      if [ "$cell" = base ]; then source_dir="$UXLAB_RENDER_BASE_DIR"; fi
      # run.sh creates, verifies and removes its own exact container and volume
      # before the next iteration. Collect every independent cell; any failure stays red.
      UXLAB_RENDER_CELL="$cell" UXLAB_RENDER_MODE="$mode" UXLAB_RENDER_REPO_DIR="$source_dir" bash "$here/run.sh" cell || failed=1
    done
  done
  exit "$failed"
fi
