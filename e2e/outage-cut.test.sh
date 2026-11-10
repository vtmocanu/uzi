#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PHASE="${OUTBOX_TEST_PHASE:-$ROOT/e2e/phases/52-api-outage-outbox.sh}"
SCRATCH="$(mktemp -d "$ROOT/.uzi/scratch/outage-cut.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT
# The real phase-52 helpers snapshot agent logs before stop/recreate (#4060); the fake compose below
# answers the ps/logs reads against the scratch RUNROOT.
eval "$(awk '/^capture_agent_logs\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
fixture() {
  # shellcheck disable=SC2034  # evaluated real helpers consume these variables
  RUNROOT="$SCRATCH" PROJECT=fixture COMPOSE=(fake_compose) OUTBOX_BASE_COMPOSE=(fake_compose) OUTBOX_RESTORE_NEEDED=0 RUN_B1=00000000-0000-0000-0000-000000000001
  AGENT_RUNNING=0 SETTLED=1
  # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
  OUTBOX_AGENT_CONTAINER=owned-current OUTBOX_WORKER_ID="$RUN_B1" OUTBOX_PROBED_FEATURE=true
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
      stop)
        if [ "$2" = agent ]; then
          [ "$SETTLED" = 1 ] || fail 'agent stopped before fixture settlement'
          AGENT_RUNNING=0
        else [ "$ALIAS_AVAILABLE" = 1 ] || return 1; API_AVAILABLE=0; fi ;;
      ps) echo owned-current ;;
      pause) API_PAUSED=1; API_AVAILABLE=0 ;;
      up)
        case "${!#}" in
          outbox-api-placeholder) [ "$REFUSE_ALIAS" = 0 ] || return 1;
            if [ "$SLOW_HELPER:$BARRIER_ARMED" = 1:0 ]; then FIXTURE_FINISHED=1; fi
            ALIAS_AVAILABLE=1 ;;
          api) [ "$REFUSE_API" = 0 ] || return 1; API_AVAILABLE=1; API_PAUSED=0 ;;
          agent) AGENT_RUNNING=1; [ "$RESTORING" != 1 ] || [ -z "${UZI_E2E_RECOVERY_FAST_REDRIVE:-}" ] \
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
  say() { :; }
  wait_http() { [ "$API_AVAILABLE:$ALIAS_AVAILABLE" = 1:0 ]; }
  login() { :; }
  docker_unwire_prepare() { :; }
  docker_unwire_assert() { :; }
  wait_worker_online() { :; }
  apipost() { :; }
  wait_runs_terminal_off_worker() {
    printf 'settled %s\n' "$*" >> "$SCRATCH/calls"
    SETTLED=1
  }
  probe_outbox_feature() {
    [ "$AGENT_RUNNING:$API_AVAILABLE:$ALIAS_AVAILABLE" = 0:1:0 ] || fail 'probe requires stopped agent after health'
    OUTBOX_PROBED_FEATURE="$1"
    printf 'probe %s\n' "$1" >> "$SCRATCH/calls"
  }
  set_interlock() { printf 'interlock %s\n' "$1" >> "$SCRATCH/calls"; }
  wait_outbox_feature() {
    [ "$API_AVAILABLE:$ALIAS_AVAILABLE" = 1:0 ] || fail 'negotiation before health'
    [ "${E2E_RUN_PUBLICATION_RESUME_ENABLED:-true}" = "$1" ] || fail 'incorrect feature setting'
    printf 'negotiated %s\n' "$1" >> "$SCRATCH/calls"
  }
  record_margin() { printf 'margin %s %s\n' "$1" "$2" >> "$SCRATCH/calls"; }
  wait_publication_event() {
    [ "$API_AVAILABLE:$BARRIER_ARMED" = 1:1 ] || fail 'publication readiness must precede API cut'
    printf 'publication-ready\n' >> "$SCRATCH/calls"
  }
  make_outbox_run() {
    case "${1:-}" in cut_barrier|first_cut_barrier|publication_cut_barrier) BARRIER_ARMED=1 ;; esac
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
  eval "$(awk '/^(configure_outbox_api_alias|start_outbox_api_alias|remove_outbox_api_alias|recreate_outbox_api|recreate_outbox_agent|stop_outbox_agent|api_cut|release_outbox_cut_barrier|api_back|restore_outbox_resolution|save_outbox_agent_evidence|restore_outage_state|transition_outbox_feature|record_outbox_transition|f42_cut)\(\) \{/,/^\}/' "$PHASE")"
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
( fixture
  : > "$SCRATCH/calls"
  configure_outbox_api_alias
  AGENT_RUNNING=1 SETTLED=0 OUTBOX_RESTORE_NEEDED=1
  docker_unwire_prepare() {
    [ "$SETTLED:$AGENT_RUNNING" = 1:0 ] || fail 'unwire preceded fixture settlement and agent stop'
    echo unwire-prepared >> "$SCRATCH/calls"
  }
  docker_unwire_assert() {
    [ "$AGENT_RUNNING" = 1 ] || fail 'final unwired check preceded agent recreation'
    echo final-unwired >> "$SCRATCH/calls"
  }
  restore_outbox_resolution
  awk '/^settled / && !settled { settled=NR } /^unwire-prepared$/ { unwire=NR }
    /force-recreate agent/ { recreated=NR; count++ } /^final-unwired$/ { checked=NR }
    END { exit(settled && unwire > settled && recreated > unwire && checked > recreated && count == 1 ? 0 : 1) }' "$SCRATCH/calls" \
    || fail 'cleanup must settle, unwire, recreate once, and check the final container'
)
echo 'PASS: phase 52 settles fixtures before unwiring and checks its sole final agent recreate'
( fixture
  : > "$SCRATCH/calls"
  configure_outbox_api_alias
  AGENT_RUNNING=1 OUTBOX_RESTORE_NEEDED=1
  wait_runs_terminal_off_worker() { return 1; }
  docker_wire_exit_restore() { echo unsafe-fallback >> "$SCRATCH/calls"; }
  local_rc=0
  restore_outage_state || local_rc=$?
  [ "$local_rc" != 0 ] || fail 'unconfirmed cleanup must fail even after a successful phase'
  if grep -F unsafe-fallback "$SCRATCH/calls" >/dev/null; then fail 'unconfirmed cleanup forced a blind recreate'; fi
  if grep -F 'force-recreate agent' "$SCRATCH/calls" >/dev/null; then fail 'unconfirmed cleanup recreated the agent'; fi
)
echo 'PASS: unconfirmed phase-52 settlement leaves the agent intact and reports cleanup failure'
fixture
configure_outbox_api_alias
SLOW_HELPER=1
wait_publication_event() { wait_terminal_lost "$1"; }
# shellcheck disable=SC2034  # real extracted case consumes generation
GEN_B1=1
eval "$(awk '
  /^say "CASE 3:/ { inside=1; next }
  /^say "CASE 4:/ { inside=0 }
  inside && (/^make_outbox_run/ || /^api_cut$/ || /^release_outbox_cut_barrier/ || /^intent_wait_b1=/ || /^api_back$/) { print }
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
awk '/force-recreate api/ { api=NR } /force-recreate agent/ { agent=NR }
  /interlock true/ && !interlock { interlock=NR } /negotiated true/ { negotiated=NR }
  END { exit(api && interlock > api && agent > interlock && negotiated > agent ? 0 : 1) }' "$SCRATCH/calls"
