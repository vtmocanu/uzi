#!/usr/bin/env bash
# Guard: CI tests the Postgres the stack ships.
#
#   check-postgres-pin-parity.sh [<compose-file> <ci-workflow> <store-it-script>]
#     defaults: docker-compose.yml .github/workflows/ci.yml e2e/run-store-it.sh
#
# 1. ci.yml's test-api-store-it postgres service pins the same tag AND digest as
#    docker-compose.yml's db. Since #2621 that service pulls through mirror.gcr.io
#    (Docker Hub's anonymous rate limit), so Renovate sees two packages (`postgres`,
#    `mirror.gcr.io/library/postgres`). renovate.json groups them into one branch
#    (#2622); this fails the gate if they still drift (a hand edit, or a branch
#    carrying only one).
# 2. Every floating `postgres:<tag>` reference in ci.yml and run-store-it.sh (the codex
#    job's UZI_STORE_IT_PG_IMAGE, its docker-pull-fallback.sh argument, the script's
#    default) uses the pinned tag. Renovate does not manage floating tags, so a major
#    bump of the pins must carry them by hand; this makes that bump's PR red until it does.
#
# Exit: 0 = consistent; 1 = a pin or floating tag differs; 2 = a pin is missing,
# duplicated or malformed (instrument broken: a reworded image line must not read as
# parity).
# awk, not grep: the patterns need a negated class, which this host's ugrep mishandles
# in -E mode and BSD grep cannot do in -P mode (root CLAUDE.md, "grep on this host").
set -euo pipefail

compose=${1:-docker-compose.yml}
ci=${2:-.github/workflows/ci.yml}
storeit=${3:-e2e/run-store-it.sh}

# Prints "<tag>@sha256:<hex>" for the one `image: <repo>:...` line in a file. Every
# `image: <repo>:` line is a candidate: exactly one may exist, and it must be a full
# tag + 64-hex digest pin.
pin() {
  local file=$1 repo=$2 out
  out=$(awk -v repo="$repo" '
    { line = $0; sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line) }
    index(line, "image:") == 1 {
      ref = substr(line, 7); sub(/^[ \t]+/, "", ref)
      if (index(ref, repo ":") == 1) { n++; v = substr(ref, length(repo) + 2) }
    }
    END {
      if (n != 1) exit 1
      if (v !~ /^[^@ \t]+@sha256:[0-9a-f]+$/) exit 1
      if (length(v) - index(v, "@sha256:") - 7 != 64) exit 1
      print v
    }' "$file") || {
    echo "check-postgres-pin-parity: expected exactly one '$repo:<tag>@sha256:<64 hex>' image line in $file" >&2
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

tag=${compose_pin%%@*}
# Floating references: `postgres:<tag>` starting a name, i.e. at line start or after a
# space, `/`, quote, `=`, `(` or `:`, or after the `:-` of a shell default
# (`${X:-postgres:17}`). So `mirror.gcr.io/library/postgres:` counts and
# `my-postgres:` does not.
drift=$(awk -v want="$tag" '
  {
    s = $0; off = 0
    while (match(s, /postgres:[A-Za-z0-9._-]+/)) {
      abs = off + RSTART
      pre = (abs > 1) ? substr($0, abs - 1, 1) : ""
      pre2 = (abs > 2) ? substr($0, abs - 2, 1) : ""
      named = (abs == 1) || pre ~ /[ \t\/"'"'"'=(:]/ || (pre == "-" && pre2 == ":")
      t = substr(s, RSTART + 9, RLENGTH - 9)
      if (named && t != want) printf "%s:%d: postgres:%s\n", FILENAME, FNR, t
      off += RSTART + RLENGTH - 1
      s = substr(s, RSTART + RLENGTH)
    }
  }' "$ci" "$storeit")
if [ -n "$drift" ]; then
  echo "check-postgres-pin-parity: floating postgres references differ from the pinned tag $tag:" >&2
  printf '  %s\n' "$drift" >&2
  echo "Renovate does not manage floating tags; update them with the pin." >&2
  exit 1
fi
echo "check-postgres-pin-parity: OK ($compose_pin)"
