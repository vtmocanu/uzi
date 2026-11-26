#!/usr/bin/env bash
# Assert the three web nginx configs serve the SPA's static assets compressed and with the
# right Cache-Control, while keeping the security headers and leaving /api/ bodies alone.
#
# usage: scripts/assert-web-nginx.sh [repo-root]
#
# WHY THIS EXISTS. Three nginx configs carry the same server block and must stay identical
# where it matters: web/nginx.conf (compose), web/nginx.mock.conf (mock image) and the
# default.conf rendered by deploy/chart/templates/web-configmap.yaml (Kubernetes). Without
# gzip the multi-hundred-KB hashed bundle ships raw, and without a Cache-Control the
# browser revalidates every immutable /assets/ file on each visit. The traps are quiet:
# nginx drops every inherited add_header the moment a location declares one of its own, so
# a Cache-Control on a location silently removes the security headers unless they are
# repeated there, and a gzip in the wrong scope would recompress (or corrupt) an api
# response that already carries its own Content-Encoding. A render that lints clean proves
# none of that, so every property is asserted on real responses from real nginx.
#
# HOW. The image pinned in web/Dockerfile runs, by its exact digest, once per config
# (compose, chart, mock) next to a stub upstream that answers as the api. The digest is
# fetched from mirror.gcr.io first, then Docker Hub (anonymous per-IP pull quota, #2621);
# This loop also checks both registries' local images before any pull. Fixtures are
# generated here, deterministically,
# and compared byte for byte (curl never decodes; gzip -dc and cmp do). Nothing is
# bind-mounted (the daemon may not see this host's paths): files are docker cp'd in. Every
# container and the network are named wnginx-<pid>-*, never uzi-*. Only what this run created
# is removed on exit: a container by the ID its successful docker create printed (never by
# name), the network only once docker network create succeeded. The web containers use
# --dns-option ndots:0 so the chart's static proxy_pass host "api" resolves to the stub
# alias and not to a search-domain lookalike.
#
# RUNTIME WEB MODULES (PRD #4686). Each web container also gets the module delivery files the
# images ship (web/nginx-main.conf as /etc/nginx/nginx.conf, web/nginx-modules.js, and a fixture
# /etc/nginx/uzi-modules/ naming one module `example`), and the stub upstream answers as that
# module's origin (alias `modup`) with deliberately hostile headers. The per-module location is
# scripts/fixtures/web-modules/location.example.conf, the block the chart and the compose
# generator emit (scripts/web-modules-gen.test.sh and assert-web-modules-render.sh diff both).
# Asserted in all three configs: the host security headers and CSP exactly once, every upstream
# cache/cookie/security header and every X-Accel-* header stripped, X-Accel-Redirect not followed, Cookie and Authorization not forwarded, GET and HEAD only,
# the Cache-Control split per path and final status, /modules.json, an unknown id, an absent
# upstream (nginx still starts), and that web/nginx-main.conf is the pinned image's own
# nginx.conf plus exactly the two njs lines. The module resolver is Docker's embedded DNS
# (127.0.0.11) in every variant: the throwaway network has no other DNS a name could be
# resolved by, so it stands in for the cluster DNS IP the chart's web.nginx.resolver names.
# Module resolution is independent of the chart's dynamicResolution (false here).
# The chart variants (chart, chartdyn) take their modules.json and locations.conf from the
# ConfigMap keys the chart renders from web.modules[] (chart-modules.yaml, built below), so real
# nginx runs the blocks the chart emits; the stub also answers to modup.uzi.svc.cluster.local,
# the host a dotless `modup` upstream expands to. The compose and mock variants take theirs from
# scripts/web-modules-gen.py, run on the same four modules.
#
# Three more passes: (1) the image defaults, once per variant, using the real
# web/nginx-modules-default/ dir and no fixture modules (/modules.json is `[]`, an unknown
# module is a 404 no-store); the Dockerfiles' COPY of that dir, nginx-main.conf and
# nginx-modules.js is checked by name in both Dockerfiles, and both Dockerfiles must pin the
# same final-stage nginx digest (this script reads only web/Dockerfile's). (2) A module whose id
# is `cc`, which must not collide with the Cache-Control map variable ($uzi_modcc). (3) The
# chart rendered with web.nginx.dynamicResolution=true and web.nginx.resolver=127.0.0.11 (the
# docker DNS stand-in), asserted for the module routes and an absent upstream only: its api
# upstream is the cluster-DNS name of the Service, which this network cannot resolve.
#
# Everything is read from <repo-root> (default: this script's parent directory), so it can
# be pointed at a git archive export of an older commit to prove it goes red there.
#
# EXIT CODES (the assert-db-capacity-render.sh convention):
#     2 = the instrument is broken (a tool or docker is missing, the image cannot be had,
#         the chart does not render, a container never became ready)
#     1 = a property does not hold
#     0 = every property holds
# check() and the EXIT trap call these helpers indirectly, which shellcheck cannot follow.
# shellcheck disable=SC2329
set -euo pipefail

CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(cd -- "${1:-$SCRIPT_DIR/..}" && pwd)

CSP="default-src 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; img-src 'self' data:; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'"
VARIANTS="compose chart mock"
# Response headers the module location must hide beyond the cache, cookie, security and X-Accel
# ones (the stub origin sends each one).
HIDDEN_EXTRA="Service-Worker-Allowed Clear-Site-Data Strict-Transport-Security Refresh Permissions-Policy Content-Security-Policy-Report-Only Access-Control-Allow-Origin Access-Control-Allow-Credentials Access-Control-Allow-Methods Access-Control-Allow-Headers Access-Control-Expose-Headers Report-To NEL"

broken() { echo "BROKEN: $*" >&2; exit 2; }

for tool in docker helm yq curl gzip cmp python3; do
  command -v "$tool" >/dev/null 2>&1 || broken "$tool not on PATH"
done
docker info >/dev/null 2>&1 || broken "docker daemon not reachable"
for f in web/Dockerfile web/Dockerfile.mock web/nginx.conf web/nginx.mock.conf web/nginx-main.conf web/nginx-modules.js \
  web/nginx-modules-default/modules.json web/nginx-modules-default/locations.conf \
  scripts/fixtures/web-modules/location.example.conf scripts/web-modules-gen.py deploy/chart/Chart.yaml; do
  [ -f "$ROOT/$f" ] || broken "missing $ROOT/$f"
done

