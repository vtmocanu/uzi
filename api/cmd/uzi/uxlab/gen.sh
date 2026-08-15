#!/usr/bin/env bash
# Regenerate the raw-ANSI TUI frames under frames/ by driving the real tuiModel offline
# (no server, no PTY). See uxlab_gen_test.go. Called by `devbox run build`.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
api="$(cd "$here/../../.." && pwd)"   # api/cmd/uzi/uxlab -> api

cd "$api"
UZI_UXLAB_GEN=1 go test ./cmd/uzi/ -run 'TestGenerateUXLab(Frames|Mocks)' -count=1
echo "frames written to $here/frames"