[ "$(awk -F '\t' '$1 == "cleanup" && $2 == "reused-existing" { n++ } END {print n+0}' "$SCRATCH/artifacts/52-api-outage-outbox/feature-transitions.tsv")" -ge 1 ]
echo 'PASS: failure trap restores defaults, health, interlock and negotiation using existing cleanup recreate'
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
    RUN_B2="$RUN_B1" RUN_B3="$RUN_B1" RUN_B4="$RUN_B1" GEN_B4=1 SLOW_HELPER=1
    eval "$(awk -v selected="$case_number" '
      /^say "CASE [0-9]+:/ { inside = index($0, "CASE " selected ":") > 0 }
      inside && (/^make_outbox_run/ || /^api_cut$/ || /^release_outbox_cut_barrier/ ||
        /^terminal_lost_b[234]=/ || /^publication_retrying_b4=/ || /^wait_publication_event/ || /^  \|\| fail "case 6: publication enrollment/ || /^api_back$/ ||
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
  make_outbox_run publication_cut_barrier
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
  wait_outbox_feature() {
    [ "$1" = "${E2E_RUN_PUBLICATION_RESUME_ENABLED:-true}" ] || fail 'wrong fixture feature'
    echo registered > "$SCRATCH/fixture-ready"
  }
  apipost() {
    if [[ "$1" == */issues ]]; then
      [ -f "$SCRATCH/fixture-ready" ] || fail 'fixture created before registration gate'
      rm "$SCRATCH/fixture-ready"
      printf '%s\n' "$2" > "$SCRATCH/issue.json"
      echo '{"card":{"iid":7}}'
    else echo '{}'; fi
  }
  create_run() { echo "$RUN_B1"; }
  wait_custody_headroom() { :; }
  wait_status() { :; }
  tick_count() { echo 3; }
  eval "$(awk '/^make_outbox_run\(\) \{/,/^\}/' "$PHASE")"
  for n in 1 2 3 4 5 6 7 8; do
    E2E_RUN_PUBLICATION_RESUME_ENABLED=true
    case "$n" in 4|5|7|8) E2E_RUN_PUBLICATION_RESUME_ENABLED=false ;; esac
    # Execute the actual fixture call for each case through the guarded producer.
    eval "$(awk -v selected="$n" '
      /^say "CASE [0-9]+:/ { inside = index($0, "CASE " selected ":") > 0 }
      inside && /^make_outbox_run/ { print; exit }
    ' "$PHASE")"
  done
  make_outbox_run first_cut_barrier
  jq -e '.description | contains("UZI_STUB_OUTBOX_FIRST_CUT_BARRIER") and (contains("UZI_STUB_OUTBOX_CUT_BARRIER") | not)' "$SCRATCH/issue.json" >/dev/null
  make_outbox_run publication_cut_barrier
  jq -e '.description | contains("UZI_STUB_OUTBOX_PUBLICATION_CUT_BARRIER") and (contains("UZI_STUB_OUTBOX_CUT_BARRIER") | not) and (contains("UZI_STUB_OUTBOX_FIRST_CUT_BARRIER") | not)' "$SCRATCH/issue.json" >/dev/null
  make_outbox_run cut_barrier
  jq -e '.description | contains("UZI_STUB_OUTBOX_CUT_BARRIER")' "$SCRATCH/issue.json" >/dev/null
)
echo 'PASS: real issue producer arms first-claim and every-execution barriers distinctly'
# Same run/generation event gates exercise the actual JSON parser and polling loop.
for format in json compose-spaced; do
  for variant in valid readoption wrongrun wronggen wrongreason split; do
    (
      fixture
      eval "$(awk '/^wait_publication_event\(\) \{/,/^\}/' "$PHASE")"
      sleep() { SECONDS=$((SECONDS + 1)); }
      run="$RUN_B1" gen=3 reason=ack_unknown expected=0
      case "$variant" in
        readoption) reason=readoption_pending ;;
        wrongrun) run=00000000-0000-0000-0000-000000000002; expected=1 ;;
        wronggen) gen=30; expected=1 ;;
        wrongreason) reason=aborted; expected=1 ;;
        split) expected=1 ;;
      esac
      evidence="$(jq -nc --arg run "$run" --argjson g "$gen" --arg reason "$reason" \
        '{msg:"publication intent reconciliation waiting",run_id:$run,claim_generation:$g,reason:$reason}')"
      [ "$format" = json ] || evidence="agent-1 | $(printf '%s' "$evidence" | jq -c '. ' | sed 's/:/: /g')"
      [ "$variant" != split ] || evidence="{\"msg\":\"publication intent reconciliation waiting\",\"run_id\":\"$run\"}
{\"claim_generation\":3,\"reason\":\"ack_unknown\"}"
      fake_compose() { printf '%s\n' "$evidence"; }
      rc=0
      wait_publication_event "$RUN_B1" 3 'publication intent reconciliation waiting' 2 || rc=$?
      [ "$rc" = "$expected" ] || fail "intent event $format/$variant: $rc != $expected"
    )
  done
