#!/bin/sh
# Assert the chart hands the api the hosted-worker size ceilings and quotas as INTEGERS
# (PRD #4213, "Ceilings"), and the controller the raw ceiling quantities.
#
# usage: scripts/assert-worker-size-ceilings-render.sh [chart-dir]
#
# WHY THIS EXISTS. The api validates admin-defined worker sizes against each enabled
# tier's LimitRange ceiling, and the chart is the only place that knows them. The
# conversion (uzi.quantityFloorUnits in _helpers.tpl) is exact integer arithmetic that
# FLOORS, because a ceiling rounded up would admit a size the cluster then rejects:
# 1048575500m is 1048575.5 bytes, which rounds up to 1 MiB while the floor is 0 MiB. A
# conversion derived from uzi.quantityToBytes (which rounds up) would render clean and
# pass every default-values check, so the boundaries are asserted here on the render. Quota
# conversion is the opposite on failure: it must never fail a render that renders today.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the CNPG
# subchart dependency stripped, as assert-pvc-expansion-render.sh does. An expected env that
# is absent where it must be present is a failed assertion, and a render without the api or
# controller Deployment is a BROKEN INSTRUMENT (exit 2), never a pass.
#
# EXIT CODES (the assert-db-capacity-render.sh convention):
#     2 = the instrument is broken (helm/yq/chart missing, a render that must succeed
#         failed, or an expected object absent)
#     1 = a property does not hold
#     0 = every property holds
set -eu

CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"
VALUES="$SCRIPT_DIR/../deploy/values/ci-render.yaml"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
command -v yq >/dev/null 2>&1 || { echo "BROKEN: yq (mikefarah v4) not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }
[ -f "$VALUES" ] || { echo "BROKEN: no render values at $VALUES" >&2; exit 2; }

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

# render <out> [helm args...]: the full render; exit 2 if it must succeed and did not.
render() {
  _out="$1"; shift
  if ! helm template uzi "$CHART" --namespace uzi -f "$VALUES" "$@" > "$_out" 2> "$_out.err"; then
    echo "BROKEN: a render that must succeed failed: helm template $*" >&2
    cat "$_out.err" >&2
    exit 2
  fi
}

# expect_fail <label> <message-fragment> [helm args...]: the render must fail naming the fragment.
expect_fail() {
  _label="$1"; _frag="$2"; shift 2
  if helm template uzi "$CHART" --namespace uzi -f "$VALUES" "$@" > "$WORK/f.out" 2> "$WORK/f.err"; then
    bad "$_label rendered but must fail"
  elif grep -F -q -- "$_frag" "$WORK/f.err"; then
    ok "$_label fails the render naming $_frag"
  else
    bad "$_label failed, but without '$_frag': $(head -c 300 "$WORK/f.err")"
  fi
}

# api_env <file> <NAME>: the api Deployment's env value, or "ABSENT". The api Deployment is
# the one carrying HOSTED_WORKER_VERSION.
api_env() {
  yq 'select(.kind == "Deployment") | select(.spec.template.spec.containers[].env[].name == "HOSTED_WORKER_VERSION") | .spec.template.spec.containers[].env[] | select(.name == "'"$2"'") | .value' "$1" \
    | awk 'NF { print; found = 1 } END { if (!found) print "ABSENT" }'
}

# ctl_env <file> <NAME>: the controller Deployment's env value, or "ABSENT".
ctl_env() {
  yq 'select(.kind == "Deployment") | select(.spec.template.spec.containers[].env[].name == "UZI_WORKER_MAX_CONCURRENT_RUNS") | .spec.template.spec.containers[].env[] | select(.name == "'"$2"'") | .value' "$1" \
    | awk 'NF { print; found = 1 } END { if (!found) print "ABSENT" }'
}

# want_api <label> <file> <NAME> <want>: the api env must equal want ("ABSENT" = no env).
want_api() {
  _got=$(api_env "$2" "$3")
  if [ "$_got" = "$4" ]; then ok "$1: $3=$4"; else bad "$1: $3 is '$_got', want '$4'"; fi
}
want_ctl() {
  _got=$(ctl_env "$2" "$3")
  if [ "$_got" = "$4" ]; then ok "$1: $3=$4"; else bad "$1: $3 is '$_got', want '$4'"; fi
}

# check_set <label> <env-suffix> <want> [helm args...]: render with the args and require
# the restricted-tier api env UZI_WORKER_SIZE_CEILING_<suffix> to equal want.
check_set() {
  _label="$1"; _suffix="$2"; _want="$3"; shift 3
  render "$WORK/c.yaml" "$@"
  want_api "$_label" "$WORK/c.yaml" "UZI_WORKER_SIZE_CEILING_$_suffix" "$_want"
}

P=UZI_WORKER_SIZE
RL=workers.limitRange
# A tiny maxPVCStorage trips worker-invariants.yaml's run-bound /data guard unless the
# run-bound claim is lowered with it; EDS keeps that guard quiet so the ceiling is what is tested.
EDS="--set workers.ephemeralWorkerDataSize=1Gi"

# --- (a) default render ---------------------------------------------------------------------
render "$WORK/default.yaml"
[ "$(api_env "$WORK/default.yaml" "${P}_TIERS")" != ABSENT ] \
  || { echo "BROKEN: the api Deployment carries no ${P}_TIERS in the default render" >&2; exit 2; }
[ "$(ctl_env "$WORK/default.yaml" UZI_WORKER_MAX_PVC_STORAGE)" != ABSENT ] \
  || { echo "BROKEN: the controller Deployment is not in the default render" >&2; exit 2; }
D="$WORK/default.yaml"
want_api "(a) default" "$D" "${P}_TIERS" "restricted,docker,isolated"
want_api "(a) default" "$D" "${P}_CEILING_RESTRICTED_CPU_MILLICORES" 4000
want_api "(a) default" "$D" "${P}_CEILING_RESTRICTED_MEMORY_MIB" 32768
want_api "(a) default" "$D" "${P}_CEILING_RESTRICTED_PVC_GIB" 50
want_api "(a) default" "$D" "${P}_CEILING_DOCKER_CPU_MILLICORES" 4000
want_api "(a) default" "$D" "${P}_CEILING_DOCKER_MEMORY_MIB" 32768
want_api "(a) default" "$D" "${P}_CEILING_DOCKER_PVC_GIB" 50
want_api "(a) default" "$D" "${P}_CEILING_ISOLATED_CPU_MILLICORES" 4000
want_api "(a) default" "$D" "${P}_CEILING_ISOLATED_MEMORY_MIB" 32768
want_api "(a) default" "$D" "${P}_CEILING_ISOLATED_PVC_GIB" ABSENT
# Quota defaults: restricted 20 CPU / 280Gi / 20 deployments, docker 14 / 144Gi / 10, isolated 10 / 140Gi / 10.
want_api "(a) default" "$D" "${P}_QUOTA_RESTRICTED_REQUESTS_CPU_MILLICORES" 20000
want_api "(a) default" "$D" "${P}_QUOTA_RESTRICTED_REQUESTS_MEMORY_MIB" 286720
want_api "(a) default" "$D" "${P}_QUOTA_RESTRICTED_DEPLOYMENTS" 20
want_api "(a) default" "$D" "${P}_QUOTA_DOCKER_REQUESTS_CPU_MILLICORES" 14000
want_api "(a) default" "$D" "${P}_QUOTA_DOCKER_REQUESTS_MEMORY_MIB" 147456
want_api "(a) default" "$D" "${P}_QUOTA_DOCKER_DEPLOYMENTS" 10
want_api "(a) default" "$D" "${P}_QUOTA_ISOLATED_REQUESTS_CPU_MILLICORES" 10000
want_api "(a) default" "$D" "${P}_QUOTA_ISOLATED_REQUESTS_MEMORY_MIB" 143360
want_api "(a) default" "$D" "${P}_QUOTA_ISOLATED_DEPLOYMENTS" 10
want_api "(a) default" "$D" UZI_WORKER_DIND_DATA_SIZE_OVERRIDDEN ABSENT

# --- (b) disabled tiers render no env ------------------------------------------------------
render "$WORK/nodocker.yaml" --set workers.docker.enabled=false
want_api "(b) docker off" "$WORK/nodocker.yaml" "${P}_TIERS" "restricted,isolated"
want_api "(b) docker off" "$WORK/nodocker.yaml" "${P}_CEILING_DOCKER_CPU_MILLICORES" ABSENT
want_api "(b) docker off" "$WORK/nodocker.yaml" "${P}_QUOTA_DOCKER_DEPLOYMENTS" ABSENT
want_api "(b) docker off" "$WORK/nodocker.yaml" "${P}_CEILING_ISOLATED_CPU_MILLICORES" 4000
render "$WORK/noiso.yaml" --set workers.isolatedLane.enabled=false
want_api "(b) isolated off" "$WORK/noiso.yaml" "${P}_TIERS" "restricted,docker"
want_api "(b) isolated off" "$WORK/noiso.yaml" "${P}_CEILING_ISOLATED_CPU_MILLICORES" ABSENT
want_api "(b) isolated off" "$WORK/noiso.yaml" "${P}_QUOTA_ISOLATED_REQUESTS_MEMORY_MIB" ABSENT
want_api "(b) isolated off" "$WORK/noiso.yaml" "${P}_CEILING_DOCKER_CPU_MILLICORES" 4000
want_ctl "(b) isolated off" "$WORK/noiso.yaml" UZI_WORKER_ISOLATED_MAX_CPU ABSENT
render "$WORK/nolr.yaml" --set workers.docker.limitRange.enabled=false --set workers.docker.quota.enabled=false
want_api "(b) docker limitRange+quota off" "$WORK/nolr.yaml" "${P}_TIERS" "restricted,docker,isolated"
want_api "(b) docker limitRange+quota off" "$WORK/nolr.yaml" "${P}_CEILING_DOCKER_PVC_GIB" ABSENT
want_api "(b) docker limitRange+quota off" "$WORK/nolr.yaml" "${P}_QUOTA_DOCKER_REQUESTS_CPU_MILLICORES" ABSENT
want_ctl "(b) docker limitRange off" "$WORK/nolr.yaml" UZI_WORKER_DOCKER_MAX_CPU ABSENT

# --- (c) ceiling boundaries: the conversion FLOORS ----------------------------------------
expect_fail "(c) max.memory 1048575500m (floors to 0 MiB)" "$RL.max.memory" --set "$RL.max.memory=1048575500m"
expect_fail "(c) maxPVCStorage 1073741823 (floors to 0 GiB)" "$RL.maxPVCStorage" $EDS --set "$RL.maxPVCStorage=1073741823"
check_set "(c) maxPVCStorage 1073741824" RESTRICTED_PVC_GIB 1 $EDS --set "$RL.maxPVCStorage=1073741824"
check_set "(c) max.memory 32767.9Mi" RESTRICTED_MEMORY_MIB 32767 --set "$RL.max.memory=32767.9Mi"
check_set "(c) max.memory 1.5Gi" RESTRICTED_MEMORY_MIB 1536 --set "$RL.max.memory=1.5Gi"
check_set "(c) maxPVCStorage 1.5Gi" RESTRICTED_PVC_GIB 1 $EDS --set "$RL.maxPVCStorage=1.5Gi"
check_set "(c) max.cpu 999999u" RESTRICTED_CPU_MILLICORES 999 --set "$RL.max.cpu=999999u"
check_set "(c) max.cpu 4.0005" RESTRICTED_CPU_MILLICORES 4000 --set "$RL.max.cpu=4.0005"
check_set "(c) max.memory 33554432Ki" RESTRICTED_MEMORY_MIB 32768 --set "$RL.max.memory=33554432Ki"
check_set "(c) max.memory 1e9" RESTRICTED_MEMORY_MIB 953 --set "$RL.max.memory=1e9"
check_set "(c) max.cpu 1500m" RESTRICTED_CPU_MILLICORES 1500 --set "$RL.max.cpu=1500m"
check_set "(c) max.cpu 2.5" RESTRICTED_CPU_MILLICORES 2500 --set "$RL.max.cpu=2.5"
# The other two tiers use the same helper: spot-check their keys.
render "$WORK/c.yaml" --set workers.docker.limitRange.max.memory=1.5Gi --set workers.docker.limitRange.maxPVCStorage=50Gi \
  --set workers.isolatedLane.limitRange.max.cpu=1500m
want_api "(c) docker max.memory 1.5Gi" "$WORK/c.yaml" "${P}_CEILING_DOCKER_MEMORY_MIB" 1536
want_api "(c) isolated max.cpu 1500m" "$WORK/c.yaml" "${P}_CEILING_ISOLATED_CPU_MILLICORES" 1500
expect_fail "(c) isolated max.memory 0" "workers.isolatedLane.limitRange.max.memory" --set workers.isolatedLane.limitRange.max.memory=0
expect_fail "(c) docker max.cpu abc" "workers.docker.limitRange.max.cpu" --set workers.docker.limitRange.max.cpu=abc

# --- (d) ceiling failures name the path -----------------------------------------------------
for v in abc -1Gi 1Xi 9999999Ei; do
  expect_fail "(d) max.memory $v" "$RL.max.memory" --set "$RL.max.memory=$v"
done
expect_fail "(d) max.cpu abc" "$RL.max.cpu" --set "$RL.max.cpu=abc"
expect_fail "(d) maxPVCStorage -1Gi" "$RL.maxPVCStorage" $EDS --set "$RL.maxPVCStorage=-1Gi"
expect_fail "(d) maxPVCStorage 9999999999Ei" "$RL.maxPVCStorage" $EDS --set "$RL.maxPVCStorage=9999999999Ei"

# --- (e) zero quotas render 0 and never fail ---------------------------------------------
quota_zero() {
  _label="$1"; _tier="$2"; _prefix="$3"; _key="$4"; _val="$5"; _env="$6"
  render "$WORK/q.yaml" --set "$_prefix.quota.$_key=$_val"
  want_api "(e) $_label $_key=$_val" "$WORK/q.yaml" "${P}_QUOTA_${_tier}_$_env" 0
}
for t in "RESTRICTED workers" "DOCKER workers.docker" "ISOLATED workers.isolatedLane"; do
  set -- $t
  quota_zero "$1" "$1" "$2" requestsCPU 0 REQUESTS_CPU_MILLICORES
  quota_zero "$1" "$1" "$2" requestsCPU 0.0001 REQUESTS_CPU_MILLICORES
  quota_zero "$1" "$1" "$2" requestsMemory 0 REQUESTS_MEMORY_MIB
  quota_zero "$1" "$1" "$2" requestsMemory 1 REQUESTS_MEMORY_MIB
  quota_zero "$1" "$1" "$2" deployments 0 DEPLOYMENTS
done

# --- (f) malformed quotas render and omit only that env ----------------------------------
for t in "RESTRICTED workers" "DOCKER workers.docker" "ISOLATED workers.isolatedLane"; do
  set -- $t
  render "$WORK/m.yaml" --set "$2.quota.requestsCPU=lots" --set "$2.quota.requestsMemory=-5Gi" \
    --set "$2.quota.deployments=many"
  want_api "(f) $1 requestsCPU lots" "$WORK/m.yaml" "${P}_QUOTA_$1_REQUESTS_CPU_MILLICORES" ABSENT
  want_api "(f) $1 requestsMemory -5Gi" "$WORK/m.yaml" "${P}_QUOTA_$1_REQUESTS_MEMORY_MIB" ABSENT
  want_api "(f) $1 deployments many" "$WORK/m.yaml" "${P}_QUOTA_$1_DEPLOYMENTS" ABSENT
done
render "$WORK/m.yaml" --set workers.quota.requestsCPU=99999999999Ei
want_api "(f) overflowing quota omits the key" "$WORK/m.yaml" "${P}_QUOTA_RESTRICTED_REQUESTS_CPU_MILLICORES" ABSENT
want_api "(f) other quota keys stay" "$WORK/m.yaml" "${P}_QUOTA_RESTRICTED_DEPLOYMENTS" 20

# --- (g) the dind override flag -----------------------------------------------------------
render "$WORK/dind.yaml" --set workers.docker.dindDataSize=30Gi
want_api "(g) dindDataSize set" "$WORK/dind.yaml" UZI_WORKER_DIND_DATA_SIZE_OVERRIDDEN 1
render "$WORK/dindoff.yaml" --set workers.docker.dindDataSize=
want_api "(g) dindDataSize empty" "$WORK/dindoff.yaml" UZI_WORKER_DIND_DATA_SIZE_OVERRIDDEN ABSENT

# --- (h) the controller gets the raw quantities -------------------------------------------
want_ctl "(h) default" "$D" UZI_WORKER_MAX_CPU 4
want_ctl "(h) default" "$D" UZI_WORKER_MAX_MEMORY 32Gi
want_ctl "(h) default" "$D" UZI_WORKER_DOCKER_MAX_CPU 4
want_ctl "(h) default" "$D" UZI_WORKER_DOCKER_MAX_MEMORY 32Gi
want_ctl "(h) default" "$D" UZI_WORKER_ISOLATED_MAX_CPU 4
want_ctl "(h) default" "$D" UZI_WORKER_ISOLATED_MAX_MEMORY 32Gi
render "$WORK/raw.yaml" --set "$RL.max.memory=1.5Gi" --set "$RL.max.cpu=1500m"
want_ctl "(h) raw quantity kept verbatim" "$WORK/raw.yaml" UZI_WORKER_MAX_MEMORY 1.5Gi
want_ctl "(h) raw quantity kept verbatim" "$WORK/raw.yaml" UZI_WORKER_MAX_CPU 1500m

if [ "$fail" -ne 0 ]; then
  echo "FAIL: the worker size ceilings chart surface does not hold" >&2
  exit 1
fi
echo "OK: worker size ceilings chart surface holds (floored integer ceilings and quotas, soft quotas, tiers, dind flag, raw controller ceilings)"
