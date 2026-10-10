#!/bin/sh
# Assert the chart renders the api's DB_STORAGE_CAPACITY_BYTES (the admin-health db.size
# budget) from the managed database's storage size, exactly like Kubernetes parses it.
#
# usage: scripts/assert-db-capacity-render.sh [chart-dir]
#
# WHY THIS EXISTS. The api reads DB_STORAGE_CAPACITY_BYTES (positive base-10 bytes). The
# chart derives it from database.simple.storage.size (simple) or
# postgres.cluster.storage.size (cnpg) with a pure-template quantity parser
# (uzi.quantityToBytes in templates/_helpers.tpl), and leaves it to api.config under
# database.mode external. A parser slip, a lost envFrom or a duplicate-key guard that
# stopped firing would render and lint clean and surface only as a wrong disk-usage
# budget in production, so every property is asserted on the parsed render.
#
# EXPECTED BYTES for every valid quantity were derived from
# k8s.io/apimachinery resource.MustParse(x).Value() (v0.37.1, which rounds up), not from
# the template's own arithmetic.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the CNPG
# subchart dependency stripped. A render that lacks an expected key is a BROKEN
# INSTRUMENT (exit 2), never a pass.
#
# EXIT CODES (the assert-drain-knobs-render.sh convention):
#     2 = the instrument is broken (helm/yq/chart missing, a render that must succeed
#         failed, or an expected key absent)
#     1 = a property does not hold (wrong bytes, a bad quantity rendered, a guard silent)
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

# render_cm <out> [helm args...]: render the api ConfigMap; exit 2 if it must succeed and did not.
render_cm() {
  _out="$1"; shift
  if ! helm template uzi "$CHART" --namespace uzi --show-only templates/api-configmap.yaml "$@" > "$_out" 2> "$_out.err"; then
    echo "BROKEN: a render that must succeed failed: helm template $*" >&2
    cat "$_out.err" >&2
    exit 2
  fi
}

# capacity <file>: the rendered DB_STORAGE_CAPACITY_BYTES, or "ABSENT".
capacity() {
  _v=$(yq '.data.DB_STORAGE_CAPACITY_BYTES // "ABSENT"' "$1")
  echo "$_v"
}

# expect_bytes <label> <want> [helm args...]
expect_bytes() {
  _label="$1"; _want="$2"; shift 2
  render_cm "$WORK/r.yaml" "$@"
  _got=$(capacity "$WORK/r.yaml")
  if [ "$_got" = "ABSENT" ]; then
    echo "BROKEN: DB_STORAGE_CAPACITY_BYTES absent from the render for: $_label" >&2
    exit 2
  fi
  if [ "$_got" = "$_want" ]; then
    ok "$_label -> $_got"
  else
    bad "$_label rendered '$_got', expected '$_want'"
  fi
}

# expect_fail <label> <message-fragment> [helm args...]: the render must fail naming the fragment.
expect_fail() {
  _label="$1"; _frag="$2"; shift 2
  if helm template uzi "$CHART" --namespace uzi "$@" > "$WORK/f.out" 2> "$WORK/f.err"; then
    bad "$_label rendered but must fail"
  elif grep -F -q -- "$_frag" "$WORK/f.err"; then
    ok "$_label fails the render"
  else
    bad "$_label failed, but without '$_frag': $(head -c 300 "$WORK/f.err")"
  fi
}

