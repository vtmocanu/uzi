#!/usr/bin/env bash
# Every repo fixture a web source file imports must be copied into both web image builds.
#
# The web Dockerfiles copy only web/ plus an explicit list of fixtures/<name> paths, then
# run `npm run build`, which type-checks the test files too. A new web test importing an
# uncopied fixture passes `gate:web` (the full tree is present) and breaks only the image
# build (#2602: fixtures/run-progress broke build-web and the KinD smoke on main).
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

# First path segment under fixtures/ of every STATIC module import of a fixture: `tsc
# --noEmit` must resolve those. A runtime read (readFileSync, new URL(...)) or a Vite
# `?raw` import (typed by a wildcard module declaration) is never resolved by tsc, so it
# does not break the image build and is not required here.
mapfile -t names < <(
  git grep -h -o -P "(?:\bfrom|\bimport)\s*\(?\s*['\"](?:\.\./)+fixtures/[^'\"?]+['\"]" \
    -- 'web/src/*.ts' 'web/src/*.tsx' \
    | sed -E "s|.*fixtures/([^/'\"]+).*|\\1|" | sort -u
)
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