export TMPDIR="$ROOT/.uzi/scratch"
mkdir -p "$TMPDIR"
W=$(mktemp -d "$TMPDIR/assert-web-nginx.XXXXXX")
COOKIE_SENTINEL="wnginx-request-$$-${W##*.}"
SET_COOKIE_SENTINEL="wnginx-response-$$-${W##*.}"
NET="wnginx-$$-net"
CONTAINERS=() # IDs printed by a successful docker create, nothing else
NET_MADE=0
cleanup() {
  local c
  for c in ${CONTAINERS[@]+"${CONTAINERS[@]}"}; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  if [ "$NET_MADE" = 1 ]; then docker network rm "$NET" >/dev/null 2>&1 || true; fi
  rm -rf "$W"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail=0
V=""
ok() { echo "OK: $V: $1"; }
bad() { echo "FAIL: $V: $1" >&2; fail=1; }

# --- image -------------------------------------------------------------------------------
IMAGE=$(awk '/^FROM nginxinc\/nginx-unprivileged/ { print $2; exit }' "$ROOT/web/Dockerfile")
case "$IMAGE" in
  *@sha256:*) ;;
  *) broken "no digest-pinned nginxinc/nginx-unprivileged FROM line in web/Dockerfile (got '${IMAGE}')" ;;
esac
DIGEST=${IMAGE##*@}
case "$DIGEST" in
  sha256:?*) ;;
  *) broken "unusable digest '${DIGEST}' in the web/Dockerfile nginx FROM line" ;;
esac
MIRROR_REF="mirror.gcr.io/nginxinc/nginx-unprivileged@$DIGEST"
HUB_REF="docker.io/nginxinc/nginx-unprivileged@$DIGEST"
# Pick the ref the containers run: a copy already present locally wins (no network), else
# pull the mirror, else Docker Hub.
NGINX_REF=""
NGINX_SRC=""
for ref in "$MIRROR_REF" "$HUB_REF"; do
  if docker image inspect "$ref" >/dev/null 2>&1; then
    NGINX_REF=$ref
    NGINX_SRC="local cache"
    break
  fi
done
if [ -z "$NGINX_REF" ]; then
  if docker pull "$MIRROR_REF" >/dev/null 2>"$W/pull-mirror.err"; then
    NGINX_REF=$MIRROR_REF
    NGINX_SRC="pulled"
  elif docker pull "$HUB_REF" >/dev/null 2>"$W/pull-hub.err"; then
    NGINX_REF=$HUB_REF
    NGINX_SRC="pulled"
  else
    echo "pull of $MIRROR_REF failed:" >&2
    cat "$W/pull-mirror.err" >&2
    echo "pull of $HUB_REF failed:" >&2
    cat "$W/pull-hub.err" >&2
    broken "cannot get the nginx image: neither $MIRROR_REF nor $HUB_REF is present locally or pullable"
  fi
fi
case "$NGINX_REF" in
  mirror.gcr.io/*) NGINX_REGISTRY=mirror.gcr.io ;;
  *) NGINX_REGISTRY=docker.io ;;
esac
echo "nginx image: $NGINX_REF (registry $NGINX_REGISTRY, $NGINX_SRC)"

# --- chart render ------------------------------------------------------------------------
CHART="$W/chart"
cp -R "$ROOT/deploy/chart" "$CHART"
rm -f "$CHART/Chart.lock"
rm -rf "$CHART/charts"
awk '
  BEGIN { skip = 0 }
  /^dependencies:/ { skip = 1; next }
  skip && /^[^[:space:]-]/ { skip = 0 }
  skip { next }
  { print }
' "$ROOT/deploy/chart/Chart.yaml" > "$CHART/Chart.yaml"
# The modules the chart variants run: web.modules[] rendered by the chart itself, so real nginx
# loads the blocks the chart emits (the compose and mock variants use the compose generator).
# `example` is the module origin (the stub, alias modup); a dotless upstream expands to
# <host>.<release namespace>.svc.cluster.local, so the stub also answers to modup.uzi.svc.cluster.local
# (the render below uses --namespace uzi). The ghosts exist only to prove an absent upstream does
# not stop nginx: one name never resolves, one resolves to a port nothing listens on. `cc` proves
# the Cache-Control map variable cannot collide with a module whose id is `cc`.
cat > "$W/chart-modules.yaml" <<'CHARTMODULES'
web:
  nginx:
    resolver: 127.0.0.11
  modules:
    - {id: example, version: 1.0.0, page: Page, label: Example, icon: package, navGroup: modules, upstream: "modup:8080"}
    - {id: ghost-dns, version: 1.0.0, page: Page, label: Ghost DNS, icon: package, navGroup: modules, upstream: "ghost.invalid:8080"}
    - {id: ghost-conn, version: 1.0.0, page: Page, label: Ghost Conn, icon: package, navGroup: modules, upstream: "modup:9"}
    - {id: cc, version: 1.0.0, page: Page, label: CC, icon: package, navGroup: modules, upstream: "modup:8080"}
CHARTMODULES
# render_chart <out-conf> <out-modules-dir> [helm --set args...]: default.conf plus the chart's
# modules.json and locations.conf (the two keys web-deployment.yaml mounts under uzi-modules/).
render_chart() {
  local out="$1" moddir="$2"; shift 2
  helm template uzi "$CHART" --namespace uzi --show-only templates/web-configmap.yaml -f "$W/chart-modules.yaml" "$@" > "$W/cm.yaml" 2> "$W/cm.err" \
    || { cat "$W/cm.err" >&2; broken "helm template of web-configmap.yaml failed"; }
  mkdir -p "$moddir"
  yq '.data["default.conf"]' "$W/cm.yaml" > "$out" || broken "yq could not read default.conf"
  yq '.data["modules.json"]' "$W/cm.yaml" > "$moddir/modules.json" || broken "yq could not read modules.json"
  yq '.data["modules-locations.conf"]' "$W/cm.yaml" > "$moddir/locations.conf" || broken "yq could not read modules-locations.conf"
  local f
  for f in "$out" "$moddir/modules.json" "$moddir/locations.conf"; do
    if [ ! -s "$f" ] || [ "$(head -n 1 "$f")" = "null" ]; then
      broken "the chart rendered no ${f##*/}"
    fi
  done
}
render_chart "$W/chart.conf" "$W/uzi-modules.chart"
render_chart "$W/chartdyn.conf" "$W/uzi-modules.chartdyn" --set web.nginx.dynamicResolution=true
cp "$ROOT/web/nginx.conf" "$W/compose.conf"
cp "$ROOT/web/nginx.mock.conf" "$W/mock.conf"

# --- fixtures ----------------------------------------------------------------------------
DOC="$W/docroot"
STUB="$W/stub"
mkdir -p "$DOC/assets" "$STUB"
printf '<!doctype html><html><body><div id="root">INDEX-MARKER-7f3a</div></body></html>\n' > "$DOC/index.html"
printf '(function(){document.documentElement.dataset.theme="dark";})();\n' > "$DOC/theme-preinit.js"
for ext in js css; do
  i=0
  while [ "$i" -lt 200 ]; do
    printf 'line %s of the %s fixture, padded so that it compresses well and exceeds one KB\n' "$i" "$ext"
    i=$((i + 1))
  done > "$DOC/assets/x-0123abcd.$ext"
