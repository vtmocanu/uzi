#!/usr/bin/env bash
# Standalone regression for nginx resolving the Compose api service after replacement.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROJECT="cdr-dns-$$"
SCRATCH="$(mktemp -d)"
COMPOSE_FILE="$ROOT/e2e/compose-dns/compose.yml"

compose() {
  env -i HOME="$HOME" PATH="$PATH" DOCKER_HOST="${DOCKER_HOST:-}" \
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

cleanup() {
  rc=$?
  trap - EXIT
  if [ "$rc" -ne 0 ]; then
    compose logs web api occupant >&2 || true
  fi
  compose --profile occupant down --remove-orphans >&2 || true
  rm -rf -- "$SCRATCH"
  exit "$rc"
}
trap cleanup EXIT

fail() { echo "compose DNS regression: $*" >&2; exit 1; }
container_ip() {
  docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"
}
wait_health() {
  local expected="$1" body
  for ((attempt=0; attempt<30; attempt++)); do
    body="$(curl -fsS --max-time 2 "$BASE/api/health" 2>/dev/null || true)"
    if [ "$body" = "$expected" ]; then return 0; fi
    sleep 1
  done
  fail "REST /api/health did not reach api $expected (last response: $body)"
}
check_ws() {
  local expected="$1" status identity
  local ws_key
  ws_key="$(printf 'dns-regression-1' | base64)"
  curl -s --max-time 5 -D "$SCRATCH/ws.headers" -o /dev/null \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $ws_key" \
    "$BASE/api/ws" || true
  status="$(awk 'NR == 1 {print $2}' "$SCRATCH/ws.headers")"
  identity="$(awk 'tolower($1) == "x-api-identity:" {gsub(/\r/, "", $2); print $2}' "$SCRATCH/ws.headers")"
  [ "$status" = 101 ] && [ "$identity" = "$expected" ] ||
    fail "WebSocket reached wrong api: status=$status identity=$identity expected=$expected"
}

compose up -d --wait api web
WEB_ID="$(compose ps -q web)"
OLD_ID="$(compose ps -q api)"
OLD_IP="$(container_ip "$OLD_ID")"
PORT="$(compose port web 8080)"
BASE="http://$PORT"
wait_health "${OLD_ID:0:12}"
check_ws "${OLD_ID:0:12}"

# Occupy the released address before starting the replacement api. This makes
# a startup-only nginx DNS lookup observable even if Docker would reuse the IP.
compose stop api
compose rm -f api
compose --profile occupant up -d --no-deps occupant
OCCUPANT_ID="$(compose ps -q occupant)"
OCCUPANT_IP="$(container_ip "$OCCUPANT_ID")"
[ "$OCCUPANT_IP" = "$OLD_IP" ] ||
  fail "address churn fixture failed: occupant=$OCCUPANT_IP former api=$OLD_IP"
compose up -d --no-deps api
NEW_ID="$(compose ps -q api)"
NEW_IP="$(container_ip "$NEW_ID")"
[ "$NEW_ID" != "$OLD_ID" ] && [ "$NEW_IP" != "$OLD_IP" ] ||
  fail "api was not replaced at a new address"
[ "$(compose ps -q web)" = "$WEB_ID" ] || fail "web restarted during api replacement"

wait_health "${NEW_ID:0:12}"
check_ws "${NEW_ID:0:12}"
echo "PASS: unchanged web resolved replacement api ($OLD_IP -> $NEW_IP) for REST and WebSocket"
