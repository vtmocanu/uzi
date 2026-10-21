#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PHASE="${OUTBOX_TEST_PHASE:-$ROOT/e2e/phases/52-api-outage-outbox.sh}"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
fixture() {
  # shellcheck disable=SC2034  # evaluated real helpers consume these variables
  RUNROOT="$SCRATCH" PROJECT=fixture COMPOSE=(fake_compose) OUTBOX_BASE_COMPOSE=(fake_compose) OUTBOX_RESTORE_NEEDED=0 RUN_B1=00000000-0000-0000-0000-000000000001
  API_AVAILABLE=1 API_PAUSED=0 ALIAS_AVAILABLE=0 REFUSE_API=0 REFUSE_ALIAS=0 REFUSE_REMOVE=0
  BARRIER_ARMED=0 BARRIER_RELEASED=0 FIXTURE_FINISHED=0 SLOW_HELPER=0
  RESTORING=0 PROJECTION_FAIL=0
  # Host limiter fixture: records the bound it was given, then runs the command.
  timeout() { printf 'limit %s\n' "$1" >> "$SCRATCH/calls"; shift; "$@"; }
  export UZI_E2E_RECOVERY_FAST_REDRIVE=1
  storage_diagnostic_snapshot() {
    case "$1" in
      outbox-before-restore) RESTORING=1 ;;
      outbox-after-restore) RESTORING=0 ;;
      outbox-entry) : ;;
      *) fail 'unexpected storage diagnostic boundary' ;;
    esac
    printf '%s\n' "$1" >> "$SCRATCH/calls"
  }
  fake_compose() {
    while [ "${1:-}" = -f ] || [ "${1:-}" = --profile ]; do shift 2; done
    printf '%s\n' "$*" >> "$SCRATCH/calls"
    case "$1" in
      images) echo fixture-worker-image ;;
      stop) [ "$ALIAS_AVAILABLE" = 1 ] || return 1; API_AVAILABLE=0 ;;
      pause) API_PAUSED=1; API_AVAILABLE=0 ;;
      up)
        case "${!#}" in
          outbox-api-placeholder) [ "$REFUSE_ALIAS" = 0 ] || return 1;
            if [ "$SLOW_HELPER:$BARRIER_ARMED" = 1:0 ]; then FIXTURE_FINISHED=1; fi
            ALIAS_AVAILABLE=1 ;;
          api) [ "$REFUSE_API" = 0 ] || return 1; API_AVAILABLE=1; API_PAUSED=0 ;;
          agent) [ "$RESTORING" != 1 ] || [ -z "${UZI_E2E_RECOVERY_FAST_REDRIVE:-}" ] \
            || fail 'fast redrive must be unset before agent restore' ;;
        esac ;;
      rm) [ "$*" = 'rm -s -f -v outbox-api-placeholder' ] || fail 'placeholder removal must remove its anonymous volumes';
        [ "$API_AVAILABLE" = 1 ] || fail 'alias removal creates a DNS gap';
        [ "$REFUSE_REMOVE" = 0 ] || return 1; ALIAS_AVAILABLE=0 ;;
      logs) echo fixture-evidence ;;
      restart) [ "$BARRIER_RELEASED:$FIXTURE_FINISHED" = 1:0 ] || fail 'restart preceded the guarded terminal outage' ;;
      exec)
        # Evidence projection: run the REAL script on the host against the scratch data root,
        # standing in for the agent container's /data. Matched before the barrier-writer checks.
        if [ "${2:-}:${3:-}:${4:-}:${5:-}:${6:-}:${7:-}:${8:-}" = -T:-u:0:agent:timeout:15:node ] \
          && [ "${11:-}" = /data ]; then
          [ "$PROJECTION_FAIL" = 0 ] || return 1
          node --input-type=module - "$SCRATCH/data" "${@:12}"
          return
        fi
        [ "$API_AVAILABLE" = 0 ] || fail 'barrier released before API stop';
        if [ "${3:-}:${4:-}" = agent:test ]; then
          [ "$BARRIER_RELEASED" = 1 ] || fail 'finalize file checked before barrier release'
          return 0
        fi
        [ "${3:-}:${4:-}" = -u:10001:10001 ] || fail 'barrier writer must match worker uid for sticky tmp cleanup'
        BARRIER_RELEASED=1 ;;
    esac
  }
  fail() { echo "$*" >&2; exit 1; }
  pass() { :; }
  wait_http() { [ "$API_AVAILABLE:$ALIAS_AVAILABLE" = 1:0 ]; }
  login() { :; }
  wait_worker_online() { :; }
  set_interlock() { :; }
  make_outbox_run() {
    case "${1:-}" in cut_barrier|first_cut_barrier) BARRIER_ARMED=1 ;; esac
  }
  wait_publication_retrying() { wait_terminal_lost "$@"; }
  wait_terminal_lost() {
    [ "$FIXTURE_FINISHED" = 0 ] || fail 'slow helper let fixture finish before API cut'
    [ "$SLOW_HELPER:$BARRIER_RELEASED" != 1:0 ] || fail 'cut barrier never released'
    [ "$API_AVAILABLE:$API_PAUSED:$ALIAS_AVAILABLE" = 0:0:1 ] || fail 'outage must stop API while preserving its DNS alias'
    jq -e '.services | keys == ["outbox-api-placeholder"]' "$OUTBOX_DNS_OVERRIDE" >/dev/null \
      || fail 'placeholder must not change API, worker or network configuration'
    jq -e '.services["outbox-api-placeholder"] |
      .image == "fixture-worker-image" and .pull_policy == "never" and
      .profiles == ["outbox-api-cut"] and .networks == {default:{aliases:["api"]}} and
      (.container_name | startswith("cdr-")) and .entrypoint[0] == "/usr/local/bin/node" and
      .command == [] and .user == "65534:65534" and .read_only == true and
      .cap_drop == ["ALL"] and .environment == null and .volumes == null and .secrets == null and
      .tmpfs == ["/data:size=1m","/nix:size=1m"]' "$OUTBOX_DNS_OVERRIDE" >/dev/null \
      || fail 'placeholder must be idle, phase-scoped and authority-free'
  }
  eval "$(awk '/^(configure_outbox_api_alias|start_outbox_api_alias|remove_outbox_api_alias|recreate_outbox_api|recreate_outbox_agent|api_cut|release_outbox_cut_barrier|api_back|restore_outbox_resolution|save_outbox_agent_evidence|restore_outage_state|f42_cut)\(\) \{/,/^\}/' "$PHASE")"
}
# Extract the real helper; the old name lets the same assertions demonstrate the
# regression against the pre-change phase rather than failing during setup.
publication_helper="$(awk '/^wait_(publication|permit)_retrying\(\) \{/ { sub(/\(\).*/, ""); print; exit }' "$PHASE")"
[ -n "$publication_helper" ] || { echo 'FAIL: publication helper missing'; exit 1; }
publication_predicate_case() (
  fixture
  eval "$(awk '/^wait_(publication|permit)_retrying\(\) \{/,/^\}/' "$PHASE")"
  # Each miss advances the helper clock; no real-time polling in these fixtures.
  sleep() { SECONDS=$((SECONDS + 1)); }
  fake_compose() { printf '%s\n' "$evidence"; }
  local expected="$1" evidence="$2" rc=0
  "$publication_helper" "$RUN_B1" 2 || rc=$?
  [ "$rc" = "$expected" ] || fail "$3: expected rc=$expected, got $rc"
)
for log_format in json compose compose-spaced; do
  run=00000000-0000-0000-0000-000000000001
  other=00000000-0000-0000-0000-000000000002
  durable="{\"msg\":\"finalize record durable\",\"run_id\":\"$run\"}"
  retry="{\"msg\":\"state report failed, retrying\",\"run_id\":\"$run\",\"status\":\"running\"}"
  terminal="{\"msg\":\"state report failed, retrying\",\"run_id\":\"$run\",\"status\":\"completed\"}"
  wrong_run="{\"msg\":\"state report failed, retrying\",\"run_id\":\"$other\",\"status\":\"running\"}"
  wrong_durable="{\"msg\":\"finalize record durable\",\"run_id\":\"$other\"}"
  permit="{\"msg\":\"completion permit request failed, retrying\",\"run_id\":\"$run\"}"
  if [ "$log_format" != json ]; then
    # Compose prefixes the worker's compact JSON.
    durable="agent-1  | $durable"
    retry="agent-1  | $retry"
    if [ "$log_format" = compose-spaced ]; then
      retry="agent-1  | { \"run_id\" : \"$run\", \"status\" : \"running\", \"msg\" : \"state report failed, retrying\" }"
    fi
    terminal="agent-1  | $terminal"
    wrong_run="agent-1  | $wrong_run"
    wrong_durable="agent-1  | $wrong_durable"
    permit="agent-1  | $permit"
  fi
  publication_predicate_case 0 "$durable
