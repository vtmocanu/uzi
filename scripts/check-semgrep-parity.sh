#!/bin/sh
# CI and the baked worker must use the same semgrep engine (#2093). CI holds no
# version of its own: it installs whatever scripts/semgrep-worker-version.sh reads
# from the worker lock, so the two cannot drift. This check pins that shape.
# Usage: check-semgrep-parity.sh [ci.yml devbox.lock]
# Exit 0 = CI derives from a readable lock, 1 = CI pins or installs semgrep any
# other way, 2 = missing or ambiguous input/tool.
# The CI install is exactly the adjacent DERIVE/INSTALL lines below; changing that
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

worker_version="$("$ROOT/scripts/semgrep-worker-version.sh" "$lock")" || exit 2

# One awk pass over trimmed lines (awk, not grep -E: this host's grep mishandles
# negated bracket expressions, see CLAUDE.md). DERIVE must sit exactly once, with
# INSTALL on the very next line; any other SEMGREP_VERSION assignment and any other
# semgrep install line is a second version source.
problems="$(awk -v derive="$DERIVE" -v install="$INSTALL" '
  { line = $0; sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line)
    code = line; sub(/#.*/, "", code) }
  prev_derive { if (line == install) paired++; prev_derive = 0 }
  line == derive { derives++; prev_derive = 1; next }
  line == install { next }
  code ~ /SEMGREP_VERSION[[:space:]]*[=:]/ { print NR ": other SEMGREP_VERSION assignment: " $0 }
  code ~ /pip[x3]?[[:space:]]+install.*semgrep/ { print NR ": semgrep install outside the lock-derived line: " $0 }
  END { if (derives != 1 || paired != 1) print "expected exactly one derive line immediately followed by the install line (derive=" derives + 0 ", paired=" paired + 0 ")" }
' "$workflow")"
if [ -n "$problems" ]; then
  echo "check-semgrep-parity: FAIL:" >&2
  echo "$problems" >&2
  echo "CI must install the worker lock's semgrep; a CI-side pin or Renovate bump must not land." >&2
  exit 1
fi
echo "check-semgrep-parity: OK: CI installs the worker lock's semgrep $worker_version"