done
{
  printf '{"items":['
  i=0
  while [ "$i" -lt 100 ]; do
    [ "$i" -gt 0 ] && printf ','
    printf '{"n":%s,"name":"identity-fixture-entry"}' "$i"
    i=$((i + 1))
  done
  printf ']}\n'
} > "$STUB/identity.json"
{
  printf '{"gz":['
  i=0
  while [ "$i" -lt 100 ]; do
    [ "$i" -gt 0 ] && printf ','
    printf '{"n":%s,"name":"gzip-fixture-entry"}' "$i"
    i=$((i + 1))
  done
  printf ']}\n'
} | gzip -n > "$STUB/gz.json.gz"
printf '{"files":"STUB-FILES-MARKER-91c2"}\n' > "$STUB/files.json"
cat > "$W/stub.conf" <<'STUBCONF'
server {
    listen 8080;
    gzip off;
    location ~ "^/api/runs/[0-9a-fA-F-]{36}/(inputs|completion/decision)$" {
        client_max_body_size 18m;
        return 200 '{"input":"STUB-SEMANTIC-INPUT"}';
    }
    include /usr/share/nginx/stub/cookie-proof.conf;
    include /usr/share/nginx/stub/mod-locations.conf;
    location = /api/identity {
        default_type application/json;
        alias /usr/share/nginx/stub/identity.json;
    }
    location = /api/gz {
        types {}
        default_type application/json;
        add_header Content-Encoding gzip;
        alias /usr/share/nginx/stub/gz.json.gz;
    }
    location = /api/v1/files {
        default_type application/json;
        alias /usr/share/nginx/stub/files.json;
    }
}
STUBCONF
# This endpoint proves both cookie directions without echoing either header to output.
# Only the expected request cookie receives the successful upstream response.
cat > "$STUB/cookie-proof.conf" <<STUBCOOKIE
location = /api/cookie-proof {
    if (\$http_cookie != "__Host-uzi_vault_slot=$COOKIE_SENTINEL") { return 400; }
    add_header Set-Cookie "__Host-uzi_vault_slot=$SET_COOKIE_SENTINEL; Path=/; HttpOnly; SameSite=Strict" always;
    return 200 'COOKIE-UPSTREAM-REACHED';
}
STUBCOOKIE
printf 'COOKIE-UPSTREAM-REACHED' > "$STUB/cookie-proof.body"

# --- runtime web modules fixtures --------------------------------------------------------
# The module origin. Every response carries upstream headers that must NOT survive the web
# nginx: all values contain the marker wnginx-upstream (except Expires), so a leak is found
# by name or by value.
mkdir -p "$STUB/mod/assets" "$STUB/mod/sub/assets"
for f in remoteEntry.js assets/app-0123abcd.js sub/assets/a.js; do
  i=0
  while [ "$i" -lt 100 ]; do
    printf 'module line %s of %s, padded so that it compresses well and exceeds one KB\n' "$i" "$f"
    i=$((i + 1))
  done > "$STUB/mod/$f"
done
cat > "$STUB/mod-poison.conf" <<'STUBPOISON'
add_header Set-Cookie "wnginx-upstream-cookie=1; Path=/" always;
add_header Expires "Thu, 01 Jan 2099 00:00:00 GMT" always;
add_header Cache-Control "public, max-age=31536000, immutable, wnginx-upstream" always;
add_header Content-Security-Policy "default-src * 'unsafe-eval'; report-uri wnginx-upstream" always;
add_header X-Frame-Options "ALLOW-FROM wnginx-upstream" always;
add_header X-Content-Type-Options "wnginx-upstream" always;
add_header Referrer-Policy "unsafe-url wnginx-upstream" always;
add_header X-Accel-Redirect "/internal/wnginx-upstream-target" always;
add_header X-Accel-Expires "999" always;
add_header X-Accel-Limit-Rate "1" always;
add_header X-Accel-Buffering "no" always;
add_header X-Accel-Charset "wnginx-upstream" always;
add_header X-Accel-Uzi-Probe "wnginx-upstream" always;
add_header Service-Worker-Allowed "/wnginx-upstream" always;
add_header Clear-Site-Data "\"cache\", \"wnginx-upstream\"" always;
add_header Strict-Transport-Security "max-age=1; wnginx-upstream" always;
add_header Refresh "0; url=/wnginx-upstream" always;
add_header Permissions-Policy "camera=(), wnginx-upstream=()" always;
add_header Content-Security-Policy-Report-Only "default-src *; report-uri wnginx-upstream" always;
add_header Access-Control-Allow-Origin "wnginx-upstream" always;
add_header Access-Control-Allow-Credentials "true" always;
add_header Access-Control-Allow-Methods "GET, wnginx-upstream" always;
add_header Access-Control-Allow-Headers "wnginx-upstream" always;
add_header Access-Control-Expose-Headers "wnginx-upstream" always;
add_header Report-To "wnginx-upstream" always;
add_header NEL "wnginx-upstream" always;
STUBPOISON
# Static files (Range gives 206, If-Modified-Since gives 304), a missing file (404), an echo
# of the request headers the module origin received, and two relative redirects.
cat > "$STUB/mod-locations.conf" <<'STUBMOD'
location = /modules/example/echo {
    include /usr/share/nginx/stub/mod-poison.conf;
    default_type text/plain;
    return 200 "echo cookie=[$http_cookie] authorization=[$http_authorization] proxyauth=[$http_proxy_authorization] csrf=[$http_x_csrf_token] method=[$request_method]";
}
location = /modules/cc/bad {
    include /usr/share/nginx/stub/mod-poison.conf;
    return 502;
}
location ^~ /modules/cc/ {
    include /usr/share/nginx/stub/mod-poison.conf;
    alias /usr/share/nginx/stub/mod/;
}
location = /modules/example/go {
    include /usr/share/nginx/stub/mod-poison.conf;
    return 302 /modules/example/remoteEntry.js;
}
location = /modules/example/assets/moved-0123abcd.js {
    include /usr/share/nginx/stub/mod-poison.conf;
    return 302 /modules/example/assets/app-0123abcd.js;
}
location ^~ /modules/example/assets/ {
    include /usr/share/nginx/stub/mod-poison.conf;
    if_modified_since before;
    alias /usr/share/nginx/stub/mod/assets/;
}
location ^~ /modules/example/ {
    include /usr/share/nginx/stub/mod-poison.conf;
    if_modified_since before;
    alias /usr/share/nginx/stub/mod/;
}
STUBMOD
# The module config of the compose and mock containers: `example` is served by the stub (alias
# modup); the ghosts exist only to prove an absent upstream does not stop nginx (one name never
# resolves, one resolves to a port nothing listens on). The chart containers get the same set
# from the chart's own render (chart-modules.yaml above).
# The compose and mock variants get their uzi-modules dir from the compose generator itself
# (scripts/web-modules-gen.py), run on the same module set the chart variants render, so real
# nginx loads the blocks and the public list the generator emits. The generator's resolver is
# Docker's embedded DNS (127.0.0.11), which is the resolver of the throwaway network here too.
cat > "$W/gen-modules.json" <<'GENMODULES'
[
  {"id": "example", "version": "1.0.0", "page": "Page", "label": "Example", "icon": "package", "navGroup": "modules", "upstream": "modup:8080"},
  {"id": "ghost-dns", "version": "1.0.0", "page": "Page", "label": "Ghost DNS", "icon": "package", "navGroup": "modules", "upstream": "ghost.invalid:8080"},
  {"id": "ghost-conn", "version": "1.0.0", "page": "Page", "label": "Ghost Conn", "icon": "package", "navGroup": "modules", "upstream": "modup:9"},
  {"id": "cc", "version": "1.0.0", "page": "Page", "label": "CC", "icon": "package", "navGroup": "modules", "upstream": "modup:8080"}
]
GENMODULES
for v in compose mock; do
  python3 -I "$ROOT/scripts/web-modules-gen.py" "$W/gen-modules.json" "$W/uzi-modules.$v" >/dev/null || broken "web-modules-gen.py failed for $v"
