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
# FLAG-ONLY CASE (f): the other uzi.apiConfigMapEnabled triggers are nulled out with
# --set <key>=null (and database.mode=external), so the flag is the only reason left to render the ConfigMap. Its
# negative control (the same overrides without the flag must render NO api ConfigMap)
# proves the overrides really removed every other trigger; if it does not, the instrument
# is broken (exit 2).
#
# EXIT CODES:
#     2 = the instrument is broken (helm/yq/chart missing, a render that must succeed
#         failed, or a render failed for an unrelated reason)
#     1 = a property does not hold (an expected key absent included)
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
  bad "(b) UZI_ADMIN_RUN_DEBUG absent from a render with api.adminRunDebug.enabled=true"
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

expect_fail "(d) enabled + api.secretEnv.UZI_ADMIN_RUN_DEBUG" "Set UZI_ADMIN_RUN_DEBUG through api.adminRunDebug.enabled, not api.secretEnv" \
  --set api.adminRunDebug.enabled=true --set-string api.secretEnv.UZI_ADMIN_RUN_DEBUG=x

# --- (e) only a real boolean enables the flag -----------------------------------------------
_BOOL_MSG="api.adminRunDebug.enabled must be a boolean true/false"
expect_fail "(e) string \"false\"" "$_BOOL_MSG" --set-string api.adminRunDebug.enabled=false
expect_fail "(e) string \"true\"" "$_BOOL_MSG" --set-string api.adminRunDebug.enabled=true
expect_fail "(e) string \"0\"" "$_BOOL_MSG" --set-string api.adminRunDebug.enabled=0

# --- (f) flag-only: the flag alone keeps the ConfigMap and the Deployment envFrom ------------
# Null every other uzi.apiConfigMapEnabled trigger of the default values.
set -- --set api.config.API_ADDR=null --set releaseCheck=null --set database.mode=external
if ! helm template uzi "$CHART" --namespace uzi "$@" > "$WORK/ctl.yaml" 2> "$WORK/ctl.err"; then
  echo "BROKEN: the flag-less control render failed" >&2
  cat "$WORK/ctl.err" >&2
  exit 2
fi
if [ "$(yq 'select(.kind == "ConfigMap" and .metadata.name == "uzi-api-config") | .metadata.name' "$WORK/ctl.yaml")" != "" ]; then
  echo "BROKEN: the flag-less control still renders an api ConfigMap (another trigger remains; the flag-only case would be vacuous)" >&2
  exit 2
fi
# The whole render must succeed; the ConfigMap being absent from it is the property failing.
if ! helm template uzi "$CHART" --namespace uzi "$@" --set api.adminRunDebug.enabled=true > "$WORK/only-all.yaml" 2> "$WORK/only.err"; then
  echo "BROKEN: the flag-only render failed" >&2
  cat "$WORK/only.err" >&2
  exit 2
fi
yq 'select(.kind == "ConfigMap" and .metadata.name == "uzi-api-config")' "$WORK/only-all.yaml" > "$WORK/only.yaml"
yq 'select(.kind == "Deployment" and .metadata.name == "uzi-api")' "$WORK/only-all.yaml" > "$WORK/onlydep.yaml"
if [ "$(flag_key "$WORK/only.yaml")" = "true" ]; then
  ok "(f) flag alone renders the api ConfigMap with UZI_ADMIN_RUN_DEBUG"
else
  bad "(f) flag-only render lacks the api ConfigMap UZI_ADMIN_RUN_DEBUG"
fi
if [ "$(envfrom_name "$WORK/onlydep.yaml")" = "uzi-api-config" ]; then
  ok "(f) flag-only api Deployment envFroms the ConfigMap"
else
  bad "(f) flag-only api Deployment does not envFrom the ConfigMap"
fi

# A string value must trip the type guard on a flag-only install too (the ConfigMap template
# has to be reached for the guard to fire).
expect_fail "(f) flag-only string \"true\"" "$_BOOL_MSG" "$@" --set-string api.adminRunDebug.enabled=true

if [ "$fail" -ne 0 ]; then
  echo "FAIL: UZI_ADMIN_RUN_DEBUG is NOT correctly rendered from api.adminRunDebug.enabled" >&2
  exit 1
fi
echo "OK: UZI_ADMIN_RUN_DEBUG renders only from api.adminRunDebug.enabled, defaults off, and is reserved against api.config and api.secretEnv"
