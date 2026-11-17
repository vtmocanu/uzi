#!/bin/sh
# Assert the model-provider chart values render as documented (PRD #4320): providers.allowedBaseURLs
# is off by default and renders UZI_PROVIDER_ALLOWED_BASE_URLS (path kept) when set; a bad entry
# or the same key in api.config refuses the render; workers.extraCA mounts into the api pod
# read-only and sets UZI_PROVIDER_EXTRA_CA_FILE (and rolls the pod), and is absent otherwise.
#
# usage: scripts/assert-provider-chart-render.sh [chart-dir]
#
# OFFLINE and NON-VACUOUS like assert-worker-extra-ca-render.sh: the CNPG subchart is stripped
# from a temp copy, and every render case first proves the api ConfigMap and Deployment
# rendered (sentinels), so an empty render never reads as a pass.
#
# EXIT CODES: 2 = instrument broken, 1 = a case rendered the wrong thing, 0 = all hold.
set -eu

CM_SENTINEL="kind: ConfigMap"
DEPLOY_SENTINEL="- name: UZI_DISK_PRESSURE_THRESHOLD"
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
  helm template uzi "$WORK/chart" "$@" > "$WORK/out" 2> "$WORK/err"
}
require_api() {
  grep -q -F -- "$CM_SENTINEL" "$WORK/out" && grep -q -F -- "$DEPLOY_SENTINEL" "$WORK/out" || {
    echo "BROKEN ($1): the api ConfigMap/Deployment did not render; stderr:" >&2
    cat "$WORK/err" >&2
    exit 2
  }
}
fail=0
has() { grep -q -F -- "$2" "$WORK/out" || { echo "FAIL ($1): missing: $2" >&2; fail=1; }; }
lacks() { if grep -q -F -- "$2" "$WORK/out"; then echo "FAIL ($1): unexpected: $2" >&2; fail=1; fi; }

# A bad entry must fail the render, with the entry itself never echoed. The case name is the
# first argument; the rest are --set-string flags.
refused() {
  name=$1; shift
  if render "$@"; then
    echo "FAIL ($name): rendered" >&2; fail=1
  elif ! grep -q -F "providers.allowedBaseURLs entry 1" "$WORK/err"; then
    echo "FAIL ($name): refused, but not with the providers message:" >&2; cat "$WORK/err" >&2; fail=1
  elif grep -q -F "gw.example.com" "$WORK/err"; then
    echo "FAIL ($name): the refusal echoes the entry" >&2; fail=1
  fi
}

# Default: nothing provider-related renders.
render || { echo "BROKEN (default): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_api default
lacks default "UZI_PROVIDER_ALLOWED_BASE_URLS"
lacks default "UZI_PROVIDER_EXTRA_CA_FILE"
lacks default "provider-extra-ca"
lacks default "UZI_PROVIDER_ALLOW_LOOPBACK"

# A set list renders, path kept, comma-joined.
render --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/anthropic' \
  --set-string 'providers.allowedBaseURLs[1]=https://llm.internal.example:8443/v1/claude/' \
  || { echo "BROKEN (list): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_api list
has list 'UZI_PROVIDER_ALLOWED_BASE_URLS: "https://gw.example.com/anthropic,https://llm.internal.example:8443/v1/claude/"'
lacks list "UZI_PROVIDER_ALLOW_LOOPBACK"

# Bad entries are refused.
refused http --set-string 'providers.allowedBaseURLs[0]=http://gw.example.com/anthropic'
refused query --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/anthropic?x=1'
refused fragment --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/anthropic#frag'
refused dotdot --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/a/../anthropic'
refused dot --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/a/./anthropic'
refused trailing-dotdot --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/anthropic/..'
refused userinfo --set-string 'providers.allowedBaseURLs[0]=https://user:pw@gw.example.com/anthropic'
refused percent --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/anthro%70ic'
refused semicolon --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/a;b'
refused backslash --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/a\\b'
refused empty-segment --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/a//b'
refused whitespace --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/a b'
refused comma --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/a\,https://gw.example.com/b'
refused no-host --set-string 'providers.allowedBaseURLs[0]=https:///anthropic'
refused ftp --set-string 'providers.allowedBaseURLs[0]=ftp://gw.example.com/anthropic'

# The same key in api.config is a duplicate, refused.
if render --set-string 'providers.allowedBaseURLs[0]=https://gw.example.com/anthropic' \
    --set-string 'api.config.UZI_PROVIDER_ALLOWED_BASE_URLS=https://other.example.com/x'; then
  echo "FAIL (both): rendered with the list in providers.* AND api.config" >&2; fail=1
elif ! grep -q -F "providers.allowedBaseURLs OR api.config.UZI_PROVIDER_ALLOWED_BASE_URLS" "$WORK/err"; then
  echo "FAIL (both): refused, but not with the providers message:" >&2; cat "$WORK/err" >&2; fail=1
fi

# workers.extraCA (inline pem) reaches the api pod: read-only mount, env, roll checksum, ConfigMap.
render --set api.tls.enabled=true --set-string "workers.extraCA.pem=$PEM" \
  || { echo "BROKEN (extraCA pem): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_api extraCA-pem
has extraCA-pem "- name: UZI_PROVIDER_EXTRA_CA_FILE"
has extraCA-pem '"/etc/uzi/provider-extra-ca/ca.crt"'
has extraCA-pem "- name: provider-extra-ca"
has extraCA-pem "mountPath: /etc/uzi/provider-extra-ca"
has extraCA-pem "checksum/provider-extra-ca:"
has extraCA-pem "name: uzi-worker-extra-ca"
# The mount is read-only: the api container's provider-extra-ca volumeMount carries readOnly.
awk '/- name: provider-extra-ca/{f=1;next} f&&/readOnly: true/{ok=1} f&&/^[[:space:]]*- name:/{f=0} END{exit ok?0:1}' "$WORK/out" \
  || { echo "FAIL (extraCA pem): the provider-extra-ca mount is not readOnly" >&2; fail=1; }

# An existing ConfigMap is projected by its key; no chart ConfigMap.
render --set api.tls.enabled=true --set workers.extraCA.configMap.name=trust-bundle --set workers.extraCA.configMap.key=bundle.pem \
  || { echo "BROKEN (extraCA configMap): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_api extraCA-configMap
has extraCA-configMap "- name: UZI_PROVIDER_EXTRA_CA_FILE"
has extraCA-configMap "name: trust-bundle"
has extraCA-configMap "- key: bundle.pem"
has extraCA-configMap "checksum/provider-extra-ca:"
lacks extraCA-configMap "uzi-worker-extra-ca"

# Unset: no provider mount, env or checksum (same render as the default case, with TLS on).
render --set api.tls.enabled=true || { echo "BROKEN (tls only): render failed" >&2; cat "$WORK/err" >&2; exit 2; }
require_api tls-only
lacks tls-only "UZI_PROVIDER_EXTRA_CA_FILE"
lacks tls-only "provider-extra-ca"

[ "$fail" -eq 0 ] && echo "OK: provider chart values render as documented"
exit "$fail"