$retry" 'ordered durable + running retry'
  echo "PASS: $log_format publication predicate accepts ordered durable + running retry"
  publication_predicate_case 1 "$durable
$wrong_run" 'other run retry'
  publication_predicate_case 1 "$wrong_durable
$retry" 'other run durable'
  publication_predicate_case 1 "$durable
$terminal" 'terminal status retry'
  publication_predicate_case 1 "$retry
$durable" 'retry before durable'
  publication_predicate_case 1 "$durable
$permit" 'permit-only retry'
  publication_predicate_case 1 "$durable
$terminal
{\"run_id\":\"$run\",\"status\":\"running\"}" 'running status on a different line'
  echo "PASS: $log_format publication predicate rejects other runs, terminal status, reversed order, permit-only and split-line status"
done
fixture
configure_outbox_api_alias
SLOW_HELPER=1
eval "$(awk '
  /^say "CASE 3:/ { inside=1; next }
  /^say "CASE 4:/ { inside=0 }
  inside && (/^make_outbox_run/ || /^api_cut$/ || /^release_outbox_cut_barrier/ || /^terminal_lost_b1=/ || /^api_back$/) { print }
' "$PHASE")"
[ "$API_AVAILABLE:$API_PAUSED:$ALIAS_AVAILABLE" = 1:0:0 ]
echo 'PASS: real CASE 3 keeps API alias through stop, removes placeholder after health'
restore_outbox_resolution
[ "${#COMPOSE[@]}:$OUTBOX_RESTORE_NEEDED:$ALIAS_AVAILABLE" = 1:0:0 ]
[ -s "$SCRATCH/artifacts/52-api-outage-outbox/agent-before-api-restore.log" ]
[ -z "${UZI_E2E_RECOVERY_FAST_REDRIVE:-}" ]
grep -F outbox-before-restore "$SCRATCH/calls" >/dev/null
grep -F outbox-after-restore "$SCRATCH/calls" >/dev/null
echo 'PASS: restore removes placeholder and preserves worker evidence before recreation'
: > "$SCRATCH/calls"
if ( fixture; configure_outbox_api_alias; eval "$(awk '/^trap .*restore_outage_state/ { print }' "$PHASE")"; api_cut; exit 7 ); then
  echo 'FAIL: failure status lost'; exit 1
else [ "$?" = 7 ]; fi
awk 'index($0,"force-recreate api") { api=1 } index($0,"force-recreate agent") { agent=1 }
  index($0,"rm -s -f -v outbox-api-placeholder") { removed=1 } END { exit(api && agent && removed ? 0 : 1) }' "$SCRATCH/calls"
echo 'PASS: failure trap restores defaults and removes the placeholder'
# Cleanup evidence: a failing fixture receipt wait must still leave both agent logs and both
# allowlisted recovery-record projections, captured before the agent is recreated.
phase_run=00000000-0000-0000-0000-000000000001
foreign_run=00000000-0000-0000-0000-000000000099
capture_id=22222222-2222-4222-8222-222222222222
rm -rf "$SCRATCH/artifacts" "$SCRATCH/data"
: > "$SCRATCH/calls"
mkdir -p "$SCRATCH/data/recovery/$phase_run" "$SCRATCH/data/recovery/$foreign_run"
cat > "$SCRATCH/data/recovery/$phase_run/$capture_id.json" <<JSON
{"generation":4,"state":"uploaded","reason":"final_retry_refused","inventoryGuarded":true,
 "finalRequest":{"body":"SECRET_FINAL_BODY"},"mac":"SECRET_MAC_VALUE","bundlePath":"/data/SECRET_BUNDLE_PATH",
 "sourceSha":"$(printf 'e%.0s' $(seq 40))"}
JSON
echo '{"generation":77,"state":"pinned","reason":"foreign_reason"}' > "$SCRATCH/data/recovery/$foreign_run/$capture_id.json"
rc=0
(
  fixture
  configure_outbox_api_alias
  eval "$(awk '/^trap .*restore_outage_state/ { print }' "$PHASE")"
  eval "$(awk '/^resolve_fixture_source_hold\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
  # shellcheck disable=SC2034  # read by the extracted real helper
  ADMIN_EMAIL=fixture@example.com
  hold_id=b4dec5cf-e6d4-413b-b3a8-0005a2835770
  sleep() { SECONDS=$((SECONDS + 1)); }
  apiget() {
    case "$1" in
      /api/recovery/holds) jq -nc --arg run "$RUN_B1" --arg id "$hold_id" \
        '{holds:[{id:$id,run_id:$run,generation:1,state:"open",inventory_guarded:true,attention:"source_only",
          has_available_capture:true,capture_state:"available"}]}' ;;
      "/api/runs/$RUN_B1/archives") echo '{"archives":[{"id":"kept","state":"available","checksum":"stable","byte_size":12}]}' ;;
      "/api/runs/$RUN_B1") jq -nc --arg run "$RUN_B1" '{run:{id:$run,status:"cancelled"}}' ;;
      *) fail 'unexpected fixture receipt route' ;;
    esac
  }
  db_psql() {
    jq -nc --arg run "$RUN_B1" --arg id "$hold_id" \
      '{run_id:$run,run_status:"cancelled",holds:[{id:$id,run_id:$run,generation:1,inventory_guarded:true,
        state:"open",final_disposition:null,release_evidence:null,captures:[{state:"available"}]}]}'
  }
  api_cut
  resolve_fixture_source_hold "$RUN_B1" 1 1
) > "$SCRATCH/evidence.log" 2>&1 || rc=$?
[ "$rc" != 0 ] || { echo 'FAIL: receipt timeout did not fail the subshell'; exit 1; }
grep -F 'fixture archive custody receipt timeout' "$SCRATCH/evidence.log" >/dev/null
art="$SCRATCH/artifacts/52-api-outage-outbox"
for f in agent-before-api-restore.log agent-before-agent-recreate.log \
  recovery-records-before-api-restore.jsonl recovery-records-before-agent-recreate.jsonl; do
  [ -s "$art/$f" ] || { echo "FAIL: missing cleanup evidence $f"; exit 1; }
