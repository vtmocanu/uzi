#!/bin/sh
# CI and the baked worker must run the same workflow linters (#2288), or a hosted
# worker's `lint:actions` and CI's lint-repo job disagree about what a finding is.
#   * zizmor: CI holds no version of its own; it installs whatever
#     scripts/worker-tool-version.sh reads from the worker lock (the semgrep shape).
#   * actionlint: CI keeps its sha256-verified release tarball (Renovate bumps
#     version and hash together), and its ACTIONLINT_VERSION must equal the lock's.
#     A bump on either side reddens this check until the other side follows.
# Usage: check-actions-tool-parity.sh [ci.yml devbox.lock]
# Exit 0 = parity, 1 = drift or a second version source, 2 = missing or
# ambiguous input/tool.
set -eu

broken() { echo "check-actions-tool-parity: INSTRUMENT BROKEN: $*" >&2; exit 2; }
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
case "$#" in
  0) workflow="$ROOT/.github/workflows/ci.yml"; lock="$ROOT/agent/devbox-global/devbox.lock" ;;
  2) workflow="$1"; lock="$2" ;;
  *) broken "usage: $0 [ci.yml devbox.lock]" ;;
esac
[ -r "$workflow" ] || broken "workflow is unreadable"

DERIVE='ZIZMOR_VERSION="$(./scripts/worker-tool-version.sh zizmor)"'
INSTALL='pipx install "zizmor==${ZIZMOR_VERSION}"'

zizmor_version="$("$ROOT/scripts/worker-tool-version.sh" zizmor "$lock")" || exit 2
actionlint_version="$("$ROOT/scripts/worker-tool-version.sh" actionlint "$lock")" || exit 2

# One awk pass over trimmed lines (awk, not grep -E: this host's grep mishandles
# negated bracket expressions, see CLAUDE.md). zizmor: DERIVE exactly once with
# INSTALL on the very next line, and no other assignment or install. actionlint:
# exactly one `ACTIONLINT_VERSION="v<X.Y.Z>"` assignment, compared to the lock.
problems="$(awk -v derive="$DERIVE" -v install="$INSTALL" -v want="v$actionlint_version" '
  { line = $0; sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line)
    code = line; sub(/#.*/, "", code); sub(/[[:space:]]+$/, "", code) }
  prev_derive { if (line == install) paired++; prev_derive = 0 }
  line == derive { derives++; prev_derive = 1; next }
  line == install { next }
  code ~ /ZIZMOR_VERSION[[:space:]]*[=:]/ { print NR ": other ZIZMOR_VERSION assignment: " $0 }
  code ~ /install.*zizmor/ || code ~ /zizmor[=@]=?[0-9]/ { print NR ": zizmor install or pin outside the lock-derived line: " $0 }
  code ~ /^ACTIONLINT_VERSION="v[0-9]+\.[0-9]+\.[0-9]+"$/ {
    pins++; got = code; sub(/^ACTIONLINT_VERSION="/, "", got); sub(/"$/, "", got)
    if (got != want) print NR ": CI actionlint " got " differs from the worker lock " want
    next }
  code ~ /ACTIONLINT_VERSION[[:space:]]*[=:]/ { print NR ": other ACTIONLINT_VERSION assignment: " $0 }
  code ~ /install.*actionlint/ { print NR ": actionlint install outside the pinned tarball: " $0 }
  END {
    if (derives != 1 || paired != 1) print "expected exactly one zizmor derive line immediately followed by the install line (derive=" derives + 0 ", paired=" paired + 0 ")"
    if (pins != 1) print "expected exactly one ACTIONLINT_VERSION=\"vX.Y.Z\" pin (found " pins + 0 ")"
  }
' "$workflow")"
if [ -n "$problems" ]; then
  echo "check-actions-tool-parity: FAIL:" >&2
  echo "$problems" >&2
  echo "CI and the worker lock (agent/devbox-global/devbox.lock) must carry the same actionlint and zizmor." >&2
  exit 1
fi
echo "check-actions-tool-parity: OK: CI and the worker lock agree on actionlint $actionlint_version and zizmor $zizmor_version"
