#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PHASE="${OUTBOX_TEST_PHASE:-$ROOT/e2e/phases/52-api-outage-outbox.sh}"
mkdir -p "$ROOT/.uzi/scratch/tmp"
export TMPDIR="$ROOT/.uzi/scratch/tmp"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
fixture() {
  # shellcheck disable=SC2034  # evaluated real helpers consume these variables
  RUNROOT="$SCRATCH" PROJECT=fixture COMPOSE=(fake_compose) OUTBOX_BASE_COMPOSE=(fake_compose) OUTBOX_RESTORE_NEEDED=0 RUN_B1=00000000-0000-0000-0000-000000000001
  API_AVAILABLE=1 API_PAUSED=0 ALIAS_AVAILABLE=0 REFUSE_API=0 REFUSE_ALIAS=0 REFUSE_REMOVE=0
  BARRIER_ARMED=0 BARRIER_RELEASED=0 FIXTURE_FINISHED=0 SLOW_HELPER=0
  BARRIER_STAGE="cut"
  # shellcheck disable=SC2034  # real extracted case releases consume these generations
  GEN_B1=1 GEN_B2=1 GEN_B3=1 GEN_B4=1
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
        local release_run="${9:-}" release_stage="${10:-}" release_gen="${11:-}"
        [ "$release_run" = "$RUN_B1" ] || fail 'release wrong run'
        [ "$release_stage" = "$BARRIER_STAGE" ] || fail 'release wrong stage'
        if [ "$release_stage" != cut ]; then
          [ "$release_gen" = 1 ] || fail 'release wrong generation'
        fi
        [[ "${8:-}" == *'flag:"wx"'* ]] || fail 'release must be exclusive'
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
    case "${1:-}" in cut_barrier|first_cut_barrier|terminal|running-ack) BARRIER_ARMED=1 ;; esac
    case "${1:-}" in terminal|running-ack) BARRIER_STAGE="$1";; *) BARRIER_STAGE="cut";; esac
  }
  rb_run_field() { echo 1; }
  wait_fixture_worker_event() {
    [ "$API_AVAILABLE" = 1 ] || fail 'stage readiness waited after API cut'
  }
  f42_settled_ready() { f42_durable_logged "$@"; }
  f42_files_ready() {
    [ "$BARRIER_RELEASED" = 1 ] || fail 'finalize file checked before barrier release'
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
  make_outbox_run running-ack
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
  jq -e '.description | contains("UZI_STUB_OUTBOX_FIRST_CUT_BARRIER") and contains("UZI_STUB_FIRST_PRE_ENROLLMENT_STOP") and (contains("UZI_STUB_PRE_ENROLLMENT_STOP") | not) and (contains("UZI_STUB_OUTBOX_CUT_BARRIER") | not)' "$SCRATCH/issue.json" >/dev/null
  make_outbox_run cut_barrier
  jq -e '.description | contains("UZI_STUB_OUTBOX_CUT_BARRIER") and contains("UZI_STUB_PRE_ENROLLMENT_STOP") and (contains("UZI_STUB_FIRST_PRE_ENROLLMENT_STOP") | not)' "$SCRATCH/issue.json" >/dev/null
)
echo 'PASS: real issue producer arms first-claim and every-execution barriers distinctly'
# Run exact production decoders and restart preconditions with synthetic worker logs.
# No evaluated candidate payload: fake Compose interprets only fixed inspection commands.
eval "$(awk '/^(fixture_event_ready|fixture_worker_log|fixture_worker_event_ready|wait_fixture_worker_event)\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
settled_fixture() {
  local variant="$1"
  jq -nc --arg r "$RUN_B1" --arg v "$variant" '
    [{msg:"finalize record durable",run_id:$r,claim_generation:1},
     {msg:"stub: pre-enrollment retained stop",run_id:$r,claim_generation:1},
     {msg:"stub: source-end ready",run_id:$r,claim_generation:1,sources:2}] |
    if $v=="stale" then map(.claim_generation=2)
    elif $v=="wrong-run" then map(.run_id="other")
    elif $v=="missing" then []
    elif $v=="partial" then .[:2]
    elif $v=="warning" then .[2].msg="warning: stub: source-end ready"
    elif $v=="zero" then .[2].sources=0
    elif $v=="terminal-log" then .+[{msg:"outbox: terminal journaled write-ahead",run_id:$r,claim_generation:1}]
    elif $v=="publication-log" then .+[{msg:"publication record durable",run_id:$r,claim_generation:1}]
    else . end | .[]' > "$SCRATCH/settled.jsonl"
}
for variant in valid stale wrong-run missing partial warning zero terminal-log publication-log terminal-file publication-file no-finalize inspection-error log-error; do
  (
    fixture
    eval "$(awk '/^(fixture_worker_log|fixture_worker_event_ready|wait_fixture_worker_event)\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
    eval "$(awk '/^(f42_settled_ready|f42_files_ready|f42_restart_cycle|f42_cut_judge)\(\) \{/,/^\}/' "$PHASE")"
    settled_fixture "$variant"
    API_AVAILABLE=0
    : > "$SCRATCH/restart-calls"
    fake_compose() {
      printf '%s\n' "$*" >> "$SCRATCH/restart-calls"
      case "$1" in
        logs)
          [ "$variant" != log-error ] || return 1
          cat "$SCRATCH/settled.jsonl"
          echo '{"msg":"registered"}'
          [ ! -f "$SCRATCH/restarted" ] || echo '{"msg":"registered"}'
          ;;
        exec)
          # Verify the real inspection covers all three record families; inject its exit status.
          [[ "${6:-}" == *'publication-'* && "${6:-}" == *'terminal-'* && "${6:-}" == *'finalize-'* ]] || return 1
          case "$variant" in terminal-file|publication-file|no-finalize|inspection-error) return 1;; esac
          ;;
        kill)
          [ "$variant" = valid ] || fail "unsafe kill: $variant"
          : > "$SCRATCH/restarted"
          ;;
        start) :;;
      esac
    }
    rm -f "$SCRATCH/restarted"
    api_back() { API_AVAILABLE=1; }
    say() { :; }
    fail() {
      [ "$API_AVAILABLE" = 1 ] || exit 2
      echo "restored: $*" >&2
      exit 9
    }
    rc=0
    ( f42_restart_cycle "$RUN_B1" 1 2 0 ) > "$SCRATCH/restart-$variant.log" 2>&1 || rc=$?
    if [ "$variant" = valid ]; then
      [ "$rc" = 0 ] || { cat "$SCRATCH/restart-$variant.log"; exit 1; }
      grep -F 'kill -s SIGKILL agent' "$SCRATCH/restart-calls" >/dev/null
    else
      [ "$rc" = 9 ] || { cat "$SCRATCH/restart-$variant.log"; exit 1; }
      ! grep -F 'kill -s SIGKILL agent' "$SCRATCH/restart-calls" >/dev/null
      grep -F 'restored:' "$SCRATCH/restart-$variant.log" >/dev/null
    fi
  )
  echo "PASS: restart precondition $variant preserves readiness and restores before judging"
