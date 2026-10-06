#!/bin/sh
# Offline, nonvacuous Docker tier env assertions. Scratch lives in the system temp dir.
set -eu

# Resolve the chart dir: explicit arg, else relative to this script (../deploy/chart).
# Clear CDPATH first so `cd` cannot resolve via it and echo an unexpected directory.
CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/docker-render.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

# Build an offline, dependency-free copy of the chart.
strip_chart() {
  _dst="$1"
  cp -R "$CHART_DIR" "$_dst"
  rm -f "$_dst/Chart.lock"
  rm -rf "$_dst/charts"
  awk '
    BEGIN { skip = 0 }
    /^dependencies:/ { skip = 1; next }        # drop the dependencies: list ...
    skip && /^[^[:space:]-]/ { skip = 0 }      # ... until the next top-level key
    skip { next }
    { print }
  ' "$CHART_DIR/Chart.yaml" > "$_dst/Chart.yaml"
}

# Render with the API TLS prerequisite enabled and extract WORKER_DOCKER_ENABLED.
# Print nothing
# (the caller treats empty as a broken instrument). Extra --set args are forwarded.
render_docker() {
  _chart="$1"; shift
  helm template uzi "$_chart" --set api.tls.enabled=true \
    --set workers.docker.networkPolicy.enabled=true \
    --set workers.docker.networkPolicy.podCIDR=10.244.0.0/16 \
    --set workers.docker.networkPolicy.serviceCIDR=10.96.0.0/12 \
    --set workers.docker.networkPolicy.linkLocalCIDR=169.254.0.0/16 \
    --show-only templates/api-deployment.yaml "$@" > "$WORK/render.yaml" || return 2
  awk '
      /- name: WORKER_DOCKER_ENABLED$/ { hit = 1; next }
      hit && /value:/ { v = $2; gsub(/"/, "", v); print v; hit = 0 }
  ' "$WORK/render.yaml"
}

CHART="$WORK/chart"
strip_chart "$CHART"

fail=0

# check <label> <expected> [--set args...]
check() {
  _label="$1"; _want="$2"; shift 2
  _got=$(render_docker "$CHART" "$@")
  if [ -z "$_got" ]; then
    echo "BROKEN: WORKER_DOCKER_ENABLED was absent from the render for case '$_label'" >&2
    exit 2
  fi
  if [ "$_got" != "$_want" ]; then
    echo "FAIL ($_label): WORKER_DOCKER_ENABLED is '$_got', expected '$_want'" >&2
    fail=1
  else
    echo "OK ($_label): WORKER_DOCKER_ENABLED renders '$_got'"
  fi
}

# Cases are independent; every case runs after a value mismatch, but a broken
# render stops the table. Helm is invoked once per case, with no retry.
check "default" "false"
check "tier on" "true" --set workers.enabled=true,workers.controller.enabled=true,workers.docker.enabled=true
check "tier off" "false" --set workers.enabled=true,workers.controller.enabled=true,workers.docker.enabled=false
check "hosting off" "false" --set workers.enabled=false,workers.controller.enabled=true,workers.docker.enabled=true
check "controller off" "false" --set workers.enabled=true,workers.controller.enabled=false,workers.docker.enabled=true
[ "$fail" -eq 0 ] || exit 1
echo "OK: effective Docker tier reaches the api Deployment"
