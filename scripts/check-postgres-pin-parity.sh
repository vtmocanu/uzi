#!/usr/bin/env bash
# Guard: ci.yml's test-api-store-it postgres service pins the same tag AND digest as
# docker-compose.yml's db, so the CI LiveDB lane tests the engine the stack ships.
#
#   check-postgres-pin-parity.sh [<compose-file> <ci-workflow>]
#     defaults: docker-compose.yml .github/workflows/ci.yml
#
# Since #2621 the ci.yml service pulls through mirror.gcr.io (Docker Hub's anonymous
# rate limit), so Renovate sees two packages (`postgres`, `mirror.gcr.io/library/postgres`).
# renovate.json groups them into one branch (#2622); this check fails the gate if the
# two pins still drift apart (a hand edit, or a Renovate branch carrying only one).
#
# Exit: 0 = same tag and digest; 1 = they differ; 2 = a pin is missing or unparsable
# (instrument broken: a reworded image line must not read as parity).
# awk, not grep: the pattern needs a negated class, which this host's ugrep mishandles
# in -E mode and BSD grep cannot do in -P mode (root CLAUDE.md, "grep on this host").
set -euo pipefail

compose=${1:-docker-compose.yml}
ci=${2:-.github/workflows/ci.yml}

# Prints "<tag>@sha256:<hex>" for the single matching `image:` line, or fails.
pin() {
  local file=$1 repo=$2 out
  # Exactly one `image: <repo>:<tag>@sha256:<64 hex>` line; print "<tag>@sha256:<hex>".
  out=$(awk -v repo="$repo" '
    { line = $0; sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line) }
    index(line, "image:") == 1 {
      ref = substr(line, 7); sub(/^[ \t]+/, "", ref)
      if (index(ref, repo ":") == 1) {
        rest = substr(ref, length(repo) + 2)
        if (rest ~ /^[^@ \t]+@sha256:[0-9a-f]+$/ && length(rest) - index(rest, "@sha256:") - 7 == 64) { n++; v = rest }
      }
    }
    END { if (n == 1) print v; else exit 1 }' "$file") || {
    echo "check-postgres-pin-parity: expected exactly one '$repo:<tag>@sha256:<digest>' image line in $file" >&2
    return 1
  }
  printf '%s\n' "$out"
}

compose_pin=$(pin "$compose" 'postgres') || exit 2
ci_pin=$(pin "$ci" 'mirror.gcr.io/library/postgres') || exit 2

if [ "$compose_pin" != "$ci_pin" ]; then
  echo "check-postgres-pin-parity: postgres pins differ:" >&2
  echo "  $compose: postgres:$compose_pin" >&2
  echo "  $ci: mirror.gcr.io/library/postgres:$ci_pin" >&2
  echo "Bump both to the same tag and digest (renovate.json groups them as \"postgres\")." >&2
  exit 1
fi
echo "check-postgres-pin-parity: OK ($compose_pin)"