done
# Real issue producer must positively wait for its exact stage/run/generation while API is UP.
for stage in terminal running-ack; do
  for variant in valid stale missing wrong-stage; do
    (
      fixture
      eval "$(awk '/^(fixture_worker_log|fixture_worker_event_ready|wait_fixture_worker_event)\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
      eval "$(awk '/^make_outbox_run\(\) \{/,/^\}/' "$PHASE")"
      # shellcheck disable=SC2034  # extracted producer consumes the repo identity
      REPO_ID=fixture
      apipost() { echo '{"card":{"iid":7}}'; }
      create_run() { echo "$RUN_B1"; }
      wait_custody_headroom() { :; }
      wait_status() { :; }
      tick_count() { echo 3; }
      sleep() { SECONDS=$((SECONDS + 90)); }
      fake_compose() {
        [ "$API_AVAILABLE" = 1 ] || fail 'readiness observed after API cut'
        [ "$variant" != missing ] || return 0
        local gen=1 log_stage="$stage"
        [ "$variant" != stale ] || gen=2
        [ "$variant" != wrong-stage ] || log_stage=other
        jq -nc --arg r "$RUN_B1" --arg stage "$log_stage" --argjson gen "$gen"           '{msg:"stub: fixture barrier ready",run_id:$r,claim_generation:$gen,stage:$stage}'
      }
      rc=0
      ( make_outbox_run "$stage" ) > "$SCRATCH/producer-$stage-$variant.log" 2>&1 || rc=$?
      if [ "$variant" = valid ]; then
        [ "$rc" = 0 ] || { cat "$SCRATCH/producer-$stage-$variant.log"; exit 1; }
      else
        [ "$rc" != 0 ]
        grep -F "no exact $stage readiness" "$SCRATCH/producer-$stage-$variant.log" >/dev/null
      fi
    )
    echo "PASS: real producer $stage readiness $variant gates before outage"
  done