done
! grep -q '@' "$W/uzi-modules.compose/locations.conf" || broken "the generator left a placeholder in locations.conf"
! grep -q '@UZI_' "$W/uzi-modules.chart/locations.conf" || broken "the chart left a location placeholder in locations.conf"
# nginx runs as uid 101 inside the image: everything copied in must be world-readable.
# Exercise the exact owner-input proxy ceiling and its first rejected byte.
dd if=/dev/zero of="$W/approval-exact.bin" bs=1048576 count=17 2>/dev/null
cp "$W/approval-exact.bin" "$W/approval-over.bin"
printf x >> "$W/approval-over.bin"
dd if=/dev/zero of="$W/ordinary-over.bin" bs=1048576 count=1 2>/dev/null
printf x >> "$W/ordinary-over.bin"
find "$W" -type d -exec chmod 755 {} +
find "$W" -type f -exec chmod 644 {} +

# --- containers --------------------------------------------------------------------------
docker network create "$NET" >/dev/null || broken "cannot create network $NET"
NET_MADE=1

container_logs() { docker logs "$1" 2>&1 | tail -n 20 >&2 || true; }

# start_ctr <name> <conf> <docroot-or-empty> [extra docker create args...]
start_ctr() {
  local name="$1" conf="$2" doc="$3" id; shift 3
  # Record the container for cleanup only once create succeeded, and by the ID it printed:
  # a failed create (the name is held by a container this run did not make) records nothing.
  id=$(docker create --name "$name" --network "$NET" "$@" "$NGINX_REF") || broken "docker create $name failed"
  [ -n "$id" ] || broken "docker create $name printed no container id"
  CONTAINERS+=("$id")
  docker cp "$conf" "$name:/etc/nginx/conf.d/default.conf" || broken "docker cp conf to $name failed"
  if [ -n "$doc" ]; then
    docker cp "$doc/." "$name:/usr/share/nginx/html" || broken "docker cp docroot to $name failed"
  fi
}

start_ctr "wnginx-$$-up" "$W/stub.conf" "" --network-alias api --network-alias modup --network-alias modup.uzi.svc.cluster.local --dns-option ndots:0
# docker cp creates a missing destination directory when the source is a directory.
docker cp "$STUB/." "wnginx-$$-up:/usr/share/nginx/stub" || broken "docker cp stub fixtures failed"
docker start "wnginx-$$-up" >/dev/null || { container_logs "wnginx-$$-up"; broken "stub upstream did not start"; }
stub_ready=0
for _ in $(seq 50); do
  if docker exec "wnginx-$$-up" wget -q -O /dev/null http://127.0.0.1:8080/api/identity 2>/dev/null; then stub_ready=1; break; fi
  sleep 0.2
done
[ "$stub_ready" = 1 ] || { container_logs "wnginx-$$-up"; broken "stub upstream never became ready"; }

for v in $VARIANTS; do
  start_ctr "wnginx-$$-$v" "$W/$v.conf" "$DOC" --dns-option ndots:0 -p 127.0.0.1::8080
  # The image's main nginx.conf (njs), the njs file and the module dir, as the images ship them.
  docker cp "$ROOT/web/nginx-main.conf" "wnginx-$$-$v:/etc/nginx/nginx.conf" || broken "docker cp main conf to $v failed"
  docker cp "$ROOT/web/nginx-modules.js" "wnginx-$$-$v:/etc/nginx/uzi-modules.js" || broken "docker cp njs file to $v failed"
  docker cp "$W/uzi-modules.$v/." "wnginx-$$-$v:/etc/nginx/uzi-modules" || broken "docker cp uzi-modules to $v failed"
  docker start "wnginx-$$-$v" >/dev/null || { container_logs "wnginx-$$-$v"; broken "$v web container did not start"; }
done

# The image-defaults containers (one per variant, the real web/nginx-modules-default/ dir, no
# fixture modules) and the dynamic-resolution chart container.
for v in $VARIANTS; do
  start_ctr "wnginx-$$-$v-def" "$W/$v.conf" "$DOC" --dns-option ndots:0 -p 127.0.0.1::8080
  docker cp "$ROOT/web/nginx-main.conf" "wnginx-$$-$v-def:/etc/nginx/nginx.conf" || broken "docker cp main conf to $v-def failed"
  docker cp "$ROOT/web/nginx-modules.js" "wnginx-$$-$v-def:/etc/nginx/uzi-modules.js" || broken "docker cp njs file to $v-def failed"
  docker cp "$ROOT/web/nginx-modules-default/." "wnginx-$$-$v-def:/etc/nginx/uzi-modules" || broken "docker cp default modules to $v-def failed"
  docker start "wnginx-$$-$v-def" >/dev/null || { container_logs "wnginx-$$-$v-def"; broken "$v-def web container did not start"; }
done
start_ctr "wnginx-$$-chartdyn" "$W/chartdyn.conf" "$DOC" --dns-option ndots:0 -p 127.0.0.1::8080
docker cp "$ROOT/web/nginx-main.conf" "wnginx-$$-chartdyn:/etc/nginx/nginx.conf" || broken "docker cp main conf to chartdyn failed"
docker cp "$ROOT/web/nginx-modules.js" "wnginx-$$-chartdyn:/etc/nginx/uzi-modules.js" || broken "docker cp njs file to chartdyn failed"
docker cp "$W/uzi-modules.chartdyn/." "wnginx-$$-chartdyn:/etc/nginx/uzi-modules" || broken "docker cp uzi-modules to chartdyn failed"
docker start "wnginx-$$-chartdyn" >/dev/null || { container_logs "wnginx-$$-chartdyn"; broken "chartdyn web container did not start"; }