done
for label in before-api-restore before-agent-recreate; do
  jq -se --arg run "$phase_run" --arg cap "$capture_id" \
    'any(.[]; .run_id==$run and .capture_id==$cap and .state=="uploaded" and .reason=="final_retry_refused"
      and .generation==4 and .final_request_present==true and .authenticated==false)' \
    "$art/recovery-records-$label.jsonl" >/dev/null || { echo "FAIL: projection $label lacks the phase run record"; exit 1; }
  if grep -E "$foreign_run|foreign_reason|SECRET_|eeeeeeee|/data/" "$art/recovery-records-$label.jsonl" >/dev/null; then
    echo "FAIL: projection $label leaked foreign or secret data"; exit 1
  fi
done
awk 'index($0, "exec -T -u 0 agent timeout 15 node --input-type=module - /data") { n++; if (!agent) before++; if (prev == "limit 20") bounded++ }
  index($0, "force-recreate agent") { agent=1 } { prev=$0 }
  END { exit(n == 2 && before == 2 && bounded == 2 && agent ? 0 : 1) }' "$SCRATCH/calls" \
  || { echo 'FAIL: projection did not run twice before agent recreation'; exit 1; }
echo 'PASS: failing receipt wait still saves agent logs and allowlisted recovery projections before agent recreation'
# A failing projection exec is a warning only: cleanup must still recreate api and agent, and
# save_outbox_agent_evidence itself must return 0 (the phase runs under errexit).
: > "$SCRATCH/calls"
(
  fixture
  configure_outbox_api_alias
  PROJECTION_FAIL=1
  save_outbox_agent_evidence probe || { echo 'FAIL: evidence returned nonzero on failed projection'; exit 1; }
  OUTBOX_RESTORE_NEEDED=1
  restore_outbox_resolution
) > "$SCRATCH/projfail.log" 2>&1 || { echo 'FAIL: failed projection aborted cleanup'; cat "$SCRATCH/projfail.log"; exit 1; }
grep -F 'recovery record projection failed (before-api-restore)' "$SCRATCH/projfail.log" >/dev/null \
  || { echo 'FAIL: missing projection failure warning'; exit 1; }
