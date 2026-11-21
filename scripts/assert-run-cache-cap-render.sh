#!/bin/sh
# Assert the chart relays the per-run cache cap settings (issue #4576) from
# workers.runCacheCapMaxBytes / workers.runCacheCapFraction to the controller container's
# UZI_WORKER_RUN_CACHE_CAP_MAX_BYTES / UZI_WORKER_RUN_CACHE_CAP_FRACTION, verbatim and only
# as strings, and that docker-compose passes the worker-side names through.
#
# usage: scripts/assert-run-cache-cap-render.sh [chart-dir]
#
# The repo and chart are resolved from this script's own directory, so a copy inside an
# export checks that export.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the CNPG
# subchart dependency stripped. A render that must succeed but fails, or a failing-render
# case that fails for a different reason than the one asserted, is a BROKEN INSTRUMENT.
#
# EXIT CODES:
#     2 = the instrument is broken (helm/yq/chart missing, a render that must succeed
#         failed, or a render failed for an unrelated reason)
#     1 = a property does not hold
#     0 = every property holds
set -eu

CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
CHART_DIR="${1:-$REPO_DIR/deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
command -v yq >/dev/null 2>&1 || { echo "BROKEN: yq (mikefarah v4) not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }
[ -f "$REPO_DIR/docker-compose.yml" ] || { echo "BROKEN: no docker-compose.yml under $REPO_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

CHART="$WORK/chart"
cp -R "$CHART_DIR" "$CHART"
rm -f "$CHART/Chart.lock"
rm -rf "$CHART/charts"
awk '
  BEGIN { skip = 0 }
  /^dependencies:/ { skip = 1; next }
  skip && /^[^[:space:]-]/ { skip = 0 }
  skip { next }
  { print }
' "$CHART_DIR/Chart.yaml" > "$CHART/Chart.yaml"

fail=0
ok() { echo "OK: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

MAX_KEY=UZI_WORKER_RUN_CACHE_CAP_MAX_BYTES
FRAC_KEY=UZI_WORKER_RUN_CACHE_CAP_FRACTION
MUST_BE_STRING="must be a string"

# render <out> [helm args...]: the controller Deployment; a render that must succeed.
render() {
  _out="$1"; shift
  if ! helm template uzi "$CHART" --namespace uzi --show-only templates/controller-deployment.yaml \
    --set workers.enabled=true --set api.tls.enabled=true "$@" > "$_out" 2> "$_out.err"; then
    echo "BROKEN: a render that must succeed failed: helm template $*" >&2
    cat "$_out.err" >&2
    exit 2
  fi
}

# env_of <file> <name>: the controller container's env value, or "<absent>".
env_of() {
  yq -r '.spec.template.spec.containers[] | select(.name == "controller") | .env
    | map(select(.name == "'"$2"'")) | .[0].value // "<absent>"' "$1"
}

# expect <label> <file> <want-max> <want-frac>: exact values ("<absent>" for none).
expect() {
  _gm=$(env_of "$2" "$MAX_KEY")
  _gf=$(env_of "$2" "$FRAC_KEY")
  if [ "$_gm" = "$3" ] && [ "$_gf" = "$4" ]; then
    ok "$1"
  else
    bad "$1: max='$_gm' fraction='$_gf', expected max='$3' fraction='$4'"
  fi
}

# expect_fail <label> [helm args...]: the render must fail with "must be a string".
expect_fail() {
  _label="$1"; shift
  if helm template uzi "$CHART" --namespace uzi --set workers.enabled=true --set api.tls.enabled=true \
    "$@" > "$WORK/f.out" 2> "$WORK/f.err"; then
    bad "$_label rendered but must fail"
  elif grep -F -q -- "$MUST_BE_STRING" "$WORK/f.err"; then
    ok "$_label fails the render"
  else
    echo "BROKEN: $_label failed for an unrelated reason (wanted '$MUST_BE_STRING'): $(head -c 300 "$WORK/f.err")" >&2
    exit 2
  fi
}

# --- default: neither key is rendered; the controller env is non-empty (non-vacuous) -----------
render "$WORK/default.yaml"
if [ "$(env_of "$WORK/default.yaml" UZI_WORKER_MAX_CONCURRENT_RUNS)" = "<absent>" ]; then
  echo "BROKEN: the controller env lacks UZI_WORKER_MAX_CONCURRENT_RUNS (the absence checks would be vacuous)" >&2
  exit 2
fi
expect "default renders neither cap key" "$WORK/default.yaml" "<absent>" "<absent>"

# --- --set-string max ----------------------------------------------------------------------------
render "$WORK/c.yaml" --set-string workers.runCacheCapMaxBytes=21474836480
expect "--set-string max renders exactly 21474836480" "$WORK/c.yaml" "21474836480" "<absent>"

# --- values file, quoted -------------------------------------------------------------------------
printf 'workers:\n  runCacheCapMaxBytes: "21474836480"\n' > "$WORK/v.yaml"
render "$WORK/c.yaml" -f "$WORK/v.yaml"
expect "values file quoted max renders exactly 21474836480" "$WORK/c.yaml" "21474836480" "<absent>"

# --- verbatim strings (leading and trailing spaces survive) ---------------------------------------
for v in ' OFF ' '0' 'abc'; do
  render "$WORK/c.yaml" --set-string "workers.runCacheCapMaxBytes=$v"
  expect "--set-string max '$v' is verbatim" "$WORK/c.yaml" "$v" "<absent>"
  render "$WORK/c.yaml" --set-string "workers.runCacheCapFraction=$v"
  expect "--set-string fraction '$v' is verbatim" "$WORK/c.yaml" "<absent>" "$v"
done

# --- fraction via --set (helm keeps 0.25 as a string) ---------------------------------------------
render "$WORK/c.yaml" --set workers.runCacheCapFraction=0.25
expect "--set fraction renders 0.25" "$WORK/c.yaml" "<absent>" "0.25"

# --- both set at once -----------------------------------------------------------------------------
render "$WORK/c.yaml" --set-string workers.runCacheCapMaxBytes=off --set-string workers.runCacheCapFraction=0.25
expect "both set render both keys" "$WORK/c.yaml" "off" "0.25"

# --- empty string is absent ------------------------------------------------------------------------
render "$WORK/c.yaml" --set-string workers.runCacheCapMaxBytes=
expect "--set-string max empty renders nothing" "$WORK/c.yaml" "<absent>" "<absent>"
render "$WORK/c.yaml" --set-string workers.runCacheCapFraction=
expect "--set-string fraction empty renders nothing" "$WORK/c.yaml" "<absent>" "<absent>"

# --- typed values fail the render, for both keys ----------------------------------------------------
for key in runCacheCapMaxBytes runCacheCapFraction; do
  expect_fail "typed --set $key=21474836480" --set "workers.$key=21474836480"
  expect_fail "typed --set $key=1" --set "workers.$key=1"
  expect_fail "typed --set $key=true" --set "workers.$key=true"
  printf 'workers:\n  %s: 21474836480\n' "$key" > "$WORK/v.yaml"
  expect_fail "values file unquoted number $key" -f "$WORK/v.yaml"
  printf 'workers:\n  %s: off\n' "$key" > "$WORK/v.yaml"
  expect_fail "values file unquoted off $key" -f "$WORK/v.yaml"
done

# --- validation is unconditional: controller off still fails (worker-invariants) --------------------
for key in runCacheCapMaxBytes runCacheCapFraction; do
  expect_fail "controller disabled + typed $key" --set workers.controller.enabled=false --set "workers.$key=1"
done

# --- compose inventory ------------------------------------------------------------------------------
_cm=$(yq -r '.services.agent.environment.UZI_RUN_CACHE_CAP_MAX_BYTES' "$REPO_DIR/docker-compose.yml")
_cf=$(yq -r '.services.agent.environment.UZI_RUN_CACHE_CAP_FRACTION' "$REPO_DIR/docker-compose.yml")
# The ${...} is the literal compose interpolation text, not a shell expansion.
# shellcheck disable=SC2016
if [ "$_cm" = '${UZI_RUN_CACHE_CAP_MAX_BYTES:-}' ] && [ "$_cf" = '${UZI_RUN_CACHE_CAP_FRACTION:-}' ]; then
  ok "compose agent passes UZI_RUN_CACHE_CAP_MAX_BYTES and UZI_RUN_CACHE_CAP_FRACTION, empty by default"
else
  bad "compose agent cap env is max='$_cm' fraction='$_cf'"
fi

if [ "$fail" -ne 0 ]; then
  echo "FAIL: the per-run cache cap settings are NOT correctly relayed" >&2
  exit 1
fi
echo "OK: workers.runCacheCapMaxBytes/runCacheCapFraction relay verbatim as strings, default to absent, refuse typed values and pass through compose"