# port_of <variant>: the published loopback port, kept in a file (bash 3 has no assoc arrays).
port_of() { cat "$W/port.$1"; }
for v in $VARIANTS $(for x in $VARIANTS; do printf '%s-def ' "$x"; done) chartdyn; do
  p=$(docker port "wnginx-$$-$v" 8080/tcp | grep '^127\.0\.0\.1:' | head -n 1 | sed 's/.*://' || true)
  [ -n "$p" ] || broken "no published port for $v"
  echo "$p" > "$W/port.$v"
  ready=0
  for _ in $(seq 50); do
    if curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$p/"; then ready=1; break; fi
    sleep 0.2
  done
  [ "$ready" = 1 ] || { container_logs "wnginx-$$-$v"; broken "$v web container never became ready"; }
done

# --- helpers -----------------------------------------------------------------------------
STATUS=""
# fetch <tag> <port> <path> [curl args...]: raw bytes (no --compressed); headers in $W/<tag>.h.
fetch() {
  local tag="$1" port="$2" path="$3"; shift 3
  : > "$W/$tag.h"; : > "$W/$tag.b"
  STATUS=$(curl -sS --max-time 10 -D "$W/$tag.h" -o "$W/$tag.b" -w '%{http_code}' "$@" "http://127.0.0.1:$port$path" 2>"$W/$tag.err") || STATUS="000"
}

# hvals <tag> <name>: every value of the named header (case-insensitive), CR stripped.
hvals() {
  awk -v n="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')" '
    { l = $0; sub(/\r$/, "", l); i = index(l, ":")
      if (i > 0 && tolower(substr(l, 1, i - 1)) == n) { v = substr(l, i + 1); sub(/^[ \t]+/, "", v); print v } }
  ' "$W/$1.h"
}
hcount() { hvals "$1" "$2" | wc -l | tr -d ' '; }
# hhas <tag> <name> <lowercase-substring>
hhas() { hvals "$1" "$2" | tr '[:upper:]' '[:lower:]' | grep -qF -- "$3"; }

# decoded_eq <tag> <file>: gunzip the body and compare with the fixture.
decoded_eq() { gzip -dc "$W/$1.b" 2>/dev/null | cmp -s - "$2"; }
raw_eq() { cmp -s "$W/$1.b" "$2"; }

# check <description> <command...>: ok/bad on the command's status.
check() {
  local desc="$1"; shift
  if "$@"; then ok "$desc"; else bad "$desc"; fi
}

# sec_headers <tag> <label>: the four security headers, each exactly once, with exact values.
sec_headers() {
  local tag="$1" label="$2" bad_list="" n val
  for pair in "X-Content-Type-Options=nosniff" "X-Frame-Options=DENY" "Referrer-Policy=no-referrer" "Content-Security-Policy=$CSP"; do
    n=${pair%%=*}; val=${pair#*=}
    if [ "$(hcount "$tag" "$n")" != 1 ]; then
      bad_list="$bad_list $n(count=$(hcount "$tag" "$n"))"
    elif [ "$(hvals "$tag" "$n")" != "$val" ]; then
      bad_list="$bad_list $n(value=$(hvals "$tag" "$n"))"
    fi
  done
  if [ -z "$bad_list" ]; then ok "security headers once and exact on $label"; else bad "security headers on $label ($bad_list )"; fi
}

has_status() { [ "$STATUS" = "$1" ]; }
no_ce() { [ "$(hcount "$1" content-encoding)" = 0 ]; }
not_index() { ! grep -q INDEX-MARKER "$W/$1.b"; }
cc_not_immutable() { ! hhas "$1" cache-control immutable; }

# Runtime web modules helpers. cc_is: exactly one Cache-Control header, with exactly this value.
cc_is() { [ "$(hcount "$1" cache-control)" = 1 ] && [ "$(hvals "$1" cache-control)" = "$2" ]; }
no_upstream_marks() { ! grep -qi 'wnginx-upstream' "$W/$1.h"; }
no_accel() { ! grep -qi '^x-accel-' "$W/$1.h"; }
# no_hidden_extra <tag>: none of $HIDDEN_EXTRA is present, by name.
no_hidden_extra() {
  local h
  for h in $HIDDEN_EXTRA; do [ "$(hcount "$1" "$h")" = 0 ] || return 1; done
}
no_cookie_expires() { [ "$(hcount "$1" set-cookie)" = 0 ] && [ "$(hcount "$1" expires)" = 0 ]; }
body_has() { grep -qF -- "$2" "$W/$1.b"; }
body_lacks() { ! grep -qF -- "$2" "$W/$1.b"; }
# mod_cell <label> <tag> <port> <path> <status> <cache-control> [curl args...]: one request,
# checked on the raw status and headers.
mod_cell() {
  local label="$1" tag="$2" port="$3" path="$4" st="$5" cc="$6"; shift 6
  fetch "$tag" "$port" "$path" "$@"
  check "$label: status $st (got $STATUS)" has_status "$st"
  check "$label: exactly one Cache-Control '$cc'" cc_is "$tag" "$cc"
  check "$label: no upstream header value leaked" no_upstream_marks "$tag"
  check "$label: no X-Accel-* header" no_accel "$tag"
  check "$label: no Service-Worker-Allowed, Clear-Site-Data, HSTS, Refresh, Permissions-Policy, CSP-Report-Only, CORS, Report-To or NEL" no_hidden_extra "$tag"
  check "$label: no Set-Cookie or Expires" no_cookie_expires "$tag"
  sec_headers "$tag" "$label"
}

cookie_response() {
  [ "$(hcount "$1" set-cookie)" = 1 ] &&
    [ "$(hvals "$1" set-cookie)" = "__Host-uzi_vault_slot=$SET_COOKIE_SENTINEL; Path=/; HttpOnly; SameSite=Strict" ]
}

# Stop before collecting so nginx flushes pending access-log buffers. docker logs combines
# stdout/stderr, including the image's access.log/error.log symlinks to those streams.
# docker cp preserves symlinks; only regular files are read, never stream links or FIFOs.
cookie_logs_absent() {
  local variant="$1" logs="$W/$1.logs" dir="$W/$1.logfiles" rc
  docker stop "wnginx-$$-$variant" >"$W/$variant.stop" 2>&1 ||
    broken "cookie proof: cannot stop web container"
  docker logs "wnginx-$$-$variant" >"$logs" 2>&1 ||
    broken "cookie proof: cannot collect docker output"
  mkdir -p "$dir"
  docker cp "wnginx-$$-$variant:/var/log/nginx/." "$dir" >"$W/$variant.cp" 2>&1 ||
    broken "cookie proof: cannot collect image log directory"
  find "$dir" -type f -exec cat {} + >>"$logs" 2>"$W/$variant.read.err" ||
    broken "cookie proof: cannot read image log files"
  rc=0
  grep -qF -e "$COOKIE_SENTINEL" -e "$SET_COOKIE_SENTINEL" "$logs" || rc=$?
  case "$rc" in
    0) return 1 ;;
    1) return 0 ;;
    *) broken "cookie proof: cannot inspect collected logs" ;;
  esac
}