done
echo 'PASS: actual intent gate scopes exact JSON event, run, generation and permitted reasons'
# Real probe + current-start registration parser; no Docker daemon.
for variant in valid off restart wrongbit wrongworker malformed absent mixed stale wrongcontainer wronglogworker nocap badcaps; do
  (
    fixture
    eval "$(awk '/^(probe_outbox_feature|wait_outbox_feature)\(\) \{/,/^\}/' "$PHASE")"
    # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
    BASE=http://fixture UZI_WORKER_TOKEN=fixture-token
    sleep() { SECONDS=$((SECONDS + 1)); }
    db_psql() { echo "$RUN_B1"; }
    curl() {
      [ "$AGENT_RUNNING:$API_AVAILABLE:$ALIAS_AVAILABLE" = 0:1:0 ] || fail 'live or unhealthy probe'
      jq -nc --arg id "$RUN_B1" --arg v "$variant" '
        {worker:{id:(if $v == "wrongworker" then "other" else $id end)},
         protocol_features:(if $v == "off" or $v == "wrongbit" then []
           elif $v == "malformed" then "publication_resume_v1"
           elif $v == "absent" then null
           elif $v == "mixed" then ["publication_resume_v1",42]
           else ["publication_resume_v1"] end)}'
    }
    current_start=2026-11-03T00:01:00Z
    docker() {
      case "$1" in
        inspect) [ "${!#}" = owned-current ] || fail 'foreign inspect'; echo "$current_start" ;;
        logs)
          [ "$*" = "logs --since $current_start owned-current" ] || fail 'logs not scoped to current start'
          [ "$current_start" != 2026-11-03T00:02:00Z ] || return 0
          [ "$variant" != stale ] || return 0 # old registered exists before --since only
          jq -nc --arg id "$RUN_B1" --arg v "$variant" '
            {msg:"registered",worker_id:(if $v == "wronglogworker" then "other" else $id end),
             protocol_capabilities:(if $v == "nocap" then [] elif $v == "badcaps" then
               ["publication_resume_v1",42] else ["publication_resume_v1"] end)}' ;;
      esac
    }
    expected=true; [ "$variant" != off ] || expected=false
    rc=0
    probe_outbox_feature "$expected" || rc=$?
    case "$variant" in
      wrongbit|wrongworker|malformed|absent|mixed) [ "$rc" != 0 ] || fail 'bad probe accepted'; exit 0 ;;
    esac
    [ "$rc" = 0 ] || fail 'good probe refused'
    AGENT_RUNNING=1
    # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
    [ "$variant" != wrongcontainer ] || OUTBOX_AGENT_CONTAINER=old-container
    rc=0; wait_outbox_feature "$expected" || rc=$?
    case "$variant" in
      valid|off|restart) [ "$rc" = 0 ] || fail 'good registration refused' ;;
      *) [ "$rc" != 0 ] || fail 'stale/wrong registration accepted' ;;
    esac
    if [ "$variant" = restart ]; then
      # Same ID, later start, only the old registration remains in Docker history.
      current_start=2026-11-03T00:02:00Z
      if wait_outbox_feature "$expected"; then fail 'same-container restart counted old registration twice'; fi
    fi
  )
