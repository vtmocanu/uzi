#!/bin/sh
# Assert the hosted-worker image tag is PINNED INDEPENDENTLY of Chart.AppVersion
# (PRD #422 M1, Decision 1/3).
#
# usage: scripts/assert-worker-tag-decoupled.sh [chart-dir]
#   e.g. scripts/assert-worker-tag-decoupled.sh deploy/chart
#
# 🔴 WHY THIS EXISTS. The old Model B lockstep had workers.image.tag default to
# .Chart.AppVersion, so any app-only release (an api/web/chart bump that advances
# Chart.version/appVersion) silently rolled the ENTIRE hosted-worker fleet. M1
# decouples the two: the controller-deployment template stamps a CONCRETE tag string
# (values.yaml workers.image.tag) into each worker's pod-spec hash, and an appVersion
# bump must leave that tag — and therefore the fleet — untouched. Advancing the tag is
# a deliberate operator step. This harness renders the chart three ways and proves all
# three properties hold, so a future edit that re-links the tag to appVersion reddens
# here rather than shipping a surprise fleet roll.
#
# 🔴 OFFLINE, AND NON-VACUOUS. It calls `helm template` with no network: the CNPG
# `cluster` subchart is stripped from a temp copy of the chart (the controller template
# never references it; postgres is gated OFF by default), so no `helm dependency build`
# / OCI pull is needed. Each render EXTRACTS the UZI_WORKER_IMAGE_TAG env value and
# asserts it; a render that never emitted the env var is treated as a BROKEN INSTRUMENT
# (exit 2), never as a silent pass — an empty grep must not read as green.
#
# EXIT CODES (the convention scan-secrets.sh / check-spec-numbering.sh set):
#     2 = the instrument is broken (helm/chart missing, or the env var was absent
#         from a render that was supposed to contain it)
#     1 = a decoupling property FAILED (tag wrong, moved with appVersion, or override
#         did not take)
#     0 = all three properties hold
# `task`'s own rc is 201 for any non-zero.
set -eu

PINNED_TAG="0.50.0"     # the concrete worker tag values.yaml must pin (== current appVersion, a no-op now)
SENTINEL_APPVERSION="9.9.9"  # a bogus appVersion the tag must IGNORE
OVERRIDE_TAG="0.99.0"   # the deliberate-roll lever operators use (Part B)

# Resolve the chart dir: explicit arg, else relative to this script (../deploy/chart).
# Clear CDPATH first so `cd` cannot resolve via it and echo an unexpected directory.
CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

# Build an offline, dependency-free copy of the chart. Stripping the `dependencies:`
# block lets `helm template` render without vendoring the CNPG subchart; the
# controller-deployment template does not use it and postgres.enabled defaults false.
strip_chart() {
  # $1 = destination chart dir; $2 = appVersion to stamp
  _dst="$1"; _appver="$2"
  cp -R "$CHART_DIR" "$_dst"
  rm -f "$_dst/Chart.lock"
  awk -v ver="$_appver" '
    BEGIN { skip = 0 }
    /^dependencies:/ { skip = 1; next }        # drop the dependencies: list ...
    skip && /^[^[:space:]-]/ { skip = 0 }      # ... until the next top-level key
    skip { next }
    /^appVersion:/ { print "appVersion: \"" ver "\""; next }
    { print }
  ' "$CHART_DIR/Chart.yaml" > "$_dst/Chart.yaml"
}

# Render controller-deployment.yaml and echo the UZI_WORKER_IMAGE_TAG value, or print
# nothing (caller treats empty as a broken instrument). Extra --set args are forwarded.
render_tag() {
  _chart="$1"; shift
  helm template uzi "$_chart" \
    --set workers.enabled=true --set api.tls.enabled=true \
    --show-only templates/controller-deployment.yaml "$@" 2>/dev/null \
  | awk '
      /- name: UZI_WORKER_IMAGE_TAG/ { hit = 1; next }
      hit && /value:/ {
        v = $2
        gsub(/"/, "", v)
        print v
        exit
      }
    '
}

fail=0

# --- (a) default render: the tag is the concrete pin, and it is actually present ----
CHART_A="$WORK/a"
strip_chart "$CHART_A" "$PINNED_TAG"
got_a=$(render_tag "$CHART_A")
if [ -z "$got_a" ]; then
  echo "BROKEN: UZI_WORKER_IMAGE_TAG was absent from the default controller render" >&2
  exit 2
fi
if [ "$got_a" != "$PINNED_TAG" ]; then
  echo "FAIL (a): worker tag is '$got_a', expected the pinned '$PINNED_TAG'" >&2
  fail=1
else
  echo "OK (a): default render pins the worker tag to '$got_a'"
fi

# --- (b) decoupling proof: bump appVersion to a sentinel; the tag must NOT move ------
CHART_B="$WORK/b"
strip_chart "$CHART_B" "$SENTINEL_APPVERSION"
got_b=$(render_tag "$CHART_B")
if [ -z "$got_b" ]; then
  echo "BROKEN: UZI_WORKER_IMAGE_TAG was absent from the sentinel-appVersion render" >&2
  exit 2
fi
if [ "$got_b" = "$SENTINEL_APPVERSION" ]; then
  echo "FAIL (b): worker tag followed appVersion to the sentinel '$SENTINEL_APPVERSION' -- still coupled" >&2
  fail=1
elif [ "$got_b" != "$PINNED_TAG" ]; then
  echo "FAIL (b): appVersion bumped to '$SENTINEL_APPVERSION' moved the tag to '$got_b' (expected unchanged '$PINNED_TAG')" >&2
  fail=1
else
  echo "OK (b): appVersion bump to '$SENTINEL_APPVERSION' left the worker tag at '$got_b' (decoupled)"
fi

# --- (c) override still works: the deliberate-roll lever (Part B) is intact ----------
got_c=$(render_tag "$CHART_A" --set workers.image.tag="$OVERRIDE_TAG")
if [ -z "$got_c" ]; then
  echo "BROKEN: UZI_WORKER_IMAGE_TAG was absent from the override render" >&2
  exit 2
fi
if [ "$got_c" != "$OVERRIDE_TAG" ]; then
  echo "FAIL (c): --set workers.image.tag=$OVERRIDE_TAG rendered '$got_c' -- the roll lever is broken" >&2
  fail=1
else
  echo "OK (c): --set workers.image.tag override renders '$got_c'"
fi

if [ "$fail" -ne 0 ]; then
  echo "FAIL: hosted-worker tag is NOT correctly decoupled from Chart.AppVersion (PRD #422 M1)" >&2
  exit 1
fi

echo "OK: worker tag pinned to '$PINNED_TAG', independent of Chart.AppVersion, override lever intact (PRD #422 M1)"
