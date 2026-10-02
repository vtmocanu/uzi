#!/bin/sh
# CI and the baked worker must use the same semgrep engine (#2093).
# The lock's default x86_64-linux output is the version source of truth.
# Usage: check-semgrep-parity.sh [ci.yml devbox.lock]
# Exit 0 = matching pins, 1 = drift, 2 = missing or ambiguous input/tool.
# The CI install is one literal `run: pipx install semgrep==X.Y.Z` line;
# changing that shape requires updating this check, never silently skipping it.
set -eu

broken() { echo "check-semgrep-parity: INSTRUMENT BROKEN: $*" >&2; exit 2; }
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
case "$#" in
  0) workflow="$ROOT/.github/workflows/ci.yml"; lock="$ROOT/agent/devbox-global/devbox.lock" ;;
  2) workflow="$1"; lock="$2" ;;
  *) broken "usage: $0 [ci.yml devbox.lock]" ;;
esac
command -v jq >/dev/null 2>&1 || broken "jq is required"
[ -r "$workflow" ] && [ -r "$lock" ] || broken "workflow or lock is unreadable"

ci_version="$(awk '
  /^[[:space:]]*run:[[:space:]]+pipx[[:space:]]+install[[:space:]]+semgrep==[0-9]+\.[0-9]+\.[0-9]+[[:space:]]*(#.*)?$/ {
    count++
    version = $0
    sub(/^.*semgrep==/, "", version)
    sub(/[[:space:]#].*$/, "", version)
  }
  END { if (count != 1) exit 2; print version }
' "$workflow")" || broken "expected exactly one literal CI semgrep pin"
worker_version="$(jq -er '
  .packages.semgrep.systems["x86_64-linux"].outputs
  | if type == "array" then . else error("outputs must be an array") end
  | map(select(.default == true))
  | if length == 1 then .[0].path else error("expected one default output") end
  | capture("^/nix/store/[a-z0-9]{32}-(?:python[0-9]+(?:\\.[0-9]+)*-)?semgrep-(?<version>[0-9]+\\.[0-9]+\\.[0-9]+)$")
  | .version
' "$lock")" || broken "cannot resolve the worker semgrep version from the lock"

if [ "$ci_version" != "$worker_version" ]; then
  echo "check-semgrep-parity: FAIL: CI $ci_version != worker lock $worker_version" >&2
  echo "Update the CI pin with the worker lock; independent Renovate bumps must not land." >&2
  exit 1
fi
echo "check-semgrep-parity: OK: CI and worker lock use semgrep $ci_version"
