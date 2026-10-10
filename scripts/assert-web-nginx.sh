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
# HOW. The image pinned in web/Dockerfile runs once per config (compose, chart, mock) next
# to a stub upstream that answers as the api. Fixtures are generated here, deterministically,
# and compared byte for byte (curl never decodes; gzip -dc and cmp do). Nothing is
# bind-mounted (the daemon may not see this host's paths): files are docker cp'd in. Every
# container and the network are named wnginx-<pid>-*, never uzi-*. Only what this run created
# is removed on exit: a container by the ID its successful docker create printed (never by
# name), the network only once docker network create succeeded. The web containers use
# --dns-option ndots:0 so the chart's static proxy_pass host "api" resolves to the stub
# alias and not to a search-domain lookalike.
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

broken() { echo "BROKEN: $*" >&2; exit 2; }

for tool in docker helm yq curl gzip cmp; do
  command -v "$tool" >/dev/null 2>&1 || broken "$tool not on PATH"
done
docker info >/dev/null 2>&1 || broken "docker daemon not reachable"
for f in web/Dockerfile web/nginx.conf web/nginx.mock.conf deploy/chart/Chart.yaml; do
  [ -f "$ROOT/$f" ] || broken "missing $ROOT/$f"
done

W=$(mktemp -d "${TMPDIR:-/tmp}/assert-web-nginx.XXXXXX")
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
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  docker pull "$IMAGE" >/dev/null 2>"$W/pull.err" || { cat "$W/pull.err" >&2; broken "cannot pull $IMAGE"; }
fi

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
helm template uzi "$CHART" --namespace uzi --show-only templates/web-configmap.yaml > "$W/cm.yaml" 2> "$W/cm.err" \
  || { cat "$W/cm.err" >&2; broken "helm template of web-configmap.yaml failed"; }
yq '.data["default.conf"]' "$W/cm.yaml" > "$W/chart.conf" || broken "yq could not read default.conf"
if [ ! -s "$W/chart.conf" ] || [ "$(head -n 1 "$W/chart.conf")" = "null" ]; then
  broken "the chart rendered no default.conf"
fi
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
# nginx runs as uid 101 inside the image: everything copied in must be world-readable.
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
  id=$(docker create --name "$name" --network "$NET" "$@" "$IMAGE") || broken "docker create $name failed"
  [ -n "$id" ] || broken "docker create $name printed no container id"
  CONTAINERS+=("$id")
  docker cp "$conf" "$name:/etc/nginx/conf.d/default.conf" || broken "docker cp conf to $name failed"
  if [ -n "$doc" ]; then
    docker cp "$doc/." "$name:/usr/share/nginx/html" || broken "docker cp docroot to $name failed"
  fi
}

start_ctr "wnginx-$$-up" "$W/stub.conf" "" --network-alias api --dns-option ndots:0
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
  docker start "wnginx-$$-$v" >/dev/null || { container_logs "wnginx-$$-$v"; broken "$v web container did not start"; }
done

# port_of <variant>: the published loopback port, kept in a file (bash 3 has no assoc arrays).
port_of() { cat "$W/port.$1"; }
for v in $VARIANTS; do
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

# --- assertions --------------------------------------------------------------------------
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
  fi
done

V="summary"
if [ "$fail" = 0 ]; then echo "OK: every property holds"; exit 0; fi
echo "FAIL: at least one property does not hold" >&2
exit 1
