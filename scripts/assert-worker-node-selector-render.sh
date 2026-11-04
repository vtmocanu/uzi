#!/bin/sh
# Assert the controller Deployment renders workers.nodeSelector into
# UZI_WORKER_NODE_SELECTOR as documented, and refuses a value that is neither a map
# nor null. Also asserts workers.tolerations renders into UZI_WORKER_TOLERATIONS
# (absent by default and when empty or null) and refuses a value that is neither a
# list nor null.
#
# usage: scripts/assert-worker-node-selector-render.sh [chart-dir]
#   e.g. scripts/assert-worker-node-selector-render.sh deploy/chart
#
# 🔴 WHY THIS EXISTS. The default (kubernetes.io/arch: amd64) keeps the amd64-only agent
# images off arm64 nodes. Three ways to lose it silently: a template edit that drops or
# renames the env, Helm's map merge (`{}` must KEEP the default, only `null` clears
# it), and a mistyped scalar that `with` would skip as if it were null. This renders
# each case and proves the wiring and the type guard hold.
#
# 🔴 OFFLINE, AND NON-VACUOUS, the same way assert-drain-knobs-render.sh is: the CNPG
# subchart is stripped from a temp copy, and every "no env" case first proves the
# controller Deployment rendered at all (a sentinel env is present), so an empty
# render can never read as a pass.
#
# EXIT CODES (the convention assert-drain-knobs-render.sh sets):
#     2 = the instrument is broken (helm/chart missing, or a render that should have
#         produced the controller Deployment did not)
#     1 = a case rendered the wrong thing
#     0 = every property holds
set -eu

SENTINEL="UZI_WORKER_NAMESPACE" # always rendered by the controller Deployment
AMD64='"{\"kubernetes.io/arch\":\"amd64\"}"'
ARM64='"{\"kubernetes.io/arch\":\"arm64\"}"'

CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

# An offline, dependency-free copy of the chart (see assert-drain-knobs-render.sh).
cp -R "$CHART_DIR" "$WORK/chart"
rm -f "$WORK/chart/Chart.lock"
awk '
  BEGIN { skip = 0 }
  /^dependencies:/ { skip = 1; next }
  skip && /^[^[:space:]-]/ { skip = 0 }
  skip { next }
  { print }
' "$CHART_DIR/Chart.yaml" > "$WORK/chart/Chart.yaml"

# render [helm args...]: the controller Deployment into $WORK/out, stderr into
# $WORK/err; returns helm's exit code.
render() {
  helm template uzi "$WORK/chart" \
    --set workers.enabled=true --set api.tls.enabled=true \
    --show-only templates/controller-deployment.yaml "$@" > "$WORK/out" 2> "$WORK/err"
}

# selector_value: the rendered UZI_WORKER_NODE_SELECTOR value, verbatim, or nothing.
selector_value() {
  awk '
    /- name: UZI_WORKER_NODE_SELECTOR$/ { hit = 1; next }
    hit && /value:/ { sub(/^[[:space:]]*value:[[:space:]]*/, ""); print; exit }
  ' "$WORK/out"
}

require_controller() {
  grep -q -- "- name: $SENTINEL\$" "$WORK/out" || {
    echo "BROKEN ($1): the controller Deployment did not render (no $SENTINEL); stderr:" >&2
    cat "$WORK/err" >&2
    exit 2
  }
}

fail=0

# expect_value <case> <want> [helm args...]
expect_value() {
  _case="$1"; _want="$2"; shift 2
  render "$@" || { echo "BROKEN ($_case): helm template failed:" >&2; cat "$WORK/err" >&2; exit 2; }
  require_controller "$_case"
  _got=$(selector_value)
  if [ "$_got" = "$_want" ]; then
    echo "OK ($_case): UZI_WORKER_NODE_SELECTOR = $_got"
  else
    echo "FAIL ($_case): UZI_WORKER_NODE_SELECTOR = '${_got:-<absent>}', want $_want" >&2
    fail=1
  fi
}