# M5 mutation recipe (disposable candidate export only; not run here):
# Prepend an http-scope log_format cookie_probe '$http_cookie $sent_http_set_cookie';
# to compose nginx.conf (before server), replace its server access_log off with
# access_log /var/log/nginx/access.log cookie_probe; then run this script on that export.
# Repeat with /var/log/nginx/error.log to exercise stderr, and with
# /var/log/nginx/cookie-probe.log for a regular file. Each run must exit 1 with
# "cookie proof: sentinels absent from web logs" failing, while the three positive
# cookie proof assertions pass. Repeat in the chart default.conf block for Helm.
# Never print the collected logs or headers: assertion names alone are sufficient.

# --- assertions --------------------------------------------------------------------------
# Drift guard: web/nginx-main.conf must be the pinned image's own nginx.conf plus exactly two
# added lines, load_module first (main context) and js_import directly before the conf.d
# include (http context). The stub container runs the same image, so its stock file is the base.
V="drift"
docker cp "wnginx-$$-up:/etc/nginx/nginx.conf" "$W/image-nginx.conf" || broken "docker cp of the image's nginx.conf failed"
LOAD_LINE='load_module modules/ngx_http_js_module.so;'
IMPORT_LINE='    js_import uzi_modules from /etc/nginx/uzi-modules.js;'
INCLUDE_LINE='    include /etc/nginx/conf.d/*.conf;'
main_without_added() { grep -vxF -e "$LOAD_LINE" -e "$IMPORT_LINE" "$ROOT/web/nginx-main.conf" | cmp -s - "$W/image-nginx.conf"; }
main_line_counts() {
  [ "$(grep -cxF -e "$LOAD_LINE" "$ROOT/web/nginx-main.conf")" = 1 ] &&
    [ "$(grep -cxF -e "$IMPORT_LINE" "$ROOT/web/nginx-main.conf")" = 1 ] &&
    [ "$(wc -l < "$ROOT/web/nginx-main.conf")" = "$(($(wc -l < "$W/image-nginx.conf") + 2))" ]
}
main_line_places() {
  [ "$(head -n 1 "$ROOT/web/nginx-main.conf")" = "$LOAD_LINE" ] &&
    [ "$(grep -xF -B1 -e "$INCLUDE_LINE" "$ROOT/web/nginx-main.conf" | head -n 1)" = "$IMPORT_LINE" ]
}
MOCK_IMAGE=$(awk '/^FROM nginxinc\/nginx-unprivileged/ { print $2; exit }' "$ROOT/web/Dockerfile.mock")
same_digest() { [ -n "$MOCK_IMAGE" ] && [ "${MOCK_IMAGE##*@}" = "$DIGEST" ]; }
check "web/Dockerfile.mock pins the same final-stage nginx image digest as web/Dockerfile" same_digest
dockerfile_copies() {
  grep -qxF 'COPY web/nginx-main.conf /etc/nginx/nginx.conf' "$ROOT/$1" &&
    grep -qxF 'COPY web/nginx-modules.js /etc/nginx/uzi-modules.js' "$ROOT/$1" &&
    grep -qxF 'COPY web/nginx-modules-default/ /etc/nginx/uzi-modules/' "$ROOT/$1"
}
check "web/Dockerfile COPYs nginx-main.conf, nginx-modules.js and nginx-modules-default/" dockerfile_copies web/Dockerfile
check "web/Dockerfile.mock COPYs nginx-main.conf, nginx-modules.js and nginx-modules-default/" dockerfile_copies web/Dockerfile.mock
check "web/nginx-main.conf minus the two added lines equals the pinned image's nginx.conf" main_without_added
check "web/nginx-main.conf adds exactly one load_module and one js_import line" main_line_counts
check "load_module is the first line and js_import sits directly before the conf.d include" main_line_places

# Controls on the module origin itself: it really does send the hostile headers and echo the
# request headers it receives, so the absences below are not vacuous.
V="module-origin"
docker exec "wnginx-$$-up" wget -S -q -O /dev/null http://127.0.0.1:8080/modules/example/remoteEntry.js >"$W/origin.h" 2>&1 || true
for hn in Set-Cookie Expires Cache-Control Content-Security-Policy X-Frame-Options X-Content-Type-Options Referrer-Policy \
  X-Accel-Redirect X-Accel-Expires X-Accel-Limit-Rate X-Accel-Buffering X-Accel-Charset X-Accel-Uzi-Probe \
  $HIDDEN_EXTRA; do
  check "control: the module origin sends $hn" grep -qi "^ *$hn:" "$W/origin.h"
done
docker exec "wnginx-$$-up" wget -q -O - --header 'Cookie: ctl-cookie' --header 'Authorization: ctl-auth' \
  --header 'Proxy-Authorization: ctl-pauth' --header 'X-CSRF-Token: ctl-csrf' \
  http://127.0.0.1:8080/modules/example/echo >"$W/origin.echo" 2>/dev/null || true
check "control: the module origin echoes Cookie and Authorization when sent" grep -qF 'cookie=[ctl-cookie] authorization=[ctl-auth]' "$W/origin.echo"
check "control: the module origin echoes Proxy-Authorization and X-CSRF-Token when sent" grep -qF 'proxyauth=[ctl-pauth] csrf=[ctl-csrf]' "$W/origin.echo"

