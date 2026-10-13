#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PHASE="$ROOT/e2e/phases/52-api-outage-outbox.sh"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

fixture() {
  # shellcheck disable=SC2034  # consumed by the evaluated real phase helpers
  COMPOSE=(fake_compose)
  # shellcheck disable=SC2034  # consumed by the evaluated real CASE 3 and cleanup
  API_DNS_LIVE=1 API_AVAILABLE=1 API_PAUSED=0 OUTBOX_API_PAUSED=0 OUTCOME=pending RUN_B1=fixture
  fake_compose() {
    printf '%s\n' "$1" >> "$SCRATCH/calls"
    case "$1" in
      stop) API_DNS_LIVE=0; API_AVAILABLE=0 ;;
      pause) API_PAUSED=1; API_AVAILABLE=0 ;;
      unpause) API_PAUSED=0; API_AVAILABLE=1 ;;
      up) [ "$API_PAUSED" = 0 ] || return 1; API_AVAILABLE=1; API_DNS_LIVE=1 ;;
    esac
  }
  wait_http() { [ "$API_AVAILABLE" = 1 ]; }
  login() { :; }
  set_interlock() { :; }
  wait_terminal_lost() {
    [ "$API_AVAILABLE" = 0 ] || return 1
    # Slow unresolved API lookups occupy Node's DNS workers. Forge finalization
    # succeeds only when this HTTP outage preserves the API's DNS alias.
    if [ "$API_DNS_LIVE" = 1 ]; then OUTCOME=completed; else OUTCOME=failed; fi
  }
  eval "$(awk '/^(api_cut|api_back|restore_outage_state)\(\) \{/,/^\}/' "$PHASE")"
}

fixture
# Drive the actual CASE 3 cut, terminal-loss gate and restoration, including the
# original stop command when this regression is run against the unfixed phase.
eval "$(awk '
  /^say "CASE 3:/ { inside=1; next }
  /^say "CASE 4:/ { inside=0 }
  inside && (/^"\$\{COMPOSE\[@\]\}" stop api/ || /^api_cut$/ || /^terminal_lost_b1=/ || /^api_back$/) { print }
' "$PHASE")"
[ "$OUTCOME" = completed ] || { echo 'FAIL: API outage also broke healthy forge publication'; exit 1; }
[ "$API_AVAILABLE:$API_PAUSED" = 1:0 ] || { echo 'FAIL: CASE 3 did not restore API availability'; exit 1; }
echo 'PASS: CASE 3 isolates HTTP outage from forge DNS'

: > "$SCRATCH/calls"
if (
  fixture
  eval "$(awk '/^trap .*restore_outage_state/ { print }' "$PHASE")"
  api_cut
  exit 7
); then
  echo 'FAIL: fixture failure was lost'; exit 1
else
  rc=$?
  [ "$rc" = 7 ] || { echo "FAIL: fixture exit changed to $rc"; exit 1; }
fi
awk '$0=="unpause" { found=1 } END { exit(found ? 0 : 1) }' "$SCRATCH/calls" \
  || { echo 'FAIL: failed phase left the API paused'; exit 1; }
echo 'PASS: failure trap unpauses API and preserves failure status'
echo 'cases=2 passed=2'