grep -F 'force-recreate api' "$SCRATCH/calls" >/dev/null && grep -F 'force-recreate agent' "$SCRATCH/calls" >/dev/null \
  || { echo 'FAIL: cleanup did not recreate api and agent after projection failure'; exit 1; }
echo 'PASS: failed projection warns, evidence returns 0 and cleanup still recreates api and agent'
# No host timeout/gtimeout: the projection is skipped (never run unbounded) and evidence returns 0.
: > "$SCRATCH/calls"
nohost_bin="$SCRATCH/nohost-bin"
mkdir -p "$nohost_bin" "$SCRATCH/artifacts/52-api-outage-outbox"
ln -s "$(command -v mkdir)" "$nohost_bin/mkdir"
(
  fixture
  unset -f timeout
  PATH="$nohost_bin"
  save_outbox_agent_evidence probe || { echo 'FAIL: evidence returned nonzero without host timeout'; exit 1; }
) > "$SCRATCH/nohost.log" 2>&1 || { echo 'FAIL: missing host timeout aborted evidence'; cat "$SCRATCH/nohost.log"; exit 1; }
grep -F 'recovery record projection skipped, no timeout command (probe)' "$SCRATCH/nohost.log" >/dev/null \
  || { echo 'FAIL: missing no_host_timeout warning'; exit 1; }
