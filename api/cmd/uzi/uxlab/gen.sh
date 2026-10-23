#!/usr/bin/env bash
# Regenerate the raw-ANSI TUI frames under frames/ by driving the real tuiModel offline
# (no server, no PTY). See uxlab_gen_test.go and README.md for Go requirements.
# Full build: ./build.sh in provisioned Claude/Codex runs; devbox run build locally.
# Partial operation: ./gen.sh with the matching Go toolchain already on PATH.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
api="$(cd "$here/../../.." && pwd)"   # api/cmd/uzi/uxlab -> api

cd "$api"
UZI_UXLAB_GEN=1 go test ./cmd/uzi/ -run TestGenerateUXLabFrames -count=1
echo "frames written to $here/frames"
