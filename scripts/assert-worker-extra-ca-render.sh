#!/bin/sh
# Assert workers.extraCA renders as documented: off by default; inline pem renders a
# chart-owned ConfigMap, the controller env, mount and a roll checksum; an existing
# ConfigMap is projected by its key with no chart ConfigMap; both set is refused.
#
# usage: scripts/assert-worker-extra-ca-render.sh [chart-dir]
#
# OFFLINE and NON-VACUOUS like assert-worker-node-selector-render.sh: the CNPG subchart
# is stripped from a temp copy, and every case first proves the controller Deployment
# rendered (a sentinel env is present), so an empty render never reads as a pass.
#
# EXIT CODES: 2 = instrument broken, 1 = a case rendered the wrong thing, 0 = all hold.
set -eu

SENTINEL="UZI_WORKER_NAMESPACE"
PEM='-----BEGIN CERTIFICATE-----
ZXhhbXBsZQ==
-----END CERTIFICATE-----'

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
  helm template uzi "$WORK/chart" --set workers.enabled=true --set api.tls.enabled=true "$@" \
    > "$WORK/out" 2> "$WORK/err"
}
require_controller() {
  grep -q -- "- name: $SENTINEL\$" "$WORK/out" || {
    echo "BROKEN ($1): the controller Deployment did not render; stderr:" >&2
    cat "$WORK/err" >&2
    exit 2
  }
}
fail=0
has() { grep -q -F -- "$2" "$WORK/out" || { echo "FAIL ($1): missing: $2" >&2; fail=1; }; }
lacks() { if grep -q -F -- "$2" "$WORK/out"; then echo "FAIL ($1): unexpected: $2" >&2; fail=1; fi; }

render || { echo "BROKEN (default): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_controller default
lacks default "UZI_WORKER_EXTRA_CA_FILE"
lacks default "uzi-worker-extra-ca"

render --set-string "workers.extraCA.pem=$PEM" || { echo "BROKEN (pem): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_controller pem
has pem "- name: UZI_WORKER_EXTRA_CA_FILE"
has pem '"/etc/uzi/worker-extra-ca/ca.crt"'
has pem "name: uzi-worker-extra-ca"
has pem "checksum/worker-extra-ca:"
has pem "ZXhhbXBsZQ=="

render --set workers.extraCA.configMap.name=trust-bundle --set workers.extraCA.configMap.key=bundle.pem \
  || { echo "BROKEN (configMap): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_controller configMap
has configMap "- name: UZI_WORKER_EXTRA_CA_FILE"
has configMap "name: trust-bundle"
has configMap "- key: bundle.pem"
lacks configMap "uzi-worker-extra-ca"
lacks configMap "checksum/worker-extra-ca:"

if render --set-string "workers.extraCA.pem=$PEM" --set workers.extraCA.configMap.name=trust-bundle; then
  echo "FAIL (both): rendered with pem AND configMap.name set" >&2; fail=1
elif ! grep -q -F "set pem OR configMap.name" "$WORK/err"; then
  echo "FAIL (both): refused, but not with the extraCA message:" >&2; cat "$WORK/err" >&2; fail=1
fi

[ "$fail" -eq 0 ] && echo "OK: workers.extraCA renders as documented"
exit "$fail"