done
# Real f42_cut timeout and release-failure paths restore before their caller judges.
for variant in timeout release-error; do
  (
    fixture
    configure_outbox_api_alias
    f42_durable_logged() { return 1; }
    f42_settled_ready() { return 1; }
    sleep() { SECONDS=$((SECONDS + 90)); }
    [ "$variant" != release-error ] || release_outbox_cut_barrier() { return 1; }
    rc=0; f42_cut "$RUN_B1" 1 || rc=$?
    [ "$rc" = 1 ]
    api_back
    [ "$API_AVAILABLE:$ALIAS_AVAILABLE" = 1:0 ]
    eval "$(awk '/^f42_cut_judge\(\) \{/,/^\}/' "$PHASE")"
    fail() { [ "$API_AVAILABLE" = 1 ] || exit 2; exit 9; }
    result=0; ( f42_cut_judge "$rc" "$RUN_B1" 1 ) || result=$?
    [ "$result" = 9 ]
  )
  echo "PASS: finalize $variant restores API before failure"
done

# Real durable-file projection and predicates: completion/gaplessness alone must not
# satisfy the terminal cut when the post-release trace was missing or already ACKed.
eval "$(awk '/^(terminal_tail_projection|terminal_tail_pending_valid|terminal_tail_persisted_valid|wait_terminal_tail_pending|snapshot_terminal_tail|assert_terminal_tail_replayed)\(\) \{/,/^\}/' "$PHASE")"
(
  fixture
  run="$RUN_B1"
  dir="$SCRATCH/tail-outbox/$run"
  mkdir -p "$dir"
  fake_compose() {
    [ "$*" = "exec -T agent timeout 15 node --input-type=module - /data/outbox $run 2" ] || return 1
    node --input-type=module - "$SCRATCH/tail-outbox" "$run" 2
  }
  COMPOSE=(fake_compose)
  jq -nc --arg r "$run" '{run_id:$r,claim_generation:2,body:{status:"completed"},messages_through_seq:12}' > "$dir/terminal-2.json"
  jq -nc --arg r "$run" '{runId:$r,cursor:10,records:[{kind:"segment",firstSeq:11,lastSeq:12,file:"seg.json"}]}' > "$dir/manifest.json"
  jq -nc --arg r "$run" '{runId:$r,generation:2,messages:[{seq:12,payload:{event:"stub_outbox_terminal_tail",run_id:$r,claim_generation:2}}]}' > "$dir/seg.json"
  evidence="$(terminal_tail_projection "$run" 2)"
  printf '%s\n' "$evidence" | terminal_tail_pending_valid "$run" 2 10
  for filter in '.tails=[]' '.tails[0].seq=10' '.cursor=12' '.run="foreign"'       '.generation=1' '.manifest_run="foreign"' '.tails[0].run="foreign"'       '.tails[0].generation=1' '.tails[0].payload.run_id="foreign"'       '.tails[0].payload.claim_generation=1' '.tails[0].seq=0' '.fence=11'       '.tails[0].first=13' '.tails[0].last=11' '.tails += .tails'; do
    if printf '%s\n' "$evidence" | jq "$filter" | terminal_tail_pending_valid "$run" 2 10; then
      fail "pending tail accepted $filter"
    fi
  done
  if terminal_tail_pending_valid "$run" 2 10 < /dev/null; then fail 'absent pending proof accepted'; fi
  # A real manifest cursor at the tail removes its segment from the projection.
  jq '.cursor=12' "$dir/manifest.json" > "$SCRATCH/acked.json"
  cp "$SCRATCH/acked.json" "$dir/manifest.json"
  if terminal_tail_projection "$run" 2 | terminal_tail_pending_valid "$run" 2 10; then fail 'retired segment accepted'; fi
  rm "$dir/seg.json"
  if terminal_tail_projection "$run" 2 | terminal_tail_pending_valid "$run" 2 10; then fail 'absent segment accepted'; fi
  echo 'PASS: terminal cut requires an exact post-cut pending message segment and covering journal fence'

  persisted="$(jq -nc --arg r "$run" '{run:$r,generation:2,status:"completed",last_seq:12,tail_count:1,tail_seq:12,tail_run:$r,tail_generation:2,fence_count:12,after_tail:true}')"
  printf '%s\n' "$persisted" | terminal_tail_persisted_valid "$run" 2 12 12
  for filter in '.after_tail=false' '.after_tail=null' '.tail_count=0' '.tail_count=2'       '.tail_seq=10' '.tail_run="foreign"' '.tail_generation=1' '.run="foreign"'       '.generation=1' '.last_seq=11' '.fence_count=11' '.status="running"'; do
    if printf '%s\n' "$persisted" | jq "$filter" | terminal_tail_persisted_valid "$run" 2 12 12; then
      fail "persisted tail accepted $filter"
    fi
  done
  if terminal_tail_persisted_valid "$run" 2 12 12 < /dev/null; then fail 'absent timestamp proof accepted'; fi
  echo 'PASS: terminal replay rejects premature completion, duplicates and wrong run/generation/sequence/fence'

  # Exercise the real outage wait: pending disk evidence is insufficient if the DB
  # already ACKed the tail or applied a terminal. Advance only the fixture clock.
  terminal_tail_projection() { printf '%s\n' "$evidence"; }
  sleep() { SECONDS=$((SECONDS + 31)); }
  for db_variant in valid acknowledged premature missing; do
    rb_run_field() {
      case "$db_variant:$2" in
        valid:last_seq|premature:last_seq|missing:last_seq) echo 10;;
        acknowledged:last_seq) echo 12;;
        premature:status) echo completed;;
        *) echo running;;
      esac
    }
    if [ "$db_variant" = missing ]; then terminal_tail_projection() { echo '{}'; }; fi
    rc=0
    wait_terminal_tail_pending "$run" 2 10 || rc=$?
    if [ "$db_variant" = valid ]; then [ "$rc" = 0 ]; else [ "$rc" != 0 ]; fi
  done
  echo 'PASS: pending tail wait rejects already-acknowledged trace, premature terminal and missing disk proof'
)
(
  fixture
  run="$RUN_B1"
  snapshot_variant=valid
  apiget() {
    if [[ "$1" == */messages ]]; then
      if [ "$snapshot_variant" = mismatch ]; then echo '{"messages":[{"seq":11}]}'; else echo '{"messages":[{"seq":10}]}'; fi
    elif [ "$snapshot_variant" = premature ]; then echo '{"run":{"status":"completed"}}'
    else echo '{"run":{"status":"running"}}'
    fi
  }
  rb_run_field() { echo 10; }
  db_psql() { if [ "$snapshot_variant" = preexisting ]; then echo 1; else echo 0; fi; }
  for snapshot_variant in valid premature mismatch preexisting; do
    rc=0
    seq="$(snapshot_terminal_tail "$run" 2)" || rc=$?
    if [ "$snapshot_variant" = valid ]; then [ "$rc:$seq" = 0:10 ]; else [ "$rc" != 0 ]; fi
  done
  echo 'PASS: healthy barrier snapshot rejects premature completion, DB/API disagreement and a pre-cut tail'
)
# Evaluate the actual case wiring: each case must carry its healthy snapshot into the
# outage proof, retain that proof before restart, and verify persistence after recovery.
for selected in 3 4 5; do
  (
    fixture
    configure_outbox_api_alias
    # shellcheck disable=SC2034  # evaluated case commands consume these ids
    RUN_B2="$RUN_B1" RUN_B3="$RUN_B1"
    SNAPSHOT_DONE=0 PENDING_DONE=0
    make_outbox_run terminal
    snapshot_terminal_tail() {
      [ "$API_AVAILABLE" = 1 ] || fail 'snapshot after cut'
      echo 10
    }
    api_cut_original="$(declare -f api_cut)"
    eval "${api_cut_original/api_cut ()/cut_with_alias ()}"
    api_cut() { SNAPSHOT_DONE=1; cut_with_alias; }
    wait_terminal_tail_pending() {
      [ "$API_AVAILABLE:$BARRIER_RELEASED:$3" = 0:1:10 ] || fail 'pending proof outside exact cut'
      PENDING_DONE=1
    }
    compose_with_proof() {
      if [ "$1" = restart ]; then [ "$PENDING_DONE" = 1 ] || fail 'restart without pending message proof'; fi
      fake_compose "$@"
    }
    COMPOSE=(compose_with_proof)
    assert_terminal_tail_replayed() {
      [ "$SNAPSHOT_DONE:$PENDING_DONE:$API_AVAILABLE" = 1:1:1 ] || fail 'replay missing post-cut proof'
    }
    eval "$(awk -v selected="$selected" '
      /^say "CASE [0-9]+:/ { inside=index($0,"CASE " selected ":")>0 }
      inside && (/^PRE_B[123]=/ || /^api_cut$/ || /^release_outbox_cut_barrier/ ||
        /^terminal_lost_b[123]=/ || /^tail_pending_b[123]=/ || /^api_back$/ ||
        /^\[ "\$tail_pending_b[123]"/ || /^assert_terminal_tail_replayed/ ||
        index($0,"\"${COMPOSE[@]}\" restart agent")==1) { print }
    ' "$PHASE")"
    [ "$PENDING_DONE" = 1 ]
  )
  echo "PASS: real CASE $selected wires the healthy snapshot, pending message fence and persisted-tail proof"
done
echo 'cases=55 passed=55'