grep -F no_host_timeout "$SCRATCH/artifacts/52-api-outage-outbox/recovery-records-probe.jsonl" >/dev/null \
  || { echo 'FAIL: missing no_host_timeout marker'; exit 1; }
! grep -F 'exec -T -u 0 agent' "$SCRATCH/calls" >/dev/null || { echo 'FAIL: projection ran without a host limiter'; exit 1; }
echo 'PASS: absent host timeout skips the projection with a marker and returns 0'
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
for case_number in 4 5 6; do
  (
    fixture
    configure_outbox_api_alias
    # shellcheck disable=SC2034  # evaluated real case commands consume these ids
    RUN_B2="$RUN_B1" RUN_B3="$RUN_B1" RUN_B4="$RUN_B1" SLOW_HELPER=1
    eval "$(awk -v selected="$case_number" '
      /^say "CASE [0-9]+:/ { inside = index($0, "CASE " selected ":") > 0 }
      inside && (/^make_outbox_run/ || /^api_cut$/ || /^release_outbox_cut_barrier/ ||
        /^terminal_lost_b[234]=/ || /^publication_retrying_b4=/ || /^api_back$/ ||
        index($0, "\"${COMPOSE[@]}\" restart agent") == 1) { print }
    ' "$PHASE")"
    [ "$API_AVAILABLE:$ALIAS_AVAILABLE:$BARRIER_ARMED:$BARRIER_RELEASED:$FIXTURE_FINISHED" = 1:0:1:1:0 ] \
      || fail "case $case_number did not enforce the cut barrier"
  )
  echo "PASS: real CASE $case_number cannot finish during slow helper startup before the cut"
