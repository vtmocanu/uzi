#!/usr/bin/env bash
# Standalone Compose nginx DNS regression. Run after the Docker preflight.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${1:---green}"
case "$MODE" in --green|--expect-stale) ;; *) echo "usage: $0 [--green|--expect-stale]" >&2; exit 2 ;; esac

RUN_ID="cwebdns-$$-$RANDOM"
WEB_IMAGE="$RUN_ID-web"
API_IMAGE="$RUN_ID-api"
SCRATCH="$(mktemp -d)"
NETWORKS=()
CONTAINERS=()
cleanup() {
  local name
  for name in "${CONTAINERS[@]}"; do docker rm -f "$name" >/dev/null 2>&1 || true; done
  for name in "${NETWORKS[@]}"; do docker network rm "$name" >/dev/null 2>&1 || true; done
  docker image rm "$WEB_IMAGE" "$API_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

fail() { echo "compose-web-dns: $*" >&2; exit 1; }
note() { echo "compose-web-dns: $*"; }

docker build -q -f "$ROOT/web/Dockerfile" -t "$WEB_IMAGE" "$ROOT" >/dev/null
docker build -q -f "$ROOT/e2e/compose-web-dns-fixture/Dockerfile" \
  -t "$API_IMAGE" "$ROOT/e2e/compose-web-dns-fixture" >/dev/null
docker run --rm "$WEB_IMAGE" nginx -t >/dev/null

new_network() {
  NET="$RUN_ID-$1"
  docker network create "$NET" >/dev/null
  NETWORKS+=("$NET")
  API="$NET-api"
  FILLER="$NET-filler"
  WEB="$NET-web"
}
start_api() {
  docker run -d --name "$API" --network "$NET" --network-alias api \
    -e MARKER=api "$API_IMAGE" >/dev/null
  CONTAINERS+=("$API")
}
start_filler() {
  docker run -d --name "$FILLER" --network "$NET" \
    -e MARKER=filler "$API_IMAGE" >/dev/null
  CONTAINERS+=("$FILLER")
}
start_web() {
  local mount=()
  if [ "$MODE" = --expect-stale ]; then
    mount=(-v "$ROOT/e2e/compose-web-dns-fixture/nginx-old.conf:/etc/nginx/conf.d/default.conf:ro")
  fi
  docker run -d --name "$WEB" --network "$NET" \
    -p 127.0.0.1::8080 "${mount[@]}" "$WEB_IMAGE" >/dev/null
  CONTAINERS+=("$WEB")
  HOSTPORT="$(docker port "$WEB" 8080/tcp)"
  [ -n "$HOSTPORT" ] || fail "web has no published port"
}
release_scenario() {
  local name
  for name in "$WEB" "$API" "$FILLER"; do
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
  docker network rm "$NET" >/dev/null 2>&1 || true
}

REST_HOST=127.0.0.1
REST_TARGET='/api/encoded%2Fpath?key=a%2Fb&twice=1&twice=2'
WS_TARGET='/api/ws?key=a%2Fb&twice=1'
FORGED_XFF=203.0.113.99
WS_KEY='dGhlIHNh'
WS_KEY="${WS_KEY}bXBsZSBub25jZQ=="
WS_ACCEPT='s3pPLMBiTxaQ9kYGzzhZRbK+xOo='
b64() { printf '%s' "$1" | base64 | tr -d '\n'; }
header_value() {
  awk -F': ' -v wanted="$2" 'tolower($1)==tolower(wanted) {gsub(/\r/, "", $2); print $2; exit}' "$1"
}
request_rest() {
  REST_CODE="$(curl --path-as-is --max-time 4 -sS \
    -o "$SCRATCH/rest.json" -w '%{http_code}' \
    -H "X-Forwarded-For: $FORGED_XFF" \
    "http://$HOSTPORT$REST_TARGET" 2>"$SCRATCH/rest.err")" || return 1
}
check_rest() {
  request_rest || return 1
  [ "$REST_CODE" = 200 ] || return 1
  jq -e --arg target "$REST_TARGET" --arg host "$REST_HOST" \
    --arg forged "$FORGED_XFF" '
      .marker == "api" and .target == $target and
      .headers.host == $host and
      (.headers["x-real-ip"] // "") != "" and
      .headers["x-forwarded-for"] == .headers["x-real-ip"] and
      .headers["x-forwarded-for"] != $forged
    ' "$SCRATCH/rest.json" >/dev/null
}
request_ws() {
  local rc=0
  : > "$SCRATCH/ws.headers"
  curl --http1.1 --max-time 4 -sS \
    -D "$SCRATCH/ws.headers" -o /dev/null \
    -H "Host: $HOSTPORT" -H "Origin: http://$HOSTPORT" \
    -H "X-Forwarded-For: $FORGED_XFF" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $WS_KEY" \
    "http://$HOSTPORT$WS_TARGET" 2>"$SCRATCH/ws.err" || rc=$?
  [ "$rc" -eq 0 ] || [ "$rc" -eq 52 ]
}
check_ws() {
  request_ws || return 1
  grep -q '^HTTP/1.1 101 ' "$SCRATCH/ws.headers" || return 1
  [ "$(header_value "$SCRATCH/ws.headers" Sec-WebSocket-Accept)" = "$WS_ACCEPT" ] || return 1
  [ "$(header_value "$SCRATCH/ws.headers" X-Echo-Marker)" = api ] || return 1
  [ "$(header_value "$SCRATCH/ws.headers" X-Echo-Target-B64)" = "$(b64 "$WS_TARGET")" ] || return 1
  [ "$(header_value "$SCRATCH/ws.headers" X-Echo-Host-B64)" = "$(b64 "$HOSTPORT")" ] || return 1
  [ "$(header_value "$SCRATCH/ws.headers" X-Echo-Origin-B64)" = "$(b64 "http://$HOSTPORT")" ] || return 1
  local xff real_ip
  xff="$(header_value "$SCRATCH/ws.headers" X-Echo-Xff-B64 | base64 -d 2>/dev/null)" || return 1
  real_ip="$(header_value "$SCRATCH/ws.headers" X-Echo-Real-IP-B64 | base64 -d 2>/dev/null)" || return 1
  [ -n "$xff" ] && [ "$xff" = "$real_ip" ] && [ "$xff" != "$FORGED_XFF" ]
}
identity() {
  docker inspect -f '{{.Id}} {{.State.StartedAt}}' "$WEB"
  docker exec "$WEB" sh -c \
    "ps -o pid,args | awk '/nginx: (master|worker) process/ && !/awk/ {print \$1 \":\" \$0}' | sort"
}
record_identity() {
  BEFORE_IDENTITY="$(identity)"
  printf '%s\n' "$BEFORE_IDENTITY" | grep -q 'master process' || fail "nginx master PID absent"
  printf '%s\n' "$BEFORE_IDENTITY" | grep -q 'worker process' || fail "nginx worker PID absent"
}
assert_identity() {
  [ "$(identity)" = "$BEFORE_IDENTITY" ] ||
    fail "web container ID, StartedAt, or nginx master/worker PIDs changed"
}
poll_recovery() {
  local deadline=$((SECONDS + 25))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if check_rest && check_ws; then assert_identity; return 0; fi
    sleep 1
  done
  return 1
}
api_ip() {
  docker inspect -f "{{with index .NetworkSettings.Networks \"$NET\"}}{{.IPAddress}}{{end}}" "$API"
}

if [ "$MODE" = --green ]; then
  new_network startup
  start_web
  record_identity
  request_rest || fail "startup request timed out or failed"
  [ "$REST_CODE" = 502 ] || fail "web-before-api should return temporary 502 (got $REST_CODE)"
  start_api
  poll_recovery || fail "web-before-api did not recover within 25s"
  assert_identity
  note "web-before-api recovered without a web restart"
  release_scenario
fi

new_network swap
start_api
start_filler
start_web
record_identity
poll_recovery || fail "could not prime both locations before swap"
OLD_IP="$(api_ip)"
[ -n "$OLD_IP" ] || fail "stand-in has no initial IP"
docker stop "$API" >/dev/null
docker restart "$FILLER" >/dev/null
docker start "$API" >/dev/null
NEW_IP="$(api_ip)"
API_STARTED="$(docker inspect -f '{{.State.StartedAt}}' "$API")"
[ -n "$NEW_IP" ] && [ "$OLD_IP" != "$NEW_IP" ] ||
  fail "api address did not change ($OLD_IP -> $NEW_IP); no stale-address proof"
assert_identity
# Prove the replacement is serving at its NEW address before a 502 can count as
# stale proxy routing. Run the probe from the already-owned filler container.
api_probe() {
  [ "$(docker inspect -f '{{.State.Running}} {{.State.StartedAt}}' "$API")" = "true $API_STARTED" ] || return 1
  docker exec -e TARGET_IP="$NEW_IP" "$FILLER" node -e '
    fetch(`http://${process.env.TARGET_IP}:8080/api/health`, {
      signal: AbortSignal.timeout(4000),
    }).then(r => r.json()).then(body => {
      if (body.marker !== "api") process.exit(1);
    }).catch(() => process.exit(1));
  ' >/dev/null 2>&1
}
api_live=0
live_deadline=$((SECONDS + 10))
while [ "$SECONDS" -lt "$live_deadline" ]; do
  if api_probe; then api_live=1; break; fi
  sleep 1
done
[ "$api_live" = 1 ] || fail "replacement api is not healthy at $NEW_IP"

if [ "$MODE" = --expect-stale ]; then
  sleep 6
  api_probe || fail "replacement api died before stale REST probe"
  request_rest || fail "red control request timed out in the harness"
  api_probe || fail "replacement api died during stale REST probe"
  case "$REST_CODE" in
    502) ;;
    200) jq -e '.marker == "filler"' "$SCRATCH/rest.json" >/dev/null ||
      fail "red control reached an unexpected upstream" ;;
    *) fail "red control received unexpected HTTP $REST_CODE" ;;
  esac
  api_probe || fail "replacement api died before stale WebSocket probe"
  request_ws || fail "red control WebSocket request timed out in the harness"
  api_probe || fail "replacement api died during stale WebSocket probe"
  if grep -q '^HTTP/1.1 101 ' "$SCRATCH/ws.headers"; then
    [ "$(header_value "$SCRATCH/ws.headers" X-Echo-Marker)" = filler ] ||
      fail "red control WebSocket reached unexpected upstream"
  else
    grep -q '^HTTP/1.1 502 ' "$SCRATCH/ws.headers" ||
      fail "red control WebSocket got neither stale filler nor 502"
  fi
  assert_identity
  note "EXPECTED RED: primed old config kept the stale api address after asserted swap"
else
  poll_recovery || fail "REST or WebSocket did not recover after address swap within 25s"
  assert_identity
  note "GREEN: REST and WebSocket recovered at $NEW_IP without nginx reload"
fi
release_scenario
