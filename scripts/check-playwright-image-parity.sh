#!/usr/bin/env bash
# Guard: test-web-browser's Playwright image matches web's playwright npm pin.
#
#   check-playwright-image-parity.sh [<ci-workflow> <web-package-json>]
#     defaults: .github/workflows/ci.yml web/package.json
#
# The job runs in mcr.microsoft.com/playwright:v<version>-noble, which bakes the
# browsers for exactly that Playwright release. npm ci installs web's `playwright`
# devDependency; if the two versions differ, Playwright looks for browser builds the
# image does not carry and the job fails with "Executable doesn't exist".
# renovate.json groups the image and the npm package; this fails the gate if they
# still drift (a hand edit, or a branch carrying only one).
#
# Exit: 0 = consistent; 1 = versions differ; 2 = the image line or npm pin is
# missing, duplicated or malformed (instrument broken).
# awk, not grep -E with a negated class (root CLAUDE.md, "grep on this host").
set -euo pipefail

ci=${1:-.github/workflows/ci.yml}
pkg=${2:-web/package.json}
repo=mcr.microsoft.com/playwright

# Only jobs.test-web-browser.container.image counts: the job block runs from its
# two-space key to the next two-space key, and the container block from its
# four-space `container:` key to the next four-space key. Exactly one six-space
# `image:` may sit there, and it must be the Playwright image, so an image in another
# job or in a step's `with:` cannot read as parity.
image=$(awk -v repo="$repo" '
  /^  [^ #]/ { injob = ($0 ~ /^  test-web-browser:[ \t]*$/); incont = 0; next }
  injob && /^    [^ #]/ { incont = ($0 ~ /^    container:[ \t]*$/); next }
  injob && incont && /^      image:/ {
    ref = $0; sub(/^      image:[ \t]*/, "", ref); sub(/[ \t]+$/, "", ref)
    n++
    if (index(ref, repo ":") != 1) exit 1
    v = substr(ref, length(repo) + 2)
  }
  END {
    if (n != 1) exit 1
    if (v !~ /^v[0-9]+\.[0-9]+\.[0-9]+-[a-z]+@sha256:[0-9a-f]+$/) exit 1
    if (length(v) - index(v, "@sha256:") - 7 != 64) exit 1
    print v
  }' "$ci") || {
  echo "check-playwright-image-parity: expected exactly one 'image: $repo:v<x.y.z>-<distro>@sha256:<64 hex>' line under $ci's jobs.test-web-browser.container" >&2
  exit 2
}

npm=$(awk '
  /"playwright"[ \t]*:/ {
    n++; v = $0
    sub(/^.*"playwright"[ \t]*:[ \t]*"/, "", v); sub(/".*$/, "", v)
  }
  END { if (n != 1 || v !~ /^[0-9]+\.[0-9]+\.[0-9]+$/) exit 1; print v }' "$pkg") || {
  echo "check-playwright-image-parity: expected exactly one exact \"playwright\": \"x.y.z\" pin in $pkg" >&2
  exit 2
}

img_version=${image#v}
img_version=${img_version%%-*}
if [ "$img_version" != "$npm" ]; then
  echo "check-playwright-image-parity: versions differ:" >&2
  echo "  $ci: $repo:$image" >&2
  echo "  $pkg: playwright $npm" >&2
  echo "Bump both together (renovate.json groups them as \"playwright\")." >&2
  exit 1
fi
echo "check-playwright-image-parity: OK (playwright $npm)"