done
# Exercise the real CASE 6 timeout/judgement sequence with the real helper.
rc=0
(
  fixture
  configure_outbox_api_alias
  # shellcheck disable=SC2034  # evaluated real CASE 6 commands consume this id
  RUN_B4="$RUN_B1"
  make_outbox_run cut_barrier
  eval "$(awk '/^wait_publication_retrying\(\) \{/,/^\}/' "$PHASE")"
  sleep() { SECONDS=$((SECONDS + 1)); }
  fail() {
    [ "$API_AVAILABLE:$ALIAS_AVAILABLE" = 1:0 ] || exit 2
    echo "API restored before failure: $*" >&2
    exit 9
  }
  eval "$(awk '
    /^say "CASE 6:/ { inside=1 }
    inside && /^api_cut$/ { commands=1 }
    inside && /^wait_status/ { exit }
    commands { print }
  ' "$PHASE")"
) > "$SCRATCH/publication-timeout.log" 2>&1 || rc=$?
[ "$rc" = 9 ] || { echo "FAIL: CASE 6 timeout expected restored failure rc=9, got $rc"; exit 1; }
grep -F 'API restored before failure: case 6:' "$SCRATCH/publication-timeout.log" >/dev/null
grep -F 'never retried its publication-running ACK after durable finalize' "$SCRATCH/publication-timeout.log" >/dev/null
echo 'PASS: real CASE 6 publication timeout restores API before assertion failure'
for case_number in 7 8; do
  (
    fixture
    configure_outbox_api_alias
    SLOW_HELPER=1
    eval "$(awk -v selected="$case_number" '
      /^say "CASE [0-9]+:/ { inside = index($0, "CASE " selected ":") > 0 }
      inside && /^make_outbox_run/ { print }
    ' "$PHASE")"
    checks=0
    f42_durable_logged() {
      checks=$((checks + 1))
      [ "$FIXTURE_FINISHED" = 0 ] || return 0
      if [ "$checks" -gt 1 ] && [ "$BARRIER_RELEASED" != 1 ]; then
        fail 'durable finalize proof attempted without releasing the cut barrier'
      fi
      [ "$BARRIER_RELEASED" = 1 ]
    }
    f42_cut "$RUN_B1" 1 || fail "case $case_number: finalize handed off before the API cut"
    [ "$API_AVAILABLE:$BARRIER_ARMED:$BARRIER_RELEASED:$FIXTURE_FINISHED" = 0:1:1:0 ]
    [ "$checks" = 2 ] || fail 'must check pre-existing durable proof before releasing the barrier'
    api_back
    if [ "$case_number" = 8 ]; then
      # A new execution consumes its own release, after the second cut too.
      BARRIER_RELEASED=0 checks=0
      f42_cut "$RUN_B1" 2 || fail 'case 8: second finalize handed off before the API cut'
      [ "$API_AVAILABLE:$BARRIER_RELEASED:$FIXTURE_FINISHED:$checks" = 0:1:0:2 ]
      api_back
    fi
  )
  echo "PASS: real CASE $case_number orders finalize after the cut despite slow helper startup"
done
(
  fixture
  # Exercise the real issue producer too, so an incorrect token cannot hide
  # behind the mode-aware fake used for the cut-order controls.
  # shellcheck disable=SC2034  # extracted helper consumes the fixture repo id
  REPO_ID=fixture
  apipost() {
    if [[ "$1" == */issues ]]; then
      printf '%s\n' "$2" > "$SCRATCH/issue.json"
      echo '{"card":{"iid":7}}'
    else echo '{}'; fi
  }
  create_run() { echo "$RUN_B1"; }
  wait_custody_headroom() { :; }
  wait_status() { :; }
  tick_count() { echo 3; }
  eval "$(awk '/^make_outbox_run\(\) \{/,/^\}/' "$PHASE")"
  make_outbox_run first_cut_barrier
  jq -e '.description | contains("UZI_STUB_OUTBOX_FIRST_CUT_BARRIER") and (contains("UZI_STUB_OUTBOX_CUT_BARRIER") | not)' "$SCRATCH/issue.json" >/dev/null
  make_outbox_run cut_barrier
  jq -e '.description | contains("UZI_STUB_OUTBOX_CUT_BARRIER")' "$SCRATCH/issue.json" >/dev/null
)
echo 'PASS: real issue producer arms first-claim and every-execution barriers distinctly'
echo 'cases=24 passed=24'
