#!/bin/sh
# Assert the chart renders the api's ISSUE_TRACKER_ALLOWED_BASE_URLS (the issue tracker SSRF
# allowlist, PRD #4173 S3) from the first-class issueTracker.allowedBaseURLs value.
#
# usage: scripts/assert-issue-tracker-allowlist-render.sh [chart-dir]
#
# WHY THIS EXISTS. The issue tracker allowlist is SEPARATE from the forge one: it is https
# only, empty by default (no issue tracker connection can be created), and must NEVER feed the
# worker egress lists, because a worker never calls an issue tracker (PRD #4173 S5). A
# guard that stopped firing, a lost key or an issue tracker host leaking into the FQDN egress
# rules would render and lint clean, so every property is asserted on the parsed render.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the CNPG
# subchart dependency stripped. A render that must succeed but fails, or a failing-render
# case that fails for a different reason than the one asserted, is a BROKEN INSTRUMENT
# (exit 2), never a pass.
#
# EXIT CODES (this script deliberately diverges from assert-db-capacity-render.sh: a
# failing-render case that fails with the WRONG message is exit 2 here, not 1, because it
# means the guard under test was not the thing that fired, so the instrument cannot
# vouch for it; a property that simply does not hold is exit 1):
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

# tracker_key <file>: the rendered ISSUE_TRACKER_ALLOWED_BASE_URLS, or "ABSENT".
tracker_key() {
  yq '.data.ISSUE_TRACKER_ALLOWED_BASE_URLS // "ABSENT"' "$1"
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
if [ "$(tracker_key "$WORK/cm.yaml")" = "ABSENT" ]; then
  ok "(a) default renders no ISSUE_TRACKER_ALLOWED_BASE_URLS"
else
  bad "(a) default rendered ISSUE_TRACKER_ALLOWED_BASE_URLS '$(tracker_key "$WORK/cm.yaml")'"
fi

# --- (b) a two-entry list renders joined ----------------------------------------------------
render "$WORK/two.yaml" api-configmap.yaml \
  --set 'issueTracker.allowedBaseURLs[0]=https://tracker-a.example.org' \
  --set 'issueTracker.allowedBaseURLs[1]=https://tracker-b.example.org:8443'
_got=$(tracker_key "$WORK/two.yaml")
_want="https://tracker-a.example.org,https://tracker-b.example.org:8443"
if [ "$_got" = "ABSENT" ]; then
  echo "BROKEN: ISSUE_TRACKER_ALLOWED_BASE_URLS absent from a render with two entries" >&2
  exit 2
elif [ "$_got" = "$_want" ]; then
  ok "(b) two entries render joined"
else
  bad "(b) rendered '$_got', expected '$_want'"
fi

# --- (c) invalid entries and the duplicate key fail the render ------------------------------
expect_fail "(c) http:// entry" "must be an absolute https URL with a host" \
  --set 'issueTracker.allowedBaseURLs[0]=http://tracker.example.org'
expect_fail "(c) hostless https:// entry" "must be an absolute https URL with a host" \
  --set 'issueTracker.allowedBaseURLs[0]=https://'
expect_fail "(c) entry containing a comma" "must not contain a comma or whitespace" \
  --set 'issueTracker.allowedBaseURLs[0]=https://a.example.com\,http://b.example.com'
expect_fail "(c) entry containing whitespace" "must not contain a comma or whitespace" \
  --set 'issueTracker.allowedBaseURLs[0]=https://a.example.com b.example.com'
expect_fail "(c) value + api.config.ISSUE_TRACKER_ALLOWED_BASE_URLS" "EITHER issueTracker.allowedBaseURLs OR api.config.ISSUE_TRACKER_ALLOWED_BASE_URLS" \
  --set 'issueTracker.allowedBaseURLs[0]=https://tracker.example.org' \
  --set-string api.config.ISSUE_TRACKER_ALLOWED_BASE_URLS=https://other.example.org

# --- (d) the issue tracker host never reaches the worker egress lists -----------------------------
# Both providers, with a forge host present so the egress render is non-vacuous.
for provider in antrea ovn; do
  case "$provider" in
    antrea) tpl=worker-fqdn-egress.yaml ;;
    *) tpl=worker-ovn-egress.yaml ;;
  esac
  render "$WORK/egress-$provider.yaml" "$tpl" \
    --set workers.enabled=true --set api.tls.enabled=true --set workers.fqdnEgress.enabled=true \
    --set "workers.fqdnEgress.provider=$provider" --set 'workers.fqdnEgress.ovn.exceptCIDRs[0]=10.0.0.0/8' --set workers.fqdnEgress.ovn.allowWildcards=true \
    --set 'forge.allowedBaseURLs[0]=https://forge.example.com' \
    --set 'issueTracker.allowedBaseURLs[0]=https://tracker.example.org'
  if ! grep -F -q forge.example.com "$WORK/egress-$provider.yaml"; then
    echo "BROKEN: the $provider egress render lacks the forge host (the leak check would be vacuous)" >&2
    exit 2
  fi
  if grep -F -q tracker.example.org "$WORK/egress-$provider.yaml"; then
    bad "(d) the issue tracker host leaked into the $provider worker egress render"
  else
    ok "(d) $provider worker egress carries no issue tracker host"
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "FAIL: ISSUE_TRACKER_ALLOWED_BASE_URLS is NOT correctly rendered from issueTracker.allowedBaseURLs" >&2
  exit 1
fi
echo "OK: ISSUE_TRACKER_ALLOWED_BASE_URLS renders from issueTracker.allowedBaseURLs, validated, and never feeds worker egress"
