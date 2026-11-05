#!/bin/sh
# Assert the chart renders the pieces size-only PVC expansion (issue #3890) depends on:
# the DinD data-size env plumbing and its ceiling guards, and the controller's single,
# get-only StorageClass ClusterRole with its binding.
#
# usage: scripts/assert-pvc-expansion-render.sh [chart-dir]
#
# WHY THIS EXISTS. Expansion reads the claim's StorageClass, which is cluster-scoped, so
# the chart grants the controller `get` on storage.k8s.io storageclasses through ONE
# ClusterRole (workers.storageClassRead.create). That is a deliberate, owner-authorized
# exception to the otherwise namespace-only controller RBAC, so every property of it is
# asserted on the parsed render: exactly one rule, exactly one verb, bound only to the
# controller ServiceAccount, absent when disabled, and no other cluster-scoped grant over
# PVCs (those rules stay in the worker-namespace Roles). A lost default, a widened verb or
# a PVC rule that drifted into a ClusterRole would render and lint clean.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the CNPG
# subchart dependency stripped, exactly as assert-db-capacity-render.sh does. A render that
# lacks an expected object is a BROKEN INSTRUMENT (exit 2), never a pass.
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
    ok "$_label fails the render"
  else
    bad "$_label failed, but without '$_frag': $(head -c 300 "$WORK/f.err")"
  fi
}

# dind_env <file>: the controller's UZI_WORKER_DIND_DATA_SIZE value, or "ABSENT".
dind_env() {
  yq 'select(.kind == "Deployment") | .spec.template.spec.containers[].env[] | select(.name == "UZI_WORKER_DIND_DATA_SIZE") | .value' "$1" | awk 'NF { print; found = 1 } END { if (!found) print "ABSENT" }'
}

# count <file> <yq-select-expression>: how many documents match.
count() {
  yq ea "[$2] | length" "$1"
}

CR_NAME="uzi-controller-storageclass-read"

# --- (a) default render ------------------------------------------------------------------
render "$WORK/default.yaml"
# Non-vacuous: the controller Deployment must be in the render, or "no env" proves nothing.
[ "$(count "$WORK/default.yaml" 'select(.kind == "Deployment" and .spec.template.spec.containers[].env[].name == "UZI_WORKER_DOCKER_EPHEMERAL_REQUEST")')" -ge 1 ] \
  || { echo "BROKEN: the controller Deployment (docker tier knobs) is not in the default render" >&2; exit 2; }
if [ "$(dind_env "$WORK/default.yaml")" = "ABSENT" ]; then
  ok "(a) default render sets no UZI_WORKER_DIND_DATA_SIZE (per-preset defaults apply)"
else
  bad "(a) default render sets UZI_WORKER_DIND_DATA_SIZE to '$(dind_env "$WORK/default.yaml")'"
fi

# --- (b) explicit override and its ceiling ---------------------------------------------------
render "$WORK/dind30.yaml" --set workers.docker.dindDataSize=30Gi
if [ "$(dind_env "$WORK/dind30.yaml")" = "30Gi" ]; then
  ok "(b) workers.docker.dindDataSize=30Gi renders UZI_WORKER_DIND_DATA_SIZE=30Gi"
else
  bad "(b) dindDataSize=30Gi rendered '$(dind_env "$WORK/dind30.yaml")'"
fi
expect_fail "(b) dindDataSize=60Gi above the 50Gi ceiling" "workers.docker.dindDataSize" \
  --set workers.docker.dindDataSize=60Gi
expect_fail "(b) maxPVCStorage=40Gi below the 50Gi per-preset default" \
  "largest preset, l/l-highmem, claims 50Gi" \
  --set workers.docker.limitRange.maxPVCStorage=40Gi

# --- (c) the StorageClass ClusterRole ---------------------------------------------------------
cr='select(.kind == "ClusterRole" and .metadata.name == "'"$CR_NAME"'")'
crb='select(.kind == "ClusterRoleBinding" and .metadata.name == "'"$CR_NAME"'")'
[ "$(count "$WORK/default.yaml" "$cr")" = 1 ] \
  || { echo "BROKEN: ClusterRole $CR_NAME is not rendered exactly once by default" >&2; exit 2; }
