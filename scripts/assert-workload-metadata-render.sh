#!/bin/sh
# Assert workloadLabels / workloadAnnotations render as documented
# (deploy/chart/values.yaml):
#   - unset: no extra metadata and no controller env;
#   - set: every chart Deployment/StatefulSet carries them and the controller gets them
#     as JSON for the worker Deployments;
#   - a label uzi owns, a non-string value and a uzi.dev/ annotation are refused.
#
# usage: scripts/assert-workload-metadata-render.sh [chart-dir]
#
# OFFLINE and NON-VACUOUS like assert-worker-node-selector-render.sh: the CNPG subchart
# is stripped from a temp copy, and every case first proves the controller Deployment
# rendered, so an empty render never reads as a pass.
#
# EXIT CODES: 2 = instrument broken, 1 = a case rendered the wrong thing, 0 = all hold.
set -eu

SENTINEL="UZI_WORKER_NAMESPACE"
CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM
cp -R "$CHART_DIR" "$WORK/chart"
rm -f "$WORK/chart/Chart.lock"
awk '
  BEGIN { skip = 0 }
  /^dependencies:/ { skip = 1; next }
  skip && /^[^[:space:]-]/ { skip = 0 }
  skip { next }
  { print }
' "$CHART_DIR/Chart.yaml" > "$WORK/chart/Chart.yaml"

render() {
  helm template uzi "$WORK/chart" --namespace uzi --set workers.enabled=true --set api.tls.enabled=true \
    --set database.mode=simple "$@" > "$WORK/out" 2> "$WORK/err"
}
must_render() {
  render "$@" || { echo "BROKEN: render failed: $*" >&2; cat "$WORK/err" >&2; exit 2; }
  grep -q -- "- name: $SENTINEL\$" "$WORK/out" || { echo "BROKEN: no controller Deployment: $*" >&2; exit 2; }
}
fail=0
has() { grep -q -F -- "$2" "$WORK/out" || { echo "FAIL ($1): missing: $2" >&2; fail=1; }; }
lacks() { if grep -q -F -- "$2" "$WORK/out"; then echo "FAIL ($1): unexpected: $2" >&2; fail=1; fi; }
count() { grep -c -F -- "$1" "$WORK/out" || true; }
refused() {
  name=$1 msg=$2; shift 2
  if render "$@"; then echo "FAIL ($name): rendered" >&2; fail=1
  elif ! grep -q -F -- "$msg" "$WORK/err"; then echo "FAIL ($name): refused without: $msg" >&2; cat "$WORK/err" >&2; fail=1; fi
}

must_render
lacks default "UZI_WORKER_DEPLOYMENT_LABELS"
lacks default "UZI_WORKER_DEPLOYMENT_ANNOTATIONS"

must_render --set workloadLabels.team=platform --set-string 'workloadAnnotations.policy\.example\.com/skip-probes=true'
# api, web, controller and the simple-postgres StatefulSet.
[ "$(count 'team: platform')" -ge 4 ] || { echo "FAIL (metadata): team label on $(count 'team: platform') workloads, want >= 4" >&2; fail=1; }
[ "$(count 'policy.example.com/skip-probes: "true"')" -ge 4 ] || { echo "FAIL (metadata): annotation on $(count 'policy.example.com/skip-probes: "true"') workloads, want >= 4" >&2; fail=1; }
has metadata '"{\"team\":\"platform\"}"'
has metadata '"{\"policy.example.com/skip-probes\":\"true\"}"'

refused "owned label" 'workloadLabels: "app.kubernetes.io/name" is set by the chart' --set 'workloadLabels.app\.kubernetes\.io/name=x'
refused "worker id label" 'workloadLabels: "uzi.dev/hosted-worker-id" is set by the chart' --set 'workloadLabels.uzi\.dev/hosted-worker-id=x'

refused "number label" 'workloadLabels: "cost" must be a string' --set workloadLabels.cost=1234
refused "number annotation" 'workloadAnnotations: "port" must be a string' --set workloadAnnotations.port=8080
refused "reserved annotation" 'workloadAnnotations: "uzi.dev/image-roll" uses the reserved' --set-string 'workloadAnnotations.uzi\.dev/image-roll=x'

[ "$fail" -eq 0 ] && echo "OK: workload metadata renders as documented"
exit "$fail"
