#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PHASE="${OUTBOX_TEST_PHASE:-$ROOT/e2e/phases/52-api-outage-outbox.sh}"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
fixture() {
  # shellcheck disable=SC2034  # evaluated real helpers consume these variables
  RUNROOT="$SCRATCH" PROJECT=fixture COMPOSE=(fake_compose) OUTBOX_BASE_COMPOSE=(fake_compose) OUTBOX_RESTORE_NEEDED=0 RUN_B1=fixture
  API_AVAILABLE=1 API_PAUSED=0 ALIAS_AVAILABLE=0 REFUSE_API=0 REFUSE_ALIAS=0 REFUSE_REMOVE=0
  fake_compose() {
    while [ "${1:-}" = -f ] || [ "${1:-}" = --profile ]; do shift 2; done
    printf '%s\n' "$*" >> "$SCRATCH/calls"
    case "$1" in
      images) echo fixture-worker-image ;;
      stop) [ "$ALIAS_AVAILABLE" = 1 ] || return 1; API_AVAILABLE=0 ;;
      pause) API_PAUSED=1; API_AVAILABLE=0 ;;
      up)
        case "${!#}" in
          outbox-api-placeholder) [ "$REFUSE_ALIAS" = 0 ] || return 1; ALIAS_AVAILABLE=1 ;;
          api) [ "$REFUSE_API" = 0 ] || return 1; API_AVAILABLE=1; API_PAUSED=0 ;;
        esac ;;
      rm) [ "$API_AVAILABLE" = 1 ] || fail 'alias removal creates a DNS gap';
        [ "$REFUSE_REMOVE" = 0 ] || return 1; ALIAS_AVAILABLE=0 ;;
      logs) echo fixture-evidence ;;
    esac
  }
  fail() { echo "$*" >&2; exit 1; }
  wait_http() { [ "$API_AVAILABLE:$ALIAS_AVAILABLE" = 1:0 ]; }
  login() { :; }
  wait_worker_online() { :; }
  set_interlock() { :; }
  wait_terminal_lost() {
    [ "$API_AVAILABLE:$API_PAUSED:$ALIAS_AVAILABLE" = 0:0:1 ] || fail 'outage must stop API while preserving its DNS alias'
    jq -e '.services | keys == ["outbox-api-placeholder"]' "$OUTBOX_DNS_OVERRIDE" >/dev/null \
      || fail 'placeholder must not change API, worker or network configuration'
    jq -e '.services["outbox-api-placeholder"] |
      .image == "fixture-worker-image" and .pull_policy == "never" and
      .profiles == ["outbox-api-cut"] and .networks == {default:{aliases:["api"]}} and
      (.container_name | startswith("cdr-")) and .entrypoint[0] == "/usr/local/bin/node" and
      .command == [] and .user == "65534:65534" and .read_only == true and
      .cap_drop == ["ALL"] and .environment == null and .volumes == null and .secrets == null' "$OUTBOX_DNS_OVERRIDE" >/dev/null \
      || fail 'placeholder must be idle, phase-scoped and authority-free'
  }
  eval "$(awk '/^(configure_outbox_api_alias|start_outbox_api_alias|remove_outbox_api_alias|recreate_outbox_api|recreate_outbox_agent|api_cut|api_back|restore_outbox_resolution|restore_outage_state)\(\) \{/,/^\}/' "$PHASE")"
}
fixture
configure_outbox_api_alias
eval "$(awk '
  /^say "CASE 3:/ { inside=1; next }
  /^say "CASE 4:/ { inside=0 }
  inside && (/^api_cut$/ || /^terminal_lost_b1=/ || /^api_back$/) { print }
' "$PHASE")"
[ "$API_AVAILABLE:$API_PAUSED:$ALIAS_AVAILABLE" = 1:0:0 ]
echo 'PASS: real CASE 3 keeps API alias through stop, removes placeholder after health'
restore_outbox_resolution
[ "${#COMPOSE[@]}:$OUTBOX_RESTORE_NEEDED:$ALIAS_AVAILABLE" = 1:0:0 ]
[ -s "$SCRATCH/artifacts/52-api-outage-outbox/agent-before-api-restore.log" ]
echo 'PASS: restore removes placeholder and preserves worker evidence before recreation'
: > "$SCRATCH/calls"
if ( fixture; configure_outbox_api_alias; eval "$(awk '/^trap .*restore_outage_state/ { print }' "$PHASE")"; api_cut; exit 7 ); then
  echo 'FAIL: failure status lost'; exit 1
else [ "$?" = 7 ]; fi
awk 'index($0,"force-recreate api") { api=1 } index($0,"force-recreate agent") { agent=1 }
  index($0,"rm -s -f outbox-api-placeholder") { removed=1 } END { exit(api && agent && removed ? 0 : 1) }' "$SCRATCH/calls"
echo 'PASS: failure trap restores defaults and removes the placeholder'
fixture
configure_outbox_api_alias
restore_outbox_resolution
[ "$OUTBOX_RESTORE_NEEDED:$ALIAS_AVAILABLE" = 0:0 ]
echo 'PASS: setup is restored even before the first cut'
if ( fixture; configure_outbox_api_alias; REFUSE_API=1; recreate_outbox_api ) > "$SCRATCH/endpoint.log" 2>&1; then
  echo 'FAIL: refused API recreation accepted'; exit 1
fi
grep -F 'initial API recreation failed' "$SCRATCH/endpoint.log" >/dev/null
echo 'PASS: Docker endpoint refusal produces a named failure'
if ( fixture; configure_outbox_api_alias; REFUSE_ALIAS=1; api_cut ) > "$SCRATCH/alias.log" 2>&1; then
  echo 'FAIL: missing placeholder accepted'; exit 1
fi
grep -F 'API alias placeholder startup failed' "$SCRATCH/alias.log" >/dev/null
echo 'PASS: placeholder startup failure is named before stopping API'
if ( fixture; configure_outbox_api_alias; api_cut; REFUSE_REMOVE=1; api_back ) > "$SCRATCH/remove.log" 2>&1; then
  echo 'FAIL: failed placeholder removal accepted'; exit 1
fi
grep -F 'API alias placeholder removal failed' "$SCRATCH/remove.log" >/dev/null
echo 'PASS: placeholder removal failure is named after API recovery'
# shellcheck disable=SC2329  # evaluated helper invokes the injected command
if ( fixture; fake_compose() { return 0; }; configure_outbox_api_alias ) > "$SCRATCH/image.log" 2>&1; then
  echo 'FAIL: missing worker image accepted'; exit 1
fi
grep -F 'built worker image missing for API placeholder' "$SCRATCH/image.log" >/dev/null
echo 'PASS: missing built worker image fails before mutation'
echo 'cases=8 passed=8'
