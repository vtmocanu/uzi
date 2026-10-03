#!/bin/sh
# CI and the baked worker must use the same semgrep engine (#2093). CI holds no
# version of its own: it installs whatever scripts/semgrep-worker-version.sh reads
# from the worker lock, so the two cannot drift. This check pins that shape.
# Usage: check-semgrep-parity.sh [ci.yml devbox.lock]
# Exit 0 = CI derives from a readable lock, 1 = CI pins or installs semgrep any
# other way, 2 = missing or ambiguous input/tool.
# The CI install is exactly the two DERIVE/INSTALL lines below; changing that
# shape requires updating this check, never silently skipping it.
set -eu

broken() { echo "check-semgrep-parity: INSTRUMENT BROKEN: $*" >&2; exit 2; }
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
case "$#" in
  0) workflow="$ROOT/.github/workflows/ci.yml"; lock="$ROOT/agent/devbox-global/devbox.lock" ;;
  2) workflow="$1"; lock="$2" ;;
  *) broken "usage: $0 [ci.yml devbox.lock]" ;;
esac
[ -r "$workflow" ] || broken "workflow is unreadable"

DERIVE='SEMGREP_VERSION="$(./scripts/semgrep-worker-version.sh)"'
INSTALL='pipx install "semgrep==${SEMGREP_VERSION}"'
count() { awk -v want="$1" '{ line = $0; sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line); if (line == want) n++ } END { print n + 0 }' "$workflow"; }

worker_version="$("$ROOT/scripts/semgrep-worker-version.sh" "$lock")" || exit 2

fail=0
derive="$(count "$DERIVE")"
install="$(count "$INSTALL")"
if [ "$derive" != 1 ] || [ "$install" != 1 ]; then
  echo "check-semgrep-parity: FAIL: CI needs exactly one '$DERIVE' (found $derive) and one '$INSTALL' (found $install)" >&2
  fail=1
fi
# Any other semgrep install (a literal pin, an unpinned install) is a second source.
# awk, not grep -E: this host's grep mishandles negated bracket expressions (CLAUDE.md).
others="$(awk -v ok="$INSTALL" '{ code = $0; sub(/#.*/, "", code) } code ~ /pip[x3]?[[:space:]]+install.*semgrep/ && index($0, ok) == 0 { print NR ": " $0 }' "$workflow")"
if [ -n "$others" ]; then
  echo "check-semgrep-parity: FAIL: CI installs semgrep outside the lock-derived line:" >&2
  echo "$others" >&2
  fail=1
fi
if [ "$fail" != 0 ]; then
  echo "CI must install the worker lock's semgrep; a CI-side pin or Renovate bump must not land." >&2
  exit 1
fi
echo "check-semgrep-parity: OK: CI installs the worker lock's semgrep $worker_version"