for v in $VARIANTS; do
  V=$v
  port=$(port_of "$v")
  if docker exec "wnginx-$$-$v" nginx -t >"$W/$v.t" 2>&1; then ok "nginx -t"; else bad "nginx -t ($(tr '\n' ' ' < "$W/$v.t"))"; fi

  for ext in js css; do
    f="$DOC/assets/x-0123abcd.$ext"; p="/assets/x-0123abcd.$ext"
    fetch "$v-$ext-gz" "$port" "$p" -H 'Accept-Encoding: gzip'
    t="$v-$ext-gz"
    check "$p gzip: status 200 (got $STATUS)" has_status 200
    check "$p gzip: Content-Encoding gzip" hhas "$t" content-encoding gzip
    check "$p gzip: Vary has Accept-Encoding" hhas "$t" vary accept-encoding
    check "$p gzip: decoded body equals the file" decoded_eq "$t" "$f"
    check "$p: Cache-Control immutable" hhas "$t" cache-control immutable
    check "$p: Cache-Control max-age=31536000" hhas "$t" cache-control max-age=31536000
    sec_headers "$t" "$p (gzip)"

    fetch "$v-$ext-id" "$port" "$p"
    t="$v-$ext-id"
    check "$p identity: status 200 (got $STATUS)" has_status 200
    check "$p identity: no Content-Encoding" no_ce "$t"
    check "$p identity: body equals the file" raw_eq "$t" "$f"
    sec_headers "$t" "$p (identity)"
  done

  fetch "$v-via" "$port" /assets/x-0123abcd.js -H 'Accept-Encoding: gzip' -H 'Via: 1.1 proxy'
  check "/assets js behind a Via proxy: Content-Encoding gzip (gzip_proxied any)" hhas "$v-via" content-encoding gzip

  fetch "$v-404" "$port" /assets/nope.js
  check "/assets/nope.js: status 404 (got $STATUS)" has_status 404
  check "/assets/nope.js: Cache-Control not immutable" cc_not_immutable "$v-404"
  check "/assets/nope.js: body is not the SPA index" not_index "$v-404"
  sec_headers "$v-404" "/assets/nope.js (404)"

  for p in / /runs/123; do
    tag="$v-spa$(printf '%s' "$p" | tr -c 'a-z0-9' '_')"
    fetch "$tag" "$port" "$p"
    check "$p: status 200 (got $STATUS)" has_status 200
    check "$p: body is index.html" raw_eq "$tag" "$DOC/index.html"
    check "$p: Cache-Control no-cache" hhas "$tag" cache-control no-cache
    check "$p: Cache-Control not immutable" cc_not_immutable "$tag"
    sec_headers "$tag" "$p"
  done

  fetch "$v-theme" "$port" /theme-preinit.js
  check "/theme-preinit.js: status 200 (got $STATUS)" has_status 200
  check "/theme-preinit.js: Cache-Control no-cache" hhas "$v-theme" cache-control no-cache
  check "/theme-preinit.js: Cache-Control not immutable" cc_not_immutable "$v-theme"

  if [ "$v" = mock ]; then
    fetch "$v-api" "$port" /api/identity -H 'Accept-Encoding: gzip'
    check "/api/identity: 404 (got $STATUS)" has_status 404
  else
    fetch "$v-cookie" "$port" /api/cookie-proof -H "Cookie: __Host-uzi_vault_slot=$COOKIE_SENTINEL"
    check "cookie proof: upstream accepted request cookie" has_status 200
    check "cookie proof: upstream body received" raw_eq "$v-cookie" "$STUB/cookie-proof.body"
    check "cookie proof: upstream HttpOnly Set-Cookie received" cookie_response "$v-cookie"
    fetch "$v-api" "$port" /api/identity -H 'Accept-Encoding: gzip'
    check "/api/identity: status 200 (got $STATUS)" has_status 200
    check "/api/identity: not compressed by web" no_ce "$v-api"
    check "/api/identity: body equals the stub's" raw_eq "$v-api" "$STUB/identity.json"
    fetch "$v-apigz" "$port" /api/gz -H 'Accept-Encoding: gzip'
    check "/api/gz: Content-Encoding gzip kept" hhas "$v-apigz" content-encoding gzip
    check "/api/gz: body equals the stub's gzip bytes" raw_eq "$v-apigz" "$STUB/gz.json.gz"
    fetch "$v-apifiles" "$port" /api/v1/files
    check "/api/v1/files: status 200 (got $STATUS)" has_status 200
    check "/api/v1/files: reaches the stub" raw_eq "$v-apifiles" "$STUB/files.json"
    for owner_path in inputs completion/decision; do
      owner_input=/api/runs/12345678-1234-4123-8123-123456789012/$owner_path
      owner_tag=${owner_path//\//-}
      fetch "$v-$owner_tag-exact" "$port" "$owner_input" -X POST --data-binary "@$W/approval-exact.bin"
      check "semantic $owner_path: exact 17 MiB reaches upstream (got $STATUS)" has_status 200
      fetch "$v-$owner_tag-over" "$port" "$owner_input" -X POST --data-binary "@$W/approval-over.bin"
      check "semantic $owner_path: 17 MiB plus one byte rejected (got $STATUS)" has_status 413
    done
    fetch "$v-ordinary-over" "$port" /api/identity -X POST --data-binary "@$W/ordinary-over.bin"
    check "ordinary API: 1 MiB plus one byte rejected (got $STATUS)" has_status 413
    fetch "$v-worker-over" "$port" "/api/worker/runs/12345678-1234-4123-8123-123456789012/inputs" -X POST --data-binary "@$W/ordinary-over.bin"
    check "worker inputs: larger owner limit is not inherited (got $STATUS)" has_status 413
  fi

  # --- runtime web modules ---------------------------------------------------------------
  IMM="public, max-age=31536000, immutable"
  A=/modules/example/assets/app-0123abcd.js
  R=/modules/example/remoteEntry.js
  IMS='If-Modified-Since: Fri, 01 Jan 2100 00:00:00 GMT'
  # Cache-Control table: one request per path and final status, exactly one header each.
  mod_cell "asset 200" "$v-m-a200" "$port" "$A" 200 "$IMM"
  mod_cell "asset 206" "$v-m-a206" "$port" "$A" 206 "$IMM" -H 'Range: bytes=0-9'
  mod_cell "asset 304" "$v-m-a304" "$port" "$A" 304 "$IMM" -H "$IMS"
  mod_cell "asset 302" "$v-m-a302" "$port" /modules/example/assets/moved-0123abcd.js 302 no-store
  mod_cell "asset 404" "$v-m-a404" "$port" /modules/example/assets/missing-0123abcd.js 404 no-store
  check "asset 404: Cache-Control not immutable" cc_not_immutable "$v-m-a404"
  mod_cell "module path 200" "$v-m-r200" "$port" "$R" 200 no-cache
  mod_cell "module path 206" "$v-m-r206" "$port" "$R" 206 no-cache -H 'Range: bytes=0-9'
  mod_cell "module path 304" "$v-m-r304" "$port" "$R" 304 no-cache -H "$IMS"
  mod_cell "module path 302" "$v-m-r302" "$port" /modules/example/go 302 no-store
  mod_cell "module path 404" "$v-m-r404" "$port" /modules/example/missing.js 404 no-store
  mod_cell "nested assets dir is not a hashed asset" "$v-m-nested" "$port" /modules/example/sub/assets/a.js 200 no-cache
  mod_cell "asset HEAD" "$v-m-ahead" "$port" "$A" 200 "$IMM" -I
  mod_cell "module path HEAD" "$v-m-rhead" "$port" "$R" 200 no-cache -I
  # Bodies are the module origin's, X-Accel-Redirect is not followed, ranges pass through.
  fetch "$v-m-body" "$port" "$R"
  check "module body is the origin's file" raw_eq "$v-m-body" "$STUB/mod/remoteEntry.js"
  check "X-Accel-Redirect not followed (body is not the SPA index)" not_index "$v-m-body"
  fetch "$v-m-gz" "$port" "$R" -H 'Accept-Encoding: gzip'
  check "module path gzip: Content-Encoding gzip" hhas "$v-m-gz" content-encoding gzip
  check "module path gzip: decoded body equals the file" decoded_eq "$v-m-gz" "$STUB/mod/remoteEntry.js"
  check "module path gzip: still exactly one Cache-Control no-cache" cc_is "$v-m-gz" no-cache
  fetch "$v-m-range" "$port" "$R" -H 'Range: bytes=0-9'
  check "module range body is the first 10 bytes" [ "$(cat "$W/$v-m-range.b")" = "$(head -c 10 "$STUB/mod/remoteEntry.js")" ]
  # Request headers: Cookie and Authorization are cleared before the module origin.
  fetch "$v-m-echo" "$port" /modules/example/echo -H "Cookie: wnginx-modcookie-$$" -H "Authorization: Bearer wnginx-modauth-$$" \
    -H "Proxy-Authorization: Basic wnginx-modpauth-$$" -H "X-CSRF-Token: wnginx-modcsrf-$$"
  check "echo: status 200 (got $STATUS)" has_status 200
  check "echo: Cookie not forwarded" body_has "$v-m-echo" 'cookie=[]'
  check "echo: Authorization not forwarded" body_has "$v-m-echo" 'authorization=[]'
  check "echo: Proxy-Authorization not forwarded" body_has "$v-m-echo" 'proxyauth=[]'
  check "echo: X-CSRF-Token not forwarded" body_has "$v-m-echo" 'csrf=[]'
  check "echo: no sentinel reached the module origin" body_lacks "$v-m-echo" "wnginx-mod"
  # Methods: GET and HEAD only.
  for m in POST PUT DELETE; do
    mod_cell "$m is refused" "$v-m-$m" "$port" /modules/example/echo 403 no-store -X "$m" --data x
    check "$m: the module origin was not reached" body_lacks "$v-m-$m" "echo cookie="
  done
  # A module whose id is `cc` (its variable is $uzi_module_cc): the Cache-Control map variable
  # must not share that name, or the header would carry the upstream host:port.
  mod_cell "cc module entry" "$v-m-cc200" "$port" /modules/cc/remoteEntry.js 200 no-cache
  mod_cell "cc module asset" "$v-m-ccasset" "$port" /modules/cc/assets/app-0123abcd.js 200 "$IMM"
  mod_cell "cc module 404" "$v-m-cc404" "$port" /modules/cc/missing.js 404 no-store
  mod_cell "cc module upstream 502" "$v-m-cc502" "$port" /modules/cc/bad 502 no-store
  check "cc module: Cache-Control carries no host:port" [ -z "$(hvals "$v-m-cc200" cache-control | grep -F modup || true)" ]
  # Unknown id and absent upstream.
  mod_cell "unknown module id" "$v-m-nope" "$port" /modules/nope/x 404 no-store
  check "unknown module id: body is not the SPA index" not_index "$v-m-nope"
  mod_cell "unresolvable upstream" "$v-m-dns" "$port" /modules/ghost-dns/x 502 no-store
  mod_cell "refused upstream" "$v-m-conn" "$port" /modules/ghost-conn/x 502 no-store
  mod_cell "refused upstream, asset path" "$v-m-connasset" "$port" /modules/ghost-conn/assets/a-0123abcd.js 502 no-store
  fetch "$v-m-spa" "$port" /
  check "/ is still served with absent module upstreams (got $STATUS)" has_status 200
  check "/ is the SPA index" raw_eq "$v-m-spa" "$DOC/index.html"
  # The manifest.
  fetch "$v-m-json" "$port" /modules.json
  check "/modules.json: status 200 (got $STATUS)" has_status 200
  check "/modules.json: body is the module dir's modules.json (the fixture, or the chart's render)" raw_eq "$v-m-json" "$W/uzi-modules.$v/modules.json"
  check "/modules.json: exactly one Cache-Control no-cache" cc_is "$v-m-json" no-cache
  check "/modules.json: Content-Type application/json" hhas "$v-m-json" content-type application/json
  sec_headers "$v-m-json" "/modules.json"

  # Last, because it stops the container to flush its logs: the sentinels must also be absent
  # from the logs after the module requests above (which carried their own Cookie sentinels).
  if [ "$v" != mock ]; then
    check "cookie proof: sentinels absent from web logs" cookie_logs_absent "$v"
  fi
done

# Image defaults: the real web/nginx-modules-default/ dir, no fixture modules.
for v in $VARIANTS; do
  V="$v-defaults"
  port=$(port_of "$v-def")
  if docker exec "wnginx-$$-$v-def" nginx -t >"$W/$v-def.t" 2>&1; then ok "nginx -t"; else bad "nginx -t ($(tr '\n' ' ' < "$W/$v-def.t"))"; fi
  fetch "$v-def-json" "$port" /modules.json
  check "/modules.json: status 200 (got $STATUS)" has_status 200
  check "/modules.json: Content-Type application/json" hhas "$v-def-json" content-type application/json
  check "/modules.json: body is []" [ "$(cat "$W/$v-def-json.b")" = '[]' ]
  check "/modules.json: exactly one Cache-Control no-cache" cc_is "$v-def-json" no-cache
  sec_headers "$v-def-json" "/modules.json"
  mod_cell "default image, unknown module" "$v-def-x" "$port" /modules/x/y 404 no-store
  fetch "$v-def-spa" "$port" /
  check "/ is the SPA index (got $STATUS)" raw_eq "$v-def-spa" "$DOC/index.html"
done

# The chart with dynamicResolution=true and resolver 127.0.0.11: module routes and an absent upstream.
V="chartdyn"
port=$(port_of chartdyn)
if docker exec "wnginx-$$-chartdyn" nginx -t >"$W/chartdyn.t" 2>&1; then ok "nginx -t"; else bad "nginx -t ($(tr '\n' ' ' < "$W/chartdyn.t"))"; fi
check "the rendered chart has a resolver 127.0.0.11 line" grep -q 'resolver 127.0.0.11' "$W/chartdyn.conf"
mod_cell "module path 200" "$V-m-r200" "$port" /modules/example/remoteEntry.js 200 no-cache
mod_cell "asset 200" "$V-m-a200" "$port" /modules/example/assets/app-0123abcd.js 200 "$IMM"
mod_cell "cc module entry" "$V-m-cc200" "$port" /modules/cc/remoteEntry.js 200 no-cache
mod_cell "unresolvable upstream" "$V-m-dns" "$port" /modules/ghost-dns/x 502 no-store
mod_cell "refused upstream" "$V-m-conn" "$port" /modules/ghost-conn/x 502 no-store
fetch "$V-spa" "$port" /
check "/ is still served with absent module upstreams (got $STATUS)" has_status 200
check "/ is the SPA index" raw_eq "$V-spa" "$DOC/index.html"

V="summary"
if [ "$fail" = 0 ]; then echo "OK: every property holds"; exit 0; fi
echo "FAIL: at least one property does not hold" >&2
exit 1