done
echo 'PASS: stopped probe validates authenticated worker and string features; current-start registration rejects stale/foreign/malformed evidence'
# Execute the actual entry through fixture 1, beginning with phase 51's stopped agent.
(
  fixture
  configure_outbox_api_alias
  unset E2E_RUN_PUBLICATION_RESUME_ENABLED
  : > "$SCRATCH/calls"
  eval "$(awk '
    /^export E2E_WORKER_HEARTBEAT_STALE=300s/ { inside=1 }
    inside { print }
    inside && /^make_outbox_run$/ { exit }
  ' "$PHASE")"
  [ "$AGENT_RUNNING" = 1 ] || fail 'entry did not recreate agent'
  awk '/force-recreate api/ {api=NR} /probe true/ {probe=NR}
    /force-recreate agent/ {agent=NR} /negotiated true/ {registered=NR}
    END {exit(api && probe>api && agent>probe && registered>agent ? 0 : 1)}' "$SCRATCH/calls"
)
echo 'PASS: stopped entry reaches first real recreation before registration gate'
(
  fixture
  configure_outbox_api_alias
  : > "$SCRATCH/calls"
  unset SECONDS
  SECONDS=100
  # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
  RUN_B3="$RUN_B1" RUN_B4="$RUN_B1"
  for pair in before4:false before6:true before7:false; do
    SETTLED=0
    wait_runs_terminal_off_worker() {
      [ "$*" = "120 $RUN_B1" ] || fail 'wrong preceding fixture settlement'
      SETTLED=1; SECONDS=$((SECONDS + 1))
    }
    probe_outbox_feature() {
      [ "$AGENT_RUNNING" = 0 ] || fail 'live probe'
      # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
    OUTBOX_PROBED_FEATURE="$1"; SECONDS=$((SECONDS + 2))
    }
    wait_worker_online() { SECONDS=$((SECONDS + 4)); }
    # Deterministic clock includes settlement, API, probe, agent startup and registration.
    recreate_outbox_api() { echo "recreate $E2E_RUN_PUBLICATION_RESUME_ENABLED" >> "$SCRATCH/calls"; SECONDS=$((SECONDS + 2)); }
    wait_outbox_feature() { echo "negotiated $1" >> "$SCRATCH/calls"; SECONDS=$((SECONDS + 3)); }
    eval "$(awk -v label="${pair%:*}" '$1 == "transition_outbox_feature" && $2 == label { print }' "$PHASE")"
    echo fixture >> "$SCRATCH/calls"
  done
  [ "$(awk '/^(recreate|negotiated|fixture)/' "$SCRATCH/calls")" = "$(printf 'recreate false\nnegotiated false\nfixture\nrecreate true\nnegotiated true\nfixture\nrecreate false\nnegotiated false\nfixture')" ] || fail 'transition/fixture order'
  awk -F '\t' '$1 ~ /^before/ { n++; if ($2 != "additional" || $3 != 12) exit 1 } END { if(n != 3) exit 1 }' \
    "$SCRATCH/artifacts/52-api-outage-outbox/feature-transitions.tsv" || fail 'simulated durations'
)
(
  fixture
  configure_outbox_api_alias
  export E2E_RUN_PUBLICATION_RESUME_ENABLED=false
  unset SECONDS
  SECONDS=100
  save_outbox_agent_evidence() { :; }
  wait_http() { SECONDS=$((SECONDS + 2)); }
  wait_runs_terminal_off_worker() {
    [ "$*" = "120 $RUN_B1" ] || fail 'cleanup touched foreign fixtures'
    SETTLED=1; SECONDS=$((SECONDS + 1))
  }
  probe_outbox_feature() {
    [ "$AGENT_RUNNING" = 0 ] || fail 'cleanup probed live agent'
    [ "${E2E_RUN_PUBLICATION_RESUME_ENABLED+set}" != set ] || fail 'cleanup retained overlay'
    # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
    OUTBOX_PROBED_FEATURE="$1"; SECONDS=$((SECONDS + 2))
  }
  wait_worker_online() { SECONDS=$((SECONDS + 4)); }
  wait_outbox_feature() {
    [ "${E2E_RUN_PUBLICATION_RESUME_ENABLED+set}" != set ] || fail 'cleanup must unset before negotiation'
    [ "$1" = true ] || fail 'cleanup did not negotiate default true'
    SECONDS=$((SECONDS + 3))
  }
  restore_outbox_resolution
  [ "$(tail -1 "$SCRATCH/artifacts/52-api-outage-outbox/feature-transitions.tsv")" = "$(printf 'cleanup\treused-existing\t12')" ] || fail 'cleanup simulated duration omits health or negotiation'
)
echo 'PASS: four timings include settlement, API health, probe, agent startup and registration (three additional API recreations, reused cleanup)'
# Case 3's real invariant must reject a requeue even if its generation stays put.
if (
  fixture
  # shellcheck disable=SC2034  # real extracted assertion consumes baseline
  RQ_B1=0
  rb_run_field() { echo 1; }
  eval "$(awk '/^\[ "\$\(rb_run_field "\$RUN_B1" requeue_count\)/ { print; getline; print }' "$PHASE")"
); then echo 'FAIL: case 3 accepted requeue'; exit 1; fi
echo 'PASS: case 3 rejects requeue independently of generation'
(
  fixture
  configure_outbox_api_alias
  export E2E_RUN_PUBLICATION_RESUME_ENABLED=false
  REFUSE_API=1
  if restore_outbox_resolution; then fail 'cleanup accepted refused API'; fi
  [ "${E2E_RUN_PUBLICATION_RESUME_ENABLED+set}" != set ] || fail 'failed cleanup retained internal knob'
)
echo 'PASS: failed cleanup still unsets internal knob'
# Complete remote-head fingerprints catch local-path pushes even without receive-pack HTTP.
(
  fixture
  eval "$(awk '/^outbox_remote_heads\(\) \{/,/^\}/' "$PHASE")"
  seed_git() { GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git "$@"; }
  mkdir -p "$RUNROOT/fakeremote"
  seed_git init -q --bare "$RUNROOT/fakeremote/repo.git"
  tree="$(printf '' | seed_git --git-dir="$RUNROOT/fakeremote/repo.git" mktree)"
  oid="$(printf 'fixture\n' | seed_git --git-dir="$RUNROOT/fakeremote/repo.git" \
    -c user.name=fixture -c user.email=fixture@example.test commit-tree "$tree")"
  seed_git --git-dir="$RUNROOT/fakeremote/repo.git" update-ref refs/heads/main "$oid"
  before="$(outbox_remote_heads)"
  [ "$before" = "$(outbox_remote_heads)" ] || fail 'unstable ordered refs'
  seed_git --git-dir="$RUNROOT/fakeremote/repo.git" update-ref refs/heads/publication "$oid"
  [ "$before" != "$(outbox_remote_heads)" ] || fail 'publication branch invisible to fingerprint'
  seed_git() { return 7; }
  if outbox_remote_heads >/dev/null; then fail 'Git failure hidden by digest pipeline'; fi
)
echo 'PASS: complete ordered remote-head fingerprint detects publication branches and propagates Git failures'
for observation in unchanged pushed gitfailed mrfailed statusfailed; do
  rc=0
  (
    fixture
    API_AVAILABLE=0 ALIAS_AVAILABLE=1 BARRIER_RELEASED=1
    # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
    GEN_B1=1 MRS_B1=0 HEADS_B1=before
    # shellcheck disable=SC2034  # extracted phase helpers consume these fixture values
    intent_wait_b1=1
    outbox_remote_heads() {
      case "$observation" in gitfailed) return 7 ;; pushed) echo after ;; *) echo before ;; esac
    }
    fake_state() { [ "$observation" != mrfailed ] || return 7; echo '{"mrs":[]}'; }
    rb_run_field() { [ "$observation" != statusfailed ] || return 7; echo running; }
    fail() {
      [ "$API_AVAILABLE:$ALIAS_AVAILABLE" = 1:0 ] || exit 8
      exit 9
    }
    eval "$(awk '
      /^intent_file_b1=0/ { inside=1 }
      /^# Completion is fenced/ { inside=0 }
      inside { print }
    ' "$PHASE")"
  ) || rc=$?
  expected=9; [ "$observation" != unchanged ] || expected=0
  [ "$rc" = "$expected" ] || { echo "FAIL: offline $observation rc=$rc"; exit 1; }