# --- (a) simple default: 8Gi, and the api Deployment envFrom the ConfigMap ------------------
expect_bytes "(a) simple default (8Gi)" 8589934592
render_cm "$WORK/cm.yaml"
CM_NAME=$(yq '.metadata.name' "$WORK/cm.yaml")
check_envfrom() {
  _label="$1"; shift
  helm template uzi "$CHART" --namespace uzi --show-only templates/api-deployment.yaml "$@" > "$WORK/d.yaml" 2> "$WORK/d.err" \
    || { echo "BROKEN: api-deployment render failed for: $_label" >&2; cat "$WORK/d.err" >&2; exit 2; }
  _refs=$(yq '[.spec.template.spec.containers[].envFrom[].configMapRef.name] | join(",")' "$WORK/d.yaml")
  case ",$_refs," in
    *",$CM_NAME,"*) ok "$_label: api Deployment envFrom $CM_NAME" ;;
    *) bad "$_label: api Deployment envFrom is '$_refs', expected $CM_NAME" ;;
  esac
}
check_envfrom "(a) simple default"

# --- (b) api.config emptied: the capacity alone enables the ConfigMap and the envFrom -------
# `--set api.config={}` cannot do this: Helm MERGES maps, so the chart's default keys stay.
# Rewrite the default `api.config` block to `{}` in a second chart copy instead, and prove
# the copy really has no config (non-vacuous) before asserting. releaseCheck is nulled too
# (it renders its own keys by default and would enable the ConfigMap by itself).
CHART_NOCFG="$WORK/chart-nocfg"
cp -R "$CHART" "$CHART_NOCFG"
awk '
  /^api:/ { inapi = 1; print; next }
  inapi && /^[^[:space:]#]/ { inapi = 0 }
  inapi && !done && /^  config:/ { print "  config: {}"; skipping = 1; done = 1; next }
  skipping && (/^  [A-Za-z]/ || /^[^[:space:]#]/) { skipping = 0 }
  skipping { next }
  { print }
' "$CHART/values.yaml" > "$CHART_NOCFG/values.yaml"
if [ "$(helm template uzi "$CHART_NOCFG" --set releaseCheck=null --show-only templates/api-configmap.yaml 2>/dev/null | yq '.data | length')" != "1" ]; then
  echo "BROKEN: the api.config-less chart copy still renders other ConfigMap keys (or failed)" >&2
  exit 2
fi
CHART_MAIN="$CHART"
CHART="$CHART_NOCFG"
expect_bytes "(b) simple, empty api.config, no releaseCheck" 8589934592 --set releaseCheck=null
check_envfrom "(b) simple, empty api.config, no releaseCheck" --set releaseCheck=null
CHART="$CHART_MAIN"

# --- (c) cnpg ------------------------------------------------------------------------------
CNPG="--set database.mode=cnpg --set postgres.enabled=true"
# shellcheck disable=SC2086 # CNPG is a deliberate word-split list of flags
expect_bytes "(c) cnpg 5Gi" 5368709120 $CNPG --set-string postgres.cluster.storage.size=5Gi
# shellcheck disable=SC2086
expect_bytes "(c) cnpg 20Gi" 21474836480 $CNPG --set-string postgres.cluster.storage.size=20Gi

# --- (d) external ----------------------------------------------------------------------------
render_cm "$WORK/ext.yaml" --set database.mode=external
if [ "$(capacity "$WORK/ext.yaml")" = "ABSENT" ]; then
  ok "(d) external renders no DB_STORAGE_CAPACITY_BYTES"
else
  bad "(d) external rendered DB_STORAGE_CAPACITY_BYTES '$(capacity "$WORK/ext.yaml")'"
fi
expect_bytes "(d) external with api.config.DB_STORAGE_CAPACITY_BYTES=1000" 1000 \
  --set database.mode=external --set-string api.config.DB_STORAGE_CAPACITY_BYTES=1000

# --- (e) duplicate key in a managed mode -----------------------------------------------------
expect_fail "(e) simple + api.config.DB_STORAGE_CAPACITY_BYTES" "duplicate DB_STORAGE_CAPACITY_BYTES" \
  --set-string api.config.DB_STORAGE_CAPACITY_BYTES=1000

# --- (f) valid quantity matrix ---------------------------------------------------------------
# Bytes below are resource.MustParse(<quantity>).Value() from k8s.io/apimachinery v0.37.1.
cat > "$WORK/matrix.txt" <<'MATRIX'
8Gi	8589934592
8G	8000000000
500M	500000000
1.5Gi	1610612736
.5Gi	536870912
+8Gi	8589934592
100m	1
8e9	8000000000
1E	1000000000000000000
1234567890123456789m	1234567890123457
0.000000000000000000001Ei	1
1Ei	1152921504606846976
12e-1	2
MATRIX
while IFS="	" read -r q want; do
  expect_bytes "(f) size=$q" "$want" --set-string "database.simple.storage.size=$q"
done < "$WORK/matrix.txt"
# Long fractions, built here rather than typed. Both are valid Kubernetes quantities whose
# Value() rounds up: 1.(70 zeros)1 -> 2 exercises the chart's fraction truncation (the
# sticky "dropped a nonzero digit" flag, a mutant clearing it renders 1), and
# 0.(64 zeros)1Ki -> 1 exercises a tiny binary-suffixed value that still rounds up.
Z70=$(awk 'BEGIN { for (i = 0; i < 70; i++) printf "0" }')
Z64=$(awk 'BEGIN { for (i = 0; i < 64; i++) printf "0" }')
expect_bytes "(f) size=1.<70 zeros>1" 2 --set-string "database.simple.storage.size=1.${Z70}1"
expect_bytes "(f) size=0.<64 zeros>1Ki" 1 --set-string "database.simple.storage.size=0.${Z64}1Ki"
# 20-digit exponents: Kubernetes itself rejects or wraps these, so the chart's behaviour is
# the documented intent, not a resource.Quantity comparison: the exponent clamps to 18 nines,
# a huge positive one fails the bound, a huge negative one rounds a nonzero value up to 1.
N20=$(awk 'BEGIN { for (i = 0; i < 20; i++) printf "9" }')
# This row documents the chart's exact behaviour only (Kubernetes itself rejects this
# quantity); it does not guard the clamp's negative half.
expect_bytes "(f) size=1e-<20 nines> (chart intent: rounds up to 1)" 1 --set-string "database.simple.storage.size=1e-${N20}"
# A YAML plain integer arrives as a float64 (the %v form is 1.073741824e+10); --set gives an int64.
printf 'database:\n  simple:\n    storage:\n      size: 10737418240\n' > "$WORK/int.yaml"
expect_bytes "(f) YAML int 10737418240" 10737418240 -f "$WORK/int.yaml"
expect_bytes "(f) --set int 1073741824" 1073741824 --set database.simple.storage.size=1073741824

# --- (g) invalid quantities fail the render --------------------------------------------------
for q in 8GB abc 0 0.0Gi -1Gi 2Ei 1025Pi 1152921504606846976.5 1152921504606846977 1e99999999; do
  expect_fail "(g) size=$q" "database.simple.storage.size must be a positive Kubernetes quantity" \
    --set-string "database.simple.storage.size=$q"
done
expect_fail "(g) size=1152921504606846976.<70 zeros>1" "database.simple.storage.size must be a positive Kubernetes quantity" \
  --set-string "database.simple.storage.size=1152921504606846976.${Z70}1"
expect_fail "(g) size=1e<20 nines> (chart intent: rejected)" "database.simple.storage.size must be a positive Kubernetes quantity" \
  --set-string "database.simple.storage.size=1e${N20}"
printf 'database:\n  simple:\n    storage:\n      size: ""\n' > "$WORK/empty.yaml"
expect_fail "(g) size=<empty>" "database.simple.storage.size must be a positive Kubernetes quantity" -f "$WORK/empty.yaml"

if [ "$fail" -ne 0 ]; then
  echo "FAIL: DB_STORAGE_CAPACITY_BYTES is NOT correctly derived from the database storage size" >&2
  exit 1
fi
echo "OK: DB_STORAGE_CAPACITY_BYTES renders from the database storage size, parsed like resource.Quantity.Value()"
