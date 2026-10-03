#!/bin/sh
# Print the semgrep version baked into the worker toolchain (#2093): the lock's
# default x86_64-linux output is the single source of truth, and CI installs
# exactly this version. Usage: semgrep-worker-version.sh [devbox.lock]
# Exit 0 = version printed, 2 = missing, ambiguous or unparseable input/tool.
set -eu

broken() { echo "semgrep-worker-version: INSTRUMENT BROKEN: $*" >&2; exit 2; }
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
case "$#" in
  0) lock="$ROOT/agent/devbox-global/devbox.lock" ;;
  1) lock="$1" ;;
  *) broken "usage: $0 [devbox.lock]" ;;
esac
command -v jq >/dev/null 2>&1 || broken "jq is required"
[ -r "$lock" ] || broken "lock is unreadable"

jq -er '
  .packages.semgrep.systems["x86_64-linux"].outputs
  | if type == "array" then . else error("outputs must be an array") end
  | map(select(.default == true))
  | if length == 1 then .[0].path else error("expected one default output") end
  | capture("^/nix/store/[a-z0-9]{32}-(?:python[0-9]+(?:\\.[0-9]+)*-)?semgrep-(?<version>[0-9]+\\.[0-9]+\\.[0-9]+)$")
  | .version
' "$lock" || broken "cannot resolve the worker semgrep version from the lock"