expect_value "default" "$AMD64"
expect_value "arm64 override" "$ARM64" --set-json 'workers.nodeSelector={"kubernetes.io/arch":"arm64"}'
# Helm deep-merges a user map into the default, so {} keeps kubernetes.io/arch: amd64.
expect_value "{} keeps the default" "$AMD64" --set-json 'workers.nodeSelector={}'

# null clears the selector: the controller renders, the env does not.
render --set-json 'workers.nodeSelector=null' || { echo "BROKEN (null): helm template failed:" >&2; cat "$WORK/err" >&2; exit 2; }
require_controller "null"
if [ -n "$(selector_value)" ]; then
  echo "FAIL (null): UZI_WORKER_NODE_SELECTOR rendered '$(selector_value)'; null must clear it" >&2
  fail=1
else
  echo "OK (null): no UZI_WORKER_NODE_SELECTOR"
fi

# Neither a map nor null: the render must refuse, with the guard's own message.
for bad in false 0 '""' '[]' '"kubernetes.io/arch=amd64"'; do
  if render --set-json "workers.nodeSelector=$bad"; then
    echo "FAIL (malformed $bad): rendered instead of refusing" >&2
    fail=1
  elif grep -q "workers.nodeSelector must be a map" "$WORK/err"; then
    echo "OK (malformed $bad): render refused"
  else
    echo "FAIL (malformed $bad): render failed, but not with the nodeSelector guard:" >&2
    cat "$WORK/err" >&2
    fail=1
  fi
done

# --- workers.tolerations -> UZI_WORKER_TOLERATIONS ---------------------------------

tolerations_value() {
  awk '
    /- name: UZI_WORKER_TOLERATIONS$/ { hit = 1; next }
    hit && /value:/ { sub(/^[[:space:]]*value:[[:space:]]*/, ""); print; exit }
  ' "$WORK/out"
}

# expect_no_tolerations <case> [helm args...]
expect_no_tolerations() {
  _case="$1"; shift
  render "$@" || { echo "BROKEN (tolerations $_case): helm template failed:" >&2; cat "$WORK/err" >&2; exit 2; }
  require_controller "tolerations $_case"
  if [ -n "$(tolerations_value)" ]; then
    echo "FAIL (tolerations $_case): UZI_WORKER_TOLERATIONS rendered '$(tolerations_value)'; want none" >&2
    fail=1
  else
    echo "OK (tolerations $_case): no UZI_WORKER_TOLERATIONS"
  fi
}

expect_no_tolerations "default"
expect_no_tolerations "[]" --set-json 'workers.tolerations=[]'
expect_no_tolerations "null" --set-json 'workers.tolerations=null'

TOL_WANT='"[{\"effect\":\"NoSchedule\",\"key\":\"uzi\",\"operator\":\"Equal\",\"value\":\"true\"}]"'
render --set-json 'workers.tolerations=[{"key":"uzi","operator":"Equal","value":"true","effect":"NoSchedule"}]' \
  || { echo "BROKEN (tolerations set): helm template failed:" >&2; cat "$WORK/err" >&2; exit 2; }
require_controller "tolerations set"
if [ "$(tolerations_value)" = "$TOL_WANT" ]; then
  echo "OK (tolerations set): UZI_WORKER_TOLERATIONS = $(tolerations_value)"
else
  echo "FAIL (tolerations set): UZI_WORKER_TOLERATIONS = '$(tolerations_value)', want $TOL_WANT" >&2
  fail=1
fi
# Setting tolerations must not disturb the nodeSelector default.
if [ "$(selector_value)" != "$AMD64" ]; then
  echo "FAIL (tolerations set): UZI_WORKER_NODE_SELECTOR = '$(selector_value)', want $AMD64" >&2
  fail=1
fi

for bad in false 0 '""' '{}' '{"key":"uzi"}'; do
  if render --set-json "workers.tolerations=$bad"; then
    echo "FAIL (tolerations malformed $bad): rendered instead of refusing" >&2
    fail=1
  elif grep -q "workers.tolerations must be a list" "$WORK/err"; then
    echo "OK (tolerations malformed $bad): render refused"
  else
    echo "FAIL (tolerations malformed $bad): render failed, but not with the tolerations guard:" >&2
    cat "$WORK/err" >&2
    fail=1
  fi
done

exit "$fail"
