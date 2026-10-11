#!/usr/bin/env bash
# The ONE path definition for the release train. SOURCE this file; it defines the
# agent runtime set and is_shipping, and runs nothing on its own.
#
# AGENT_PATHS is the space-separated runtime surface used by autobump's raw Git
# pathspec diffs: source, dependency manifests, TypeScript config, binaries,
# templates, global devbox toolchain and Codex packaging/supervisor.
# is_shipping <path> returns 0 for that set (exact entries or slash descendants),
# api, controller, web app, chart and docs; 1 for tests, e2e, fixtures and everything
# else (including skill/PRD-only changes). Tests are excluded before runtime matching.
# Message exemptions live in the oracle, not this path classifier.
#
# Four consumers share this definition:
#   - scripts/assert-changelog-covers-release.sh: which merges must be cited (oracle).
#   - .agents/skills/uzi-release/scripts/release-cut.sh: whether --promote needs a
#     next candidate; skill/PRD-only work with empty [Unreleased] is nonshipping.
#   - scripts/worker-tag-autobump.sh: which runtime paths warrant rolling workers.
#   - scripts/check-changelog-entry.sh: which branch changes need a changelog entry.
AGENT_PATHS="agent/src agent/package.json agent/package-lock.json agent/tsconfig.json agent/bin agent/templates agent/devbox-global agent/codex"

is_shipping() {
  case "$1" in
    *_test.go|*.test.ts|*.test.tsx|*/testdata/*|*/test/*|e2e/*|fixtures/*) return 1 ;;
    api/*|controller/*|web/src/*|deploy/chart/*|docs/*) return 0 ;;
  esac
  local runtime_path
  for runtime_path in $AGENT_PATHS; do
    case "$1" in
      "$runtime_path"|"$runtime_path"/*) return 0 ;;
    esac
  done
  return 1
}
