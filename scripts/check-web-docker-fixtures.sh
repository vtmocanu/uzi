#!/usr/bin/env bash
# Every repo fixture a web source file statically imports must be copied into both web
# image builds.
#
# The web Dockerfiles copy only web/ plus an explicit list of fixtures/<name> paths, then
# run `npm run build`, which type-checks the test files too. A new web test importing an
# uncopied fixture passes `gate:web` (the full tree is present) and breaks only the image
# build (#2602: fixtures/run-progress broke build-web and the KinD smoke on main).
#
# Only module specifiers count, i.e. `from "<spec>"`, `import "<spec>"` and `import("<spec>")`,
# including a specifier on a later line or behind comments: `tsc --noEmit` must resolve
# those. A runtime read (readFileSync, new URL(...)) or a Vite `?raw` import (typed by a
# wildcard module declaration) is never resolved by tsc and is not required here.
# Each file is scanned whole, so a specifier split from its keyword is still found.
# Hermetic tests: scripts/check-web-docker-fixtures.test.sh.
#
# Exit: 0 every imported fixture is copied; 1 a fixture is missing from a Dockerfile;
# 2 instrument broken (no fixture import found at all, or a Dockerfile is missing).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

dockerfiles=(web/Dockerfile web/Dockerfile.mock)
for df in "${dockerfiles[@]}"; do
  if [[ ! -f "$df" ]]; then
    echo "check-web-docker-fixtures: $df not found (instrument broken)" >&2
    exit 2
  fi
done

mapfile -t sources < <(git ls-files -- 'web/src/*.ts' 'web/src/*.tsx')
names=()
if [[ ${#sources[@]} -gt 0 ]]; then
  # First path segment under fixtures/ of every module specifier naming a repo fixture.
  # $gap is whitespace, block comments and line comments, so the specifier may sit on a
  # later line than its keyword. A `?` in the specifier (Vite `?raw`) fails the match.
  mapfile -t names < <(
    perl -0777 -ne '
      my $gap = qr{(?:\s|/\*.*?\*/|//[^\n]*(?:\n|\z))*}s;
      while (/\b(?:from|import)$gap\(?$gap(["\x27])(?:\.\.\/)+fixtures\/([^\/"\x27?]+)[^"\x27?]*\1/gs) {
        print "$2\n";
      }
    ' "${sources[@]}" | sort -u
  )
fi
if [[ ${#names[@]} -eq 0 ]]; then
  echo "check-web-docker-fixtures: no fixture import found under web/src (instrument broken)" >&2
  exit 2
fi

missing=0
for name in "${names[@]}"; do
  for df in "${dockerfiles[@]}"; do
    if ! grep -q -E "^COPY fixtures/${name//./\\.}(/| )" "$df"; then
      echo "check-web-docker-fixtures: $df does not COPY fixtures/$name, which web/src imports" >&2
      missing=1
    fi
  done
done

if [[ $missing -ne 0 ]]; then
  echo "check-web-docker-fixtures: add 'COPY fixtures/<name> /app/fixtures/<name>' before 'RUN npm run build' in each file above" >&2
  exit 1
fi
echo "check-web-docker-fixtures: OK - ${#names[@]} imported fixture path(s) copied by ${#dockerfiles[@]} Dockerfiles"
