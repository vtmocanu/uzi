#!/bin/sh
# Assert the API Deployment renders ephemeral size (issue #2412) and
# UZI_EPHEMERAL_LEASE (PRD #2006) from
# workers.ephemeralLease: the "2h" default, null -> "2h", an explicit 0 surviving as "0",
# and a --set override such as 30m taking effect.
#
# usage: scripts/assert-ephemeral-lease-render.sh [chart-dir]
#   e.g. scripts/assert-ephemeral-lease-render.sh deploy/chart
#
# WHY THIS EXISTS. The lease is a direct env entry on the api Deployment. A template
# written with `with` or a bare truthiness test would read an explicit 0 (lease disabled)
# as unset and silently render the 2h default, so an operator who turned the feature off
# would still have it on, with nothing red. This harness renders the chart and proves each
# value reaches the env as written.
#
# OFFLINE AND NON-VACUOUS. It calls `helm template` with no network: the CNPG `cluster`
# subchart is stripped from a temp copy of the chart (the api template never references
# it; postgres is gated off by default). Each render EXTRACTS the env value and asserts
# it; a render that never emitted the env var is a BROKEN INSTRUMENT (exit 2), never a
# silent pass.
#
# EXIT CODES (the convention assert-drain-knobs-render.sh sets):
#     2 = the instrument is broken (helm/chart missing, or the env var was absent from a
#         render that was supposed to contain it)
#     1 = a case rendered the wrong value
#     0 = every property holds
# `task`'s own rc is 201 for any non-zero.
set -eu

# Resolve the chart dir: explicit arg, else relative to this script (../deploy/chart).
# Clear CDPATH first so `cd` cannot resolve via it and echo an unexpected directory.
CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

# Build an offline, dependency-free copy of the chart.
strip_chart() {
  _dst="$1"
  cp -R "$CHART_DIR" "$_dst"
  rm -f "$_dst/Chart.lock"
  awk '
    BEGIN { skip = 0 }
    /^dependencies:/ { skip = 1; next }        # drop the dependencies: list ...
    skip && /^[^[:space:]-]/ { skip = 0 }      # ... until the next top-level key
    skip { next }
    { print }
  ' "$CHART_DIR/Chart.yaml" > "$_dst/Chart.yaml"
}

# Render api-deployment.yaml and echo the value of UZI_EPHEMERAL_LEASE, or print nothing
# (the caller treats empty as a broken instrument). Extra --set args are forwarded.
render_lease() {
  _chart="$1"; shift
  helm template uzi "$_chart" \
    --show-only templates/api-deployment.yaml "$@" 2>/dev/null \
  | awk '
      /- name: UZI_EPHEMERAL_LEASE$/ { hit = 1; next }
      hit && /value:/ {
        v = $2
        gsub(/"/, "", v)
        print v
        exit
      }
    '
}

CHART="$WORK/chart"
strip_chart "$CHART"

fail=0

# check <label> <expected> [--set args...]
check() {
  _label="$1"; _want="$2"; shift 2
  _got=$(render_lease "$CHART" "$@")
  if [ -z "$_got" ]; then
    echo "BROKEN: UZI_EPHEMERAL_LEASE was absent from the render for case '$_label'" >&2
    exit 2
  fi
  if [ "$_got" != "$_want" ]; then
    echo "FAIL ($_label): UZI_EPHEMERAL_LEASE is '$_got', expected '$_want'" >&2
    fail=1
  else
    echo "OK ($_label): UZI_EPHEMERAL_LEASE renders '$_got'"
  fi
}

check "default" "2h"
check "explicit null" "2h" --set workers.ephemeralLease=null
check "zero disables" "0" --set workers.ephemeralLease=0
check "override 30m" "30m" --set workers.ephemeralLease=30m

if [ "$fail" -ne 0 ]; then
  echo "FAIL: workers.ephemeralLease is NOT correctly wired into the api Deployment (PRD #2006)" >&2
  exit 1
fi

echo "OK: UZI_EPHEMERAL_LEASE renders 2h by default and for null, 0 survives, overrides take effect (PRD #2006)"


# Issue #2412: the adjacent ephemeral size knob is rendered on the same API
# Deployment. Keep its default, overrides and refusal cases in this offline lane.
check_size() {
  _label="$1"; _want="$2"; shift 2
  helm template uzi "$CHART" --show-only templates/api-deployment.yaml "$@" > "$WORK/size.yaml"
  _got=$(awk '
    /- name: UZI_EPHEMERAL_DEFAULT_SIZE$/ { hit = 1; next }
    hit && /value:/ { v = $2; gsub(/"/, "", v); print v; hit = 0 }
  ' "$WORK/size.yaml")
  if [ -z "$_got" ]; then
    echo "BROKEN ($_label): render emitted no UZI_EPHEMERAL_DEFAULT_SIZE" >&2
    exit 2
  fi
  if [ "$_got" != "$_want" ]; then
    echo "FAIL ($_label): UZI_EPHEMERAL_DEFAULT_SIZE is '$_got', expected exactly one '$_want'" >&2
    exit 1
  fi
  echo "OK ($_label): UZI_EPHEMERAL_DEFAULT_SIZE renders '$_got'"
}
check_size "default size" "m"
check_size "explicit small" "s" --set workers.ephemeralDefaultSize=s
check_size "explicit large" "l" --set workers.ephemeralDefaultSize=l
check_size "null secretEnv" "m" --set api.secretEnv=null
for invalid in unknown M null ''; do
  if helm template uzi "$CHART" --set "workers.ephemeralDefaultSize=$invalid" > "$WORK/invalid.yaml" 2> "$WORK/invalid.err"; then
    echo "FAIL: invalid ephemeral size '$invalid' rendered successfully" >&2
    exit 1
  fi
  if ! grep -qF 'workers.ephemeralDefaultSize must be one of s, m, l' "$WORK/invalid.err"; then
    echo "BROKEN: render failed without the expected size validation diagnostic" >&2
    exit 2
  fi
done
if helm template uzi "$CHART" --set api.secretEnv.UZI_EPHEMERAL_DEFAULT_SIZE=another-key > "$WORK/duplicate.yaml" 2> "$WORK/duplicate.err"; then
  echo "FAIL: secretEnv duplicated the chart-owned ephemeral size" >&2
  exit 1
fi
grep -qF 'Set UZI_EPHEMERAL_DEFAULT_SIZE through workers.ephemeralDefaultSize, not api.secretEnv' "$WORK/duplicate.err" || exit 2
if helm template uzi "$CHART" --set api.config.UZI_EPHEMERAL_DEFAULT_SIZE=l > "$WORK/config.yaml" 2> "$WORK/config.err"; then
  echo "FAIL: api.config set the chart-owned ephemeral size (it would be silently overridden)" >&2
  exit 1
fi
grep -qF 'Set UZI_EPHEMERAL_DEFAULT_SIZE through workers.ephemeralDefaultSize, not api.config' "$WORK/config.err" || exit 2
echo "OK: ephemeral size defaults to m, accepts s/l, tolerates null secretEnv and rejects invalid values and api.config/secretEnv duplicates"
