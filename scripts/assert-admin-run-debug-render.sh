#!/bin/sh
# Assert the chart renders the api's UZI_ADMIN_RUN_DEBUG (the default-off admin run debug
# read flag, issue #4198) from the first-class api.adminRunDebug.enabled value only.
#
# usage: scripts/assert-admin-run-debug-render.sh [chart-dir]
#
# WHY THIS EXISTS. The flag widens what an admin CLI token can read across users, so a
# default that rendered it, a lost key, a flag-only install whose ConfigMap is never
# mounted, or a freeform api.config / api.secretEnv value that bypasses the first-class
# value would all render and lint clean. Every property is asserted on the parsed render.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the CNPG
# subchart dependency stripped. A render that must succeed but fails, or a failing-render
# case that fails for a different reason than the one asserted, is a BROKEN INSTRUMENT
# (exit 2), never a pass.
#
# NOT ASSERTED: that the flag alone keeps the ConfigMap. api.config cannot be emptied (a
# values override merges into the default map, and null breaks the template's hasKey), and
# the default chart carries other ConfigMap triggers, so the flag's membership in
# uzi.apiConfigMapEnabled is not observable here. Case (b) still proves the rendered flag
# reaches the pod through the Deployment envFrom.
#
# EXIT CODES:
#     2 = the instrument is broken (helm/yq/chart missing, a render that must succeed
#         failed, an expected key absent, or a render failed for an unrelated reason)
#     1 = a property does not hold
#     0 = every property holds
set -eu

CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
command -v yq >/dev/null 2>&1 || { echo "BROKEN: yq (mikefarah v4) not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

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

# render <out> <template> [helm args...]: a render that must succeed; exit 2 if it did not.
render() {
  _out="$1"; _tpl="$2"; shift 2
  if ! helm template uzi "$CHART" --namespace uzi --show-only "templates/$_tpl" "$@" > "$_out" 2> "$_out.err"; then
    echo "BROKEN: a render that must succeed failed: helm template $*" >&2
    cat "$_out.err" >&2
    exit 2
  fi
}

# flag_key <file>: the rendered UZI_ADMIN_RUN_DEBUG, or "ABSENT".
flag_key() {
  yq '.data.UZI_ADMIN_RUN_DEBUG // "ABSENT"' "$1"
}

# envfrom_name <deployment-file>: the api container's first envFrom configMapRef name, or "ABSENT".
envfrom_name() {
  yq 'select(.kind == "Deployment") | .spec.template.spec.containers[0].envFrom[0].configMapRef.name // "ABSENT"' "$1"
}

# expect_fail <label> <message-fragment> [helm args...]: the render must fail naming the
# fragment; a failure without it means the instrument is broken, not that the guard works.
expect_fail() {
  _label="$1"; _frag="$2"; shift 2
  if helm template uzi "$CHART" --namespace uzi "$@" > "$WORK/f.out" 2> "$WORK/f.err"; then
    bad "$_label rendered but must fail"
  elif grep -F -q -- "$_frag" "$WORK/f.err"; then
    ok "$_label fails the render"
  else
    echo "BROKEN: $_label failed for an unrelated reason (wanted '$_frag'): $(head -c 300 "$WORK/f.err")" >&2
    exit 2
  fi
}

# --- (a) default: the key is absent ---------------------------------------------------------
render "$WORK/cm.yaml" api-configmap.yaml
if [ "$(yq '.data | length' "$WORK/cm.yaml")" -lt 1 ]; then
  echo "BROKEN: the default api ConfigMap has no data (the absence check would be vacuous)" >&2
  exit 2
fi
if [ "$(flag_key "$WORK/cm.yaml")" = "ABSENT" ]; then
  ok "(a) default renders no UZI_ADMIN_RUN_DEBUG"
else
  bad "(a) default rendered UZI_ADMIN_RUN_DEBUG '$(flag_key "$WORK/cm.yaml")'"
fi

# --- (b) enabled renders "true" and the api Deployment envFroms the ConfigMap ---------------
render "$WORK/on.yaml" api-configmap.yaml --set api.adminRunDebug.enabled=true
_got=$(flag_key "$WORK/on.yaml")
if [ "$_got" = "ABSENT" ]; then
  echo "BROKEN: UZI_ADMIN_RUN_DEBUG absent from a render with api.adminRunDebug.enabled=true" >&2
  exit 2
elif [ "$_got" = "true" ]; then
  ok "(b) enabled renders UZI_ADMIN_RUN_DEBUG: \"true\""
else
  bad "(b) rendered '$_got', expected 'true'"
fi
_cm_name=$(yq '.metadata.name' "$WORK/on.yaml")
render "$WORK/dep.yaml" api-deployment.yaml --set api.adminRunDebug.enabled=true
_ref=$(envfrom_name "$WORK/dep.yaml")
if [ "$_ref" = "$_cm_name" ]; then
  ok "(b) the api Deployment envFroms $_cm_name"
else
  bad "(b) api Deployment envFrom is '$_ref', expected '$_cm_name'"
fi

# --- (c) a quoted string, not a bool/number --------------------------------------------------
if [ "$(yq '.data.UZI_ADMIN_RUN_DEBUG | tag' "$WORK/on.yaml")" = "!!str" ]; then
  ok "(c) the value renders as a string"
else
  bad "(c) the value does not render as a string"
fi

# --- (d) the key is reserved against the freeform maps ---------------------------------------
expect_fail "(d) disabled + api.config.UZI_ADMIN_RUN_DEBUG" "Set UZI_ADMIN_RUN_DEBUG through api.adminRunDebug.enabled, not api.config" \
  --set api.adminRunDebug.enabled=false --set-string api.config.UZI_ADMIN_RUN_DEBUG=true
expect_fail "(d) enabled + api.config.UZI_ADMIN_RUN_DEBUG" "Set UZI_ADMIN_RUN_DEBUG through api.adminRunDebug.enabled, not api.config" \
  --set api.adminRunDebug.enabled=true --set-string api.config.UZI_ADMIN_RUN_DEBUG=true
expect_fail "(d) api.secretEnv.UZI_ADMIN_RUN_DEBUG" "Set UZI_ADMIN_RUN_DEBUG through api.adminRunDebug.enabled, not api.secretEnv" \
  --set-string api.secretEnv.UZI_ADMIN_RUN_DEBUG=x

if [ "$fail" -ne 0 ]; then
  echo "FAIL: UZI_ADMIN_RUN_DEBUG is NOT correctly rendered from api.adminRunDebug.enabled" >&2
  exit 1
fi
echo "OK: UZI_ADMIN_RUN_DEBUG renders only from api.adminRunDebug.enabled, defaults off, and is reserved against api.config and api.secretEnv"
