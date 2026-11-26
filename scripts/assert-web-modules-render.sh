#!/bin/sh
# Assert the chart renders runtime web modules (PRD #4686) from web.modules[]: the public
# /modules.json, the per-module nginx locations, the two subPath mounts, and the validation.
#
# usage: scripts/assert-web-modules-render.sh [chart-dir]
#
# WHY THIS EXISTS. web.modules[] is operator input that ends up inside an nginx config and in a
# file served without authentication. A validation slip would render and lint clean and show up
# as an nginx config injection, a leaked upstream address or a module that never appears, so
# every property is asserted on the parsed render:
#   - 0, 1 and 2 modules render; with none, modules.json is `[]` and locations.conf is comment-only;
#   - modules.json is exactly the projection id, version, page, label, icon, navGroup, adminOnly
#     (adminOnly always a boolean); the upstream and unknown input fields never reach it;
#   - a dotless upstream host expands with the RELEASE namespace, a dotted host and an IPv4 stay
#     verbatim, and a leading http:// is stripped;
#   - each emitted location block is byte-identical to scripts/fixtures/web-modules/
#     location.example.conf after substituting the id, variable, resolver and upstream (the same
#     file scripts/assert-web-nginx.sh runs under real nginx);
#   - the web Deployment mounts modules.json and locations.conf from the web ConfigMap with
#     readOnly subPath mounts (a subPath file never changes inside a running container, so a
#     change rolls the pods through checksum/nginx), and that checksum changes with the list;
#   - every accept case of scripts/fixtures/web-modules/upstreams.txt renders and every reject
#     case fails with the grammar message;
#   - web.modules false, 0, "" and {} each fail with "web.modules must be a list" (absent and [] render);
#   - every invalid values fixture fails with its specific message, a module without
#     web.nginx.resolver fails.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the CNPG
# subchart dependency stripped. A render that must succeed and does not, or a missing expected
# key, is a BROKEN INSTRUMENT (exit 2), never a pass.
#
# EXIT CODES (the assert-db-capacity-render.sh convention):
#     2 = the instrument is broken (helm/yq/chart/fixtures missing, a render that must succeed
#         failed, or an expected key absent)
#     1 = a property does not hold
#     0 = every property holds
set -eu

CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"
FIX="$SCRIPT_DIR/fixtures/web-modules"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
command -v yq >/dev/null 2>&1 || { echo "BROKEN: yq (mikefarah v4) not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }
[ -f "$FIX/location.example.conf" ] || { echo "BROKEN: no location.example.conf under $FIX" >&2; exit 2; }
[ -f "$FIX/upstreams.txt" ] || { echo "BROKEN: no upstreams.txt under $FIX" >&2; exit 2; }

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

# The release namespace is not "uzi" on purpose: the dotless-host expansion must follow it.
NS=wmns
fail=0
ok() { echo "OK: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }
broken() { echo "BROKEN: $*" >&2; exit 2; }

# render <template> <out> [helm args...]: render one template; exit 2 if it must succeed and did not.
render() {
  _tpl="$1"; _out="$2"; shift 2
  if ! helm template uzi "$CHART" --namespace "$NS" --show-only "templates/$_tpl" "$@" > "$_out" 2> "$_out.err"; then
    echo "BROKEN: a render that must succeed failed: helm template $*" >&2
    cat "$_out.err" >&2
    exit 2
  fi
}

# expect_fail <label> <message-fragment> [helm args...]: the render must fail naming the fragment.
expect_fail() {
  _label="$1"; _frag="$2"; shift 2
  if helm template uzi "$CHART" --namespace "$NS" --show-only templates/web-configmap.yaml "$@" > "$WORK/f.out" 2> "$WORK/f.err"; then
    bad "$_label rendered but must fail"
  elif grep -F -q -- "$_frag" "$WORK/f.err"; then
    ok "$_label fails the render"
  else
    bad "$_label failed, but without '$_frag': $(head -c 300 "$WORK/f.err")"
  fi
}

# cm_key <configmap.yaml> <key> <out>: the raw value of a ConfigMap data key; exit 2 if absent.
cm_key() {
  yq ".data[\"$2\"]" "$1" > "$3" 2>/dev/null || broken "yq could not read data[$2]"
  if [ ! -s "$3" ] || [ "$(head -n 1 "$3")" = "null" ]; then
    broken "the web ConfigMap has no data key $2"
  fi
}

# compact_json <file>: the JSON document on one line.
compact_json() { yq -p=json -o=json -I=0 '.' "$1"; }

# block <locations.conf> <id>: the module's location block, `location` line through the closing `}`.
block() {
  awk -v want="location ^~ /modules/$2/ {" '
    $0 == want { on = 1 }
    on { print }
    on && $0 == "}" { exit }
  ' "$1"
}

# golden <label> <locations.conf> <id> <var-suffix> <resolver> <upstream>: the emitted block must
# equal the fixture template with the four placeholders filled in, byte for byte.
golden() {
  sed -n '/^location /,$p' "$FIX/location.example.conf" \
    | sed -e "s#/modules/example/#/modules/$3/#" -e "s#uzi_module_example#uzi_module_$4#g" \
        -e "s#@UZI_RESOLVER@#$5#" -e "s#@UZI_UPSTREAM@#$6#" > "$WORK/golden.want"
  if grep -q '@UZI_\|example' "$WORK/golden.want"; then
    broken "the substituted location template still has a placeholder or the example id"
  fi
  block "$2" "$3" > "$WORK/golden.got"
  if [ ! -s "$WORK/golden.got" ]; then
    bad "$1: no location block for /modules/$3/"
  elif cmp -s "$WORK/golden.got" "$WORK/golden.want"; then
    ok "$1: the block for $3 equals location.example.conf byte for byte"
  else
    bad "$1: the block for $3 differs from location.example.conf: $(diff "$WORK/golden.want" "$WORK/golden.got" | head -n 6 | tr '\n' '|')"
  fi
}

# has_line <file> <exact line>
has_line() { grep -Fx -q -e "$2" "$1"; }

# mount_of <deployment.yaml> <mountPath>: "name|subPath|readOnly" of the web container's mount.
mount_of() {
  yq ".spec.template.spec.containers[] | select(.name == \"web\") | .volumeMounts[] | select(.mountPath == \"$2\") | .name + \"|\" + .subPath + \"|\" + (.readOnly | tostring)" "$1"
}

checksum_of() { yq '.spec.template.metadata.annotations["checksum/nginx"]' "$1"; }

# --- (a) zero modules --------------------------------------------------------------------
for name in default zero; do
  if [ "$name" = default ]; then set -- ; else set -- -f "$FIX/values-zero.yaml"; fi
  render web-configmap.yaml "$WORK/cm0.yaml" "$@"
  cm_key "$WORK/cm0.yaml" modules.json "$WORK/m0.json"
  if [ "$(compact_json "$WORK/m0.json")" = "[]" ]; then
    ok "(a) $name: modules.json is []"
  else
    bad "(a) $name: modules.json is '$(compact_json "$WORK/m0.json")', want []"
  fi
  cm_key "$WORK/cm0.yaml" modules-locations.conf "$WORK/l0.conf"
  if grep -v '^[[:space:]]*$' "$WORK/l0.conf" | grep -v '^#' | grep -q .; then
    bad "(a) $name: modules-locations.conf holds more than comments"
  else
    ok "(a) $name: modules-locations.conf is comment-only"
  fi
done
# web.modules removed from the values (null deletes the chart default): treated as no modules.
render web-configmap.yaml "$WORK/cmnull.yaml" --set-json 'web.modules=null'
cm_key "$WORK/cmnull.yaml" modules.json "$WORK/mnull.json"
if [ "$(compact_json "$WORK/mnull.json")" = "[]" ]; then
  ok "(a) absent web.modules: modules.json is []"
else
  bad "(a) absent web.modules: modules.json is '$(compact_json "$WORK/mnull.json")', want []"
fi
render web-deployment.yaml "$WORK/d0.yaml" -f "$FIX/values-zero.yaml"
SUM0=$(checksum_of "$WORK/d0.yaml")

# --- (b) the three mounts (always present, even with no modules) ------------------------------
check_mounts() {
  _label="$1"; _dep="$2"
  _n=$(yq '.spec.template.spec.containers[] | select(.name == "web") | .volumeMounts | length' "$_dep")
  [ "$_n" = 3 ] && ok "$_label: the web container has exactly 3 volume mounts" || bad "$_label: the web container has $_n volume mounts, want 3"
  for _row in \
    "/etc/nginx/conf.d/default.conf|default.conf" \
    "/etc/nginx/uzi-modules/modules.json|modules.json" \
    "/etc/nginx/uzi-modules/locations.conf|modules-locations.conf"; do
    _path=${_row%%|*}; _sub=${_row#*|}
    _got=$(mount_of "$_dep" "$_path")
    if [ "$_got" = "nginx-conf|$_sub|true" ]; then
      ok "$_label: $_path is a readOnly subPath mount of $_sub"
    else
      bad "$_label: $_path mount is '$_got', want 'nginx-conf|$_sub|true' (name|subPath|readOnly)"
    fi
  done
  _vol=$(yq '.spec.template.spec.volumes[] | select(.name == "nginx-conf") | .configMap.name' "$_dep")
  [ -n "$_vol" ] && [ "$_vol" != null ] || broken "the nginx-conf volume has no ConfigMap name"
  render web-configmap.yaml "$WORK/cmname.yaml" -f "$FIX/values-zero.yaml"
  if [ "$_vol" = "$(yq '.metadata.name' "$WORK/cmname.yaml")" ]; then
    ok "$_label: the mounts read the web nginx ConfigMap"
  else
    bad "$_label: the nginx-conf volume names '$_vol', not the web nginx ConfigMap"
  fi
}
check_mounts "(b) zero modules" "$WORK/d0.yaml"

# --- (c) one module ------------------------------------------------------------------------
render web-configmap.yaml "$WORK/cm1.yaml" -f "$FIX/values-one.yaml"
cm_key "$WORK/cm1.yaml" modules.json "$WORK/m1.json"
cm_key "$WORK/cm1.yaml" modules-locations.conf "$WORK/l1.conf"
WANT1='[{"adminOnly":false,"icon":"package","id":"my-mod","label":"Ünï ✓","navGroup":"work","page":"Page","version":"1.0.0"}]'
if [ "$(compact_json "$WORK/m1.json")" = "$WANT1" ]; then
  ok "(c) modules.json is exactly the projection (adminOnly defaulted to false)"
else
  bad "(c) modules.json is '$(compact_json "$WORK/m1.json")', want '$WANT1'"
fi
for leak in upstream svc wmns secret not-public 8080; do
  if grep -q -- "$leak" "$WORK/m1.json"; then bad "(c) modules.json contains '$leak'"; else ok "(c) modules.json has no '$leak'"; fi
done
has_line "$WORK/l1.conf" '    set $uzi_module_my_mod svc.wmns.svc.cluster.local:8080;' \
  && ok "(c) the dotless host svc expands with the release namespace and http:// is stripped" \
  || bad "(c) no 'set \$uzi_module_my_mod svc.wmns.svc.cluster.local:8080;' (expansion or scheme strip wrong)"
golden "(c)" "$WORK/l1.conf" my-mod my_mod 10.96.0.10 svc.wmns.svc.cluster.local:8080
render web-deployment.yaml "$WORK/d1.yaml" -f "$FIX/values-one.yaml"
SUM1=$(checksum_of "$WORK/d1.yaml")
check_mounts "(c) one module" "$WORK/d1.yaml"

# --- (d) two modules -----------------------------------------------------------------------
render web-configmap.yaml "$WORK/cm2.yaml" -f "$FIX/values-two.yaml"
cm_key "$WORK/cm2.yaml" modules.json "$WORK/m2.json"
cm_key "$WORK/cm2.yaml" modules-locations.conf "$WORK/l2.conf"
LAB40=$(awk 'BEGIN { for (i = 0; i < 40; i++) printf "\303\251" }')
WANT2='[{"adminOnly":true,"icon":"factory","id":"first","label":"'$LAB40'","navGroup":"factory","page":"Page","version":"2.1.0"},{"adminOnly":false,"icon":"terminal","id":"second","label":"Second","navGroup":"modules","page":"Admin","version":"0.0.1"}]'
if [ "$(compact_json "$WORK/m2.json")" = "$WANT2" ]; then
  ok "(d) modules.json keeps the manifest order and each projection (a 40-character, 80-byte label is accepted)"
else
  bad "(d) modules.json is '$(compact_json "$WORK/m2.json")', want '$WANT2'"
fi
if [ "$(grep -c '^location \^~ /modules/' "$WORK/l2.conf")" = 2 ]; then ok "(d) two location blocks"; else bad "(d) want exactly two location blocks"; fi
has_line "$WORK/l2.conf" '    set $uzi_module_first first.tools.svc.cluster.local:9000;' \
  && ok "(d) a dotted host is kept verbatim" || bad "(d) the dotted host was changed"
has_line "$WORK/l2.conf" '    set $uzi_module_second 10.0.0.1:8080;' \
  && ok "(d) an IPv4 upstream is kept verbatim" || bad "(d) the IPv4 upstream was changed"
golden "(d)" "$WORK/l2.conf" first first 10.96.0.10 first.tools.svc.cluster.local:9000
golden "(d)" "$WORK/l2.conf" second second 10.96.0.10 10.0.0.1:8080
render web-deployment.yaml "$WORK/d2.yaml" -f "$FIX/values-two.yaml"
SUM2=$(checksum_of "$WORK/d2.yaml")
check_mounts "(d) two modules" "$WORK/d2.yaml"

# --- (e) checksum/nginx follows the list --------------------------------------------------
for s in "$SUM0" "$SUM1" "$SUM2"; do
  case "$s" in
    ''|null) broken "checksum/nginx annotation absent from the web Deployment" ;;
  esac
done
[ "$SUM0" != "$SUM1" ] && ok "(e) checksum/nginx differs between 0 and 1 module" || bad "(e) checksum/nginx is the same for 0 and 1 module"
[ "$SUM1" != "$SUM2" ] && ok "(e) checksum/nginx differs between 1 and 2 modules" || bad "(e) checksum/nginx is the same for 1 and 2 modules"
[ "$SUM0" != "$SUM2" ] && ok "(e) checksum/nginx differs between 0 and 2 modules" || bad "(e) checksum/nginx is the same for 0 and 2 modules"

# --- (f) the shared upstream list --------------------------------------------------------------
GRAMMAR='is invalid: it must be [http://]HOST:PORT'
upstream_values() { # <value> <out>
  U="$1" yq -n '.web.nginx.resolver = "10.96.0.10" | .web.modules = [{"id": "m", "version": "1.0.0", "page": "Page", "label": "M", "icon": "package", "navGroup": "modules", "upstream": strenv(U)}]' > "$2"
}
n_accept=0; n_reject=0
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    ''|'#'*) continue ;;
    accept\ \<*\>) kind=accept ;;
    reject\ \<*\>) kind=reject ;;
    *) broken "upstreams.txt: unreadable line: $line" ;;
  esac
  v=${line#*<}
  v=${v%>}
  upstream_values "$v" "$WORK/up.yaml"
  if [ "$kind" = accept ]; then
    n_accept=$((n_accept + 1))
    render web-configmap.yaml "$WORK/up.out" -f "$WORK/up.yaml"
    cm_key "$WORK/up.out" modules-locations.conf "$WORK/up.conf"
    if grep -q '^    set \$uzi_module_m ' "$WORK/up.conf"; then ok "(f) accept <$v>"; else bad "(f) accept <$v>: no set line rendered"; fi
  else
    n_reject=$((n_reject + 1))
    expect_fail "(f) reject <$v>" "$GRAMMAR" -f "$WORK/up.yaml"
  fi
done < "$FIX/upstreams.txt"
[ "$n_accept" -ge 4 ] && [ "$n_reject" -ge 20 ] || broken "upstreams.txt yielded only $n_accept accept and $n_reject reject cases"

# --- (g) invalid values fixtures ----------------------------------------------------------------
invalid() { # <fixture-suffix> <message fragment>
  [ -f "$FIX/values-$1.yaml" ] || broken "missing fixture values-$1.yaml"
  expect_fail "(g) $1" "$2" -f "$FIX/values-$1.yaml"
}
invalid bad-id 'id "Bad" is malformed'
invalid bad-id-long 'is malformed'
invalid dup-id 'web.modules[1]: duplicate id "same" (web.modules[0] already uses it)'
for f in version page label icon navGroup upstream; do
  invalid "missing-$f" "missing required field \"$f\""
done
invalid version-number 'missing required field "version"'
invalid label-41 'label must be 1 to 40 characters'
invalid label-41-multibyte 'label must be 1 to 40 characters'
invalid label-control 'label contains control or invisible formatting characters'
invalid label-bidi 'label contains control or invisible formatting characters'
invalid label-newline 'label contains control or invisible formatting characters'
invalid version-invisible 'version contains control or invisible formatting characters'
invalid page-invisible 'page contains control or invisible formatting characters'
invalid label-bom 'label contains control or invisible formatting characters'
invalid id-number 'id must be a string'
invalid unknown-icon 'unknown icon "rocket"'
invalid unknown-navgroup 'unknown navGroup "sidebar"'
invalid adminonly-string 'adminOnly must be a boolean'
invalid module-not-map 'web.modules[0] must be a map'
# A falsy non-list value must not be coerced into "no modules".
invalid modules-false 'web.modules must be a list'
invalid modules-zero 'web.modules must be a list'
invalid modules-empty-string 'web.modules must be a list'
invalid modules-empty-map 'web.modules must be a list'
invalid no-resolver 'web.modules requires web.nginx.resolver'
invalid bad-resolver 'not valid in an nginx resolver directive'
# The failure is not specific to the ConfigMap template: the whole chart refuses too.
if helm template uzi "$CHART" --namespace "$NS" -f "$FIX/values-bad-id.yaml" > /dev/null 2> "$WORK/whole.err"; then
  bad "(g) the whole chart rendered with an invalid module"
else
  ok "(g) the whole chart refuses an invalid module"
fi

if [ "$fail" -ne 0 ]; then
  echo "FAIL: web.modules[] does NOT render and validate as documented" >&2
  exit 1
fi
echo "OK: web.modules[] renders modules.json, the locations and the mounts as documented, and refuses every invalid case"
