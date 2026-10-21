#!/bin/sh
# Print a tool's version as baked into the worker toolchain (#2288): the lock's
# default x86_64-linux output, named `<tool>-<X.Y.Z>`, is the source of truth.
# CI installs exactly this zizmor; actionlint keeps its own sha256 pin, checked
# against this by check-actions-tool-parity.sh. semgrep keeps its own reader
# (semgrep-worker-version.sh), since its store name carries a python prefix.
# Usage: worker-tool-version.sh <tool> [devbox.lock]
# Exit 0 = version printed, 2 = missing, ambiguous or unparseable input/tool.
set -eu

broken() { echo "worker-tool-version: INSTRUMENT BROKEN: $*" >&2; exit 2; }
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
case "$#" in
  1) tool="$1"; lock="$ROOT/agent/devbox-global/devbox.lock" ;;
  2) tool="$1"; lock="$2" ;;
  *) broken "usage: $0 <tool> [devbox.lock]" ;;
esac
# The name is spliced into a regex, so allow only characters with no regex meaning.
case "$tool" in
  ''|*[!a-z0-9-]*) broken "tool name must match [a-z0-9-]+" ;;
esac
command -v jq >/dev/null 2>&1 || broken "jq is required"
[ -r "$lock" ] || broken "lock is unreadable"

jq -er --arg tool "$tool" '
  .packages[$tool].systems["x86_64-linux"].outputs
  | if type == "array" then . else error("outputs must be an array") end
  | map(select(.default == true and ((.name // "out") == "out")))
  | if length == 1 then .[0].path else error("expected one default out output") end
  | capture("^/nix/store/[a-z0-9]{32}-" + $tool + "-(?<version>[0-9]+\\.[0-9]+\\.[0-9]+)$")
  | .version
' "$lock" || broken "cannot resolve the worker $tool version from the lock"