done
echo 'PASS: CASE 3 judges remote push and all offline observation failures only after API restore'
# Compose renders have a separate target; its availability policy is tested here
# with missing/broken clients, so this helper suite still needs no Docker client.
render_test="$ROOT/e2e/compose-config.test.sh"
mkdir -p "$SCRATCH/no-docker" "$SCRATCH/broken-compose"
cat > "$SCRATCH/broken-compose/docker" <<'SH'
#!/bin/sh
exit 127
SH
chmod +x "$SCRATCH/broken-compose/docker"
for unavailable in no-docker broken-compose; do
  env PATH="$SCRATCH/$unavailable" CI= GITHUB_ACTIONS= /bin/bash "$render_test" > "$SCRATCH/render-local.log" 2>&1
  grep -F 'SKIP: E2E overlay renders need Docker Compose' "$SCRATCH/render-local.log" >/dev/null
  ! grep -F 'PASS:' "$SCRATCH/render-local.log" >/dev/null \
    || { echo 'FAIL: unavailable local Compose reported PASS'; exit 1; }
  for context in CI GITHUB_ACTIONS; do
    if env PATH="$SCRATCH/$unavailable" CI= GITHUB_ACTIONS= "$context=true" \
        /bin/bash "$render_test" > "$SCRATCH/render-ci.log" 2>&1; then
      echo "FAIL: $context accepted unavailable Compose"; exit 1
    fi
    grep -F 'FAIL: Docker Compose is required' "$SCRATCH/render-ci.log" >/dev/null
  done
done
cat > "$SCRATCH/broken-compose/docker" <<'SH'
#!/bin/sh
[ "$*" != 'compose version' ] || exit 0
exit 23
SH
rc=0
env PATH="$SCRATCH/broken-compose:$PATH" CI=true GITHUB_ACTIONS= /bin/bash "$render_test" > "$SCRATCH/render-failed.log" 2>&1 || rc=$?
[ "$rc" = 23 ] || { echo "FAIL: Compose render failure changed exit $rc"; exit 1; }
! grep -F 'PASS:' "$SCRATCH/render-failed.log" >/dev/null \
  || { echo 'FAIL: failed Compose render reported PASS'; exit 1; }
echo 'PASS: separate Compose renders visibly skip locally, are mandatory in CI, and preserve render failures'
