#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PHASE="$ROOT/e2e/phases/52-api-outage-outbox.sh"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
fixture() {
  # shellcheck disable=SC2034  # evaluated real helpers consume these variables
  RUNROOT="$SCRATCH" COMPOSE=(fake_compose) OUTBOX_BASE_COMPOSE=(fake_compose) OUTBOX_API_PINNED=0 RUN_B1=fixture
  API_AVAILABLE=1 API_PAUSED=0
  fake_compose() {
    while [ "${1:-}" = -f ]; do shift 2; done
    printf '%s\n' "$*" >> "$SCRATCH/calls"
    case "$1" in
      ps) echo fixture-api ;;
      stop) API_AVAILABLE=0 ;;
      pause) API_PAUSED=1; API_AVAILABLE=0 ;;
      up) API_AVAILABLE=1; API_PAUSED=0 ;;
      logs) echo fixture-evidence ;;
    esac
  }
  docker() {
    if [ "$1" = inspect ]; then echo '[{"NetworkSettings":{"Networks":{"fixture_default":{}}}}]'
    else echo '[{"IPAM":{"Config":[{"Subnet":"172.30.0.0/16"}]}}]'; fi
  }
  fail() { echo "$*" >&2; exit 1; }
  wait_http() { [ "$API_AVAILABLE" = 1 ]; }
  login() { :; }
  wait_worker_online() { :; }
  set_interlock() { :; }
  wait_terminal_lost() {
    [ "$API_AVAILABLE:$API_PAUSED" = 0:0 ] || fail 'outage must refuse connections rather than hang a paused API'
    jq -e '.services.agent.extra_hosts == ["api:172.30.255.254"] and .services.api.networks.default.ipv4_address == "172.30.255.254" and (.services.web == null)' "$OUTBOX_IP_OVERRIDE" >/dev/null \
      || fail 'API pinning must cover only the agent and keep a fixed API address'
  }
  eval "$(awk '/^(pin_outbox_api_resolution|api_cut|api_back|restore_outbox_resolution|restore_outage_state)\(\) \{/,/^\}/' "$PHASE")"
}
fixture
pin_outbox_api_resolution
eval "$(awk '
  /^say "CASE 3:/ { inside=1; next }
  /^say "CASE 4:/ { inside=0 }
  inside && (/^api_cut$/ || /^terminal_lost_b1=/ || /^api_back$/) { print }
' "$PHASE")"
[ "$API_AVAILABLE:$API_PAUSED" = 1:0 ]
echo 'PASS: real CASE 3 stops API with stable agent-only hosts resolution'
restore_outbox_resolution
[ "${#COMPOSE[@]}:$OUTBOX_API_PINNED" = 1:0 ]
[ -s "$SCRATCH/artifacts/52-api-outage-outbox/agent-before-address-restore.log" ]
echo 'PASS: restore removes pinning and preserves worker evidence before recreation'
: > "$SCRATCH/calls"
if ( fixture; pin_outbox_api_resolution; eval "$(awk '/^trap .*restore_outage_state/ { print }' "$PHASE")"; api_cut; exit 7 ); then
  echo 'FAIL: failure status lost'; exit 1
else [ "$?" = 7 ]; fi
awk 'index($0,"force-recreate api") { api=1 } index($0,"force-recreate agent") { agent=1 } END { exit(api && agent ? 0 : 1) }' "$SCRATCH/calls"
echo 'PASS: failure trap restores original API and agent configuration'
if ( fixture; docker() {
  if [ "$1" = inspect ]; then echo '[{"NetworkSettings":{"Networks":{"fixture_default":{}}}}]'
  else echo '[{"IPAM":{"Config":[{"Subnet":"172.30.0.0/30"}]}}]'; fi
}; pin_outbox_api_resolution ); then
  echo 'FAIL: unsupported network topology accepted'; exit 1
fi
echo 'PASS: unsupported network topology fails closed'
echo 'cases=4 passed=4'