[ "$(count "$WORK/default.yaml" "$crb")" = 1 ] \
  || { echo "BROKEN: ClusterRoleBinding $CR_NAME is not rendered exactly once by default" >&2; exit 2; }

rules=$(yq "$cr | .rules | length" "$WORK/default.yaml")
groups=$(yq "$cr | .rules[0].apiGroups | join(\",\")" "$WORK/default.yaml")
resources=$(yq "$cr | .rules[0].resources | join(\",\")" "$WORK/default.yaml")
verbs=$(yq "$cr | .rules[0].verbs | join(\",\")" "$WORK/default.yaml")
if [ "$rules" = 1 ] && [ "$groups" = "storage.k8s.io" ] && [ "$resources" = "storageclasses" ] && [ "$verbs" = "get" ]; then
  ok "(c) ClusterRole $CR_NAME has exactly one rule: storage.k8s.io storageclasses get"
else
  bad "(c) ClusterRole $CR_NAME rules=$rules apiGroups=$groups resources=$resources verbs=$verbs"
fi

sa=$(yq "$crb | .subjects | length" "$WORK/default.yaml")
sa_kind=$(yq "$crb | .subjects[0].kind" "$WORK/default.yaml")
sa_name=$(yq "$crb | .subjects[0].name" "$WORK/default.yaml")
sa_ns=$(yq "$crb | .subjects[0].namespace" "$WORK/default.yaml")
ref_kind=$(yq "$crb | .roleRef.kind" "$WORK/default.yaml")
ref_name=$(yq "$crb | .roleRef.name" "$WORK/default.yaml")
want_sa=$(yq '.workers.controllerServiceAccountName' "$CHART/values.yaml")
if [ "$sa" = 1 ] && [ "$sa_kind" = "ServiceAccount" ] && [ "$sa_name" = "$want_sa" ] && [ "$sa_ns" = "uzi" ] \
  && [ "$ref_kind" = "ClusterRole" ] && [ "$ref_name" = "$CR_NAME" ]; then
  ok "(c) the binding grants $CR_NAME only to ServiceAccount $sa_ns/$sa_name"
else
  bad "(c) binding subjects=$sa $sa_kind $sa_ns/$sa_name roleRef=$ref_kind/$ref_name (want 1 ServiceAccount uzi/$want_sa -> ClusterRole/$CR_NAME)"
fi

# --- (d) disabling it renders neither object -----------------------------------------------
render "$WORK/off.yaml" --set workers.storageClassRead.create=false
n=$(count "$WORK/off.yaml" 'select((.kind == "ClusterRole" or .kind == "ClusterRoleBinding") and .metadata.name == "'"$CR_NAME"'")')
if [ "$n" = 0 ]; then
  ok "(d) workers.storageClassRead.create=false renders neither the ClusterRole nor its binding"
else
  bad "(d) create=false still rendered $n storageclass-read object(s)"
fi

# --- (e) the PVC and StorageClass rules stay where they belong -----------------------------
n=$(count "$WORK/default.yaml" 'select(.kind == "Role") | .rules[] | select(.resources[] == "storageclasses")')
if [ "$n" = 0 ]; then
  ok "(e) no namespaced Role grants storageclasses (a Role cannot authorize a cluster-scoped read)"
else
  bad "(e) $n Role rule(s) grant storageclasses"
fi
n=$(count "$WORK/default.yaml" 'select(.kind == "ClusterRole") | .rules[] | select(.resources[] == "persistentvolumeclaims")')
if [ "$n" = 0 ]; then
  ok "(e) no ClusterRole grants persistentvolumeclaims"
else
  bad "(e) $n ClusterRole rule(s) grant persistentvolumeclaims"
fi
# Non-vacuous: PVC rules do exist, in Roles.
n=$(count "$WORK/default.yaml" 'select(.kind == "Role") | .rules[] | select(.resources[] == "persistentvolumeclaims")')
[ "$n" -ge 1 ] || { echo "BROKEN: no Role grants persistentvolumeclaims, so (e) proves nothing" >&2; exit 2; }

if [ "$fail" -ne 0 ]; then
  echo "FAIL: the PVC expansion chart surface does not hold" >&2
  exit 1
fi
echo "OK: PVC expansion chart surface holds (dind data size plumbing, one get-only StorageClass ClusterRole, PVC rules stay in Roles)"
