#!/usr/bin/env bash
# Hermetic behavioural test for e2e/driver.sh (PRD #966 M2), the shape of
# e2e/reclaim-leaked-e2e.test.sh: fake helper FUNCTIONS + fixture phase files,
# no stack, no docker, no network. Each case runs the REAL driver in standalone
# Bash that defines the fakes and sources driver.sh AT TOP LEVEL (never inside a
# function — case 2 is exactly why that matters), then asserts on
# $RUNROOT/results.tsv, junit.xml and the case log.
#
# Every case is one you can watch go RED under a named driver mutation; the PR
# records the red output. Prints per-case PASS:/FAIL: lines and a final
# `cases=N passed=N` tally (mandatory — a zero-case run must not read green).
#
# Assertions read results.tsv with `awk -F'\t'` on exact fields (not tab-in-regex
# patterns): this host's grep is ugrep, so keeping field logic in awk is robust.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRIVER="$REPO_ROOT/e2e/driver.sh"
[ -f "$DRIVER" ] || { echo "driver.sh not found at $DRIVER" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'if [ "${DRIVER_KEEP_TMP:-0}" = 1 ]; then echo "test evidence: $TMP"; else rm -rf "$TMP"; fi' EXIT

# --- the fakes the driver depends on (sourced INSIDE each case's subshell) ----
# say/pass mirror lib.sh; fail mirrors it EXACTLY (ANSI + exit 1) so the driver's
# last-FAIL-line message extraction is exercised for real. db_psql/apipost*/
# wait_status are config-driven via FAKE_* env the parent sets per case.
FAKES="$TMP/fakes.sh"
cat > "$FAKES" <<'FAKES_EOF'
# shellcheck shell=bash
say()  { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

# db_psql: a SELECT of non-terminal runs echoes FAKE_DB_IDS (one id per line);
# any UPDATE (the forced-cancel fallback) is logged to FAKE_DB_LOG; the on-red
# artifact-capture SELECTs echo recognizable markers so a captured runs.txt/
# run-counts.txt has content the test can assert on.
db_psql() {
  case "$1" in
    *"status not in"*)
      # FUSED, faithfully mimicking real db_psql's `tr -d '\r\n'`: multiple rows come
      # back as ONE concatenated token with no separator (e.g. "r1r2"). This is the
      # buggy behaviour the driver must NOT use for the leak sweep — reverting the sweep
      # to db_psql (the mutation) reddens the two-leak case because both ids fuse into one.
      # shellcheck disable=SC2086  # deliberate split of the space-separated fixture
      printf '%s' ${FAKE_DB_IDS:-} ;;
    *"update runs"*)
      printf '%s\n' "$1" >> "${FAKE_DB_LOG:-/dev/null}" ;;
    *"from runs order by"*)
      printf 'FAKE_DB_RUNS r1 admin running issue\n' ;;
    *"trigger_source"*)
      printf %s "${FAKE_DB_WHAT:-}" ;;
    *"group by status"*)
      printf 'FAKE_DB_COUNTS running issue 1\n' ;;
  esac
}
# The fake clock is persisted because DB/curl execute in command substitutions.
date() { if [ "$*" = +%s ]; then cat "$FAKE_CLOCK"; else command date "$@"; fi; }
sleep() { echo $(( $(cat "$FAKE_CLOCK") + 30 )) > "$FAKE_CLOCK"; }
db_psql_rows() {
  local tick; tick="$(cat "$FAKE_CLOCK")"
  printf '%s timeout=%s\n' "$1" "${2:-none}" >> "$FAKE_READ_LOG"
  case "$1" in
    'SELECT id FROM users'*)
      echo baseline > "$FAKE_STAGE"
      [ "${FAKE_OWNER:-ok}" != error ] || return 2
      [ "${FAKE_OWNER:-ok}" != missing ] || return 0
      echo 00000000-0000-0000-0000-000000000001 ;;
    *'state, final_disposition, release_evidence'*)
      printf '%s\n' "${FAKE_HISTORY:-hold release evidence unavailable}" ;;
    *'FROM recovery_custody_holds'*)
      if [ "$(cat "$FAKE_STAGE")" = baseline ]; then
        echo boundary > "$FAKE_STAGE"
        [ "${FAKE_BASE_ERROR:-0}" = 0 ] || return 2
        [ -z "${FAKE_BASE_HOLDS:-}" ] || printf '%s\n' "$FAKE_BASE_HOLDS"
      else
        [ "${FAKE_HOLD_ERROR:-0}" = 0 ] || return 2
        if [ "$tick" -lt "${FAKE_SETTLE:-999}" ]; then
          [ -z "${FAKE_HOLDS:-}" ] || printf '%s\n' "$FAKE_HOLDS"
        fi
      fi ;;
    *'FROM worker_active_runs'*)
      if [ "${FAKE_DB_STALL:-0}" = 1 ]; then
        echo $((tick + $2)) > "$FAKE_CLOCK"; echo 'stalled psql' >&2; return 124
      fi
      [ "${FAKE_WORKER_ERROR:-0}" = 0 ] || return 2
      if [ "$tick" -lt "${FAKE_WORKER_SETTLE:-${FAKE_SETTLE:-999}}" ]; then
        [ -z "${FAKE_WORKERS:-}" ] || printf '%s\n' "$FAKE_WORKERS"
      fi ;;
    *"status not in"*)
      if [ "$tick" -lt "${FAKE_RUN_SETTLE:-${FAKE_SETTLE:-999}}" ]; then
        # shellcheck disable=SC2086
        [ -z "${FAKE_DB_IDS:-}" ] || printf '%s\n' $FAKE_DB_IDS
      fi
      if [ "$tick" -ge 30 ] && [ "$tick" -lt 60 ] && [ -n "${FAKE_QUEUED:-}" ]; then printf '%s\n' "$FAKE_QUEUED"; fi ;;
    *) db_psql "$1" ;;
  esac
  return 0
}
# Model an unavailable limiter without replacing the rest of PATH.
command() {
  if [ "${FAKE_NO_LIMITER:-0}" = 1 ] && [ "${1:-}" = -v ] && { [ "${2:-}" = timeout ] || [ "${2:-}" = gtimeout ]; }; then return 1; fi
  builtin command "$@"
}
boundary_cancel() {
  local code
  code="$(apipost_code "$1")"
  printf '%s' "$code" > "$3.http"
  printf 'response body' > "$3.body"; : > "$3.stderr"
  printf '%s\n' "${FAKE_TRANSPORT:-0}" > "$3.exit"
  if [ "${FAKE_CANCEL_STALL:-0}" = 1 ]; then echo $(( $(cat "$FAKE_CLOCK") + $2 )) > "$FAKE_CLOCK"; return 28; fi
  [ "${FAKE_TRANSPORT:-0}" = 0 ] && [[ "$code" = 2* ]]
}
apipost() { printf 'apipost %s\n' "$1" >> "${FAKE_API_LOG:-/dev/null}"; printf '{}'; }
# apipost_code performs the cancel POST AND returns the HTTP status (FAKE_CANCEL_CODE).
apipost_code() { printf 'cancel %s\n' "$1" >> "${FAKE_API_LOG:-/dev/null}"; printf '%s' "${FAKE_CANCEL_CODE:-200}"; }
wait_status() { :; }
# fake_docker stands in for the `COMPOSE` array the driver uses ONLY in on-red
# artifact capture: `docker compose logs …` (echo a marker so the captured svc.log
# has content) and `docker compose ps …` (print nothing, i.e. no service running, so
# 00-preflight's leg 4 takes the deferred-note path). COMPOSE=(fake_docker) is set in
# the parent test shell and serialized by run_driver's bootstrap.
fake_docker() {
  local n
  case "$1" in
    logs)
      # When FAKE_DOCKER_SEQ names a file, stamp a monotonic call number into each log
      # line so a test can tell a FIRST capture from a later OVERWRITING one (#990,
      # case17). Unset (every other case) -> byte-identical to the original output.
      if [ -n "${FAKE_DOCKER_SEQ:-}" ]; then
        n=$(( $(cat "$FAKE_DOCKER_SEQ" 2>/dev/null || echo 0) + 1 ))
        printf '%s' "$n" > "$FAKE_DOCKER_SEQ"
        printf 'FAKE_DOCKER_LOGS seq=%s %s\n' "$n" "$*"
      else
        printf 'FAKE_DOCKER_LOGS %s\n' "$*"
      fi ;;
    ps)   ;;   # no running services
    *)    ;;
  esac
}
FAKES_EOF

# --- fixture writer ----------------------------------------------------------
# mkphase PATH TITLE CRITICAL REQUIRES PROVIDES HANDOFF [RACESENS]  <<'BODY' ... BODY
# Header via printf (needs the arg values); body via the caller's QUOTED heredoc
# on stdin so `$FOO` in a body stays literal until the phase runs. The optional 7th
# arg emits `# race-sensitive: <val>` (default: line omitted).
mkphase() {
  local path="$1" title="$2" crit="$3" req="$4" prov="$5" hand="$6" rs="${7:-}" slug
  slug="$(basename "$path" .sh | sed 's/^[0-9][0-9]-//')"
  {
    printf '# shellcheck shell=bash\n'
    printf '# phase:    %s\n' "$slug"
    printf '# title:    %s\n' "$title"
    printf '# critical: %s\n' "$crit"
    printf '# lane:     gitlab\n'
    printf '# executor: any\n'
    [ -n "$rs" ] && printf '# race-sensitive: %s\n' "$rs"
    printf '# requires: %s\n' "$req"
    printf '# provides: %s\n' "$prov"
    printf '# handoff:  %s\n' "$hand"
    printf '# mutates:  -\n'
    printf '# restores: -\n'
  } > "$path"
  cat >> "$path"
}

# --- results.tsv readers (exact-field via awk, so ugrep quirks never apply) ---
TSV=""
has_row() { awk -F'\t' -v s="$1" -v st="$2" '$1==s && $2==st{f=1} END{exit !f}' "$TSV"; }
any_status() { awk -F'\t' -v st="$1" '$2==st{f=1} END{exit !f}' "$TSV"; }
row_msg() { awk -F'\t' -v s="$1" -v st="$2" '$1==s && $2==st{print $4; exit}' "$TSV"; }

# --- case harness ------------------------------------------------------------
cases=0; passed=0; cur=""; case_fail=0
begin() { cur="$1"; case_fail=0
  RUNROOT="$TMP/run-$2"; PHASES_DIR="$TMP/phases-$2"; ENVFILE="$TMP/env-$2"
  FAKE_API_LOG="$TMP/api-$2.log"; FAKE_DB_LOG="$TMP/db-$2.log"; TSV="$RUNROOT/results.tsv"
  rm -rf "$RUNROOT" "$PHASES_DIR"; mkdir -p "$RUNROOT" "$PHASES_DIR"
  : > "$ENVFILE"; : > "$FAKE_API_LOG"; : > "$FAKE_DB_LOG"
  FAKE_CLOCK="$TMP/clock-$2"; echo 0 > "$FAKE_CLOCK"
  FAKE_STAGE="$TMP/stage-$2"; echo baseline > "$FAKE_STAGE"
  FAKE_READ_LOG="$TMP/reads-$2"; : > "$FAKE_READ_LOG"
  unset FAKE_OWNER FAKE_BASE_ERROR FAKE_HOLD_ERROR FAKE_WORKER_ERROR FAKE_BASE_HOLDS FAKE_HOLDS FAKE_WORKERS FAKE_SETTLE FAKE_WORKER_SETTLE FAKE_RUN_SETTLE FAKE_QUEUED FAKE_CANCEL_STALL FAKE_TRANSPORT FAKE_DB_STALL FAKE_HISTORY 2>/dev/null || true
  export FAKE_OWNER FAKE_BASE_ERROR FAKE_HOLD_ERROR FAKE_WORKER_ERROR FAKE_BASE_HOLDS FAKE_HOLDS FAKE_WORKERS FAKE_SETTLE FAKE_WORKER_SETTLE FAKE_RUN_SETTLE FAKE_QUEUED FAKE_CANCEL_STALL FAKE_TRANSPORT FAKE_BODY_MARK FAKE_BODY_FAIL FAKE_DB_STALL
  unset E2E_ONLY E2E_SKIP E2E_STRICT_LEAKS E2E_FAULT_PHASE FAKE_DB_IDS FAKE_DB_WHAT FAKE_CANCEL_CODE FAKE_DOCKER_SEQ 2>/dev/null || true
}
bad() { printf '  - %s\n' "$1"; case_fail=1; }
contains() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }
end() {
  cases=$((cases + 1))
  if [ "$case_fail" -eq 0 ]; then passed=$((passed + 1)); printf 'PASS: %s\n' "$cur"
  else printf 'FAIL: %s\n' "$cur"; fi
}
# run_driver launches standalone Bash so source runs outside function scope.
# A subshell inside this function would retain its dynamic scope, masking stale
# globals when phase.env.next declares an unset scalar.
LOG=""; RC=0
run_driver() {
  LOG="$TMP/log-$1"
  # Preserve arrays and unexported fixture knobs; exported globals are inherited.
  declare -p RUNROOT PHASES_DIR ENVFILE COMPOSE > "$TMP/bootstrap"
  while IFS= read -r knob; do
    declare -p "$knob" >> "$TMP/bootstrap"
  done < <(compgen -A variable FAKE_; compgen -A variable E2E_ || true)
  set +e
  bash -c 'set -euo pipefail; source "$1"; source "$2"; source "$3"' \
    bash "$TMP/bootstrap" "$FAKES" "$DRIVER" > "$LOG" 2>&1
  RC=$?
  set -e
  if [ -f "$TSV" ]; then
    awk -F'\t' 'NF!=4{exit 1}' "$TSV" || bad 'results.tsv no longer has four columns'
  fi
}
# Shared globals inherited by the standalone driver shell; per-case E2E_*/FAKE_*
# knobs are serialized by run_driver even when they are not exported.
export ROOT="$REPO_ROOT" FORGE=gitlab EXECUTOR=stub ADMIN_EMAIL=fixture@example.com
# COMPOSE is an array, serialized into the standalone shell's bootstrap.
# The driver uses it only for on-red artifact capture.
export DRIVER_FAKES="$FAKES"
cat > "$TMP/compose" <<'COMPOSE_EOF'
source "$DRIVER_FAKES"
fake_docker "$@"
COMPOSE_EOF
# shellcheck disable=SC2034  # consumed by the sourced driver
COMPOSE=(bash "$TMP/compose")

# ============================================================================
# Case 1 — a body `false` then `pass ok` is recorded FAIL.
# Mutation: per-phase shape -> `( source "$f" ) || rc=$?` makes it read PASS.
begin "case1: body false is FAIL" 1
mkphase "$PHASES_DIR/10-falsephase.sh" "false phase" no "" "" "" <<'BODY'
false
pass ok
BODY
run_driver 1
has_row falsephase FAIL || bad "falsephase not recorded FAIL"
[ "$RC" -eq 1 ] || bad "exit code should be 1 on a FAIL (got $RC)"
end

# ============================================================================
# Case 2 — provides FOO round-trips to a consumer. Both PASS.
# Mutation: move `source phase.env.next` into a function -> consumer sees FOO unset.
begin "case2: provides round-trip" 2
mkphase "$PHASES_DIR/10-provideA.sh" "provide A" no "" FOO "" <<'BODY'
FOO=bar
pass "set FOO"
BODY
mkphase "$PHASES_DIR/11-consumeB.sh" "consume B" no FOO "" "" <<'BODY'
[ "$FOO" = bar ] || fail "FOO was not bar (got '${FOO:-}')"
pass "saw FOO=bar"
BODY
run_driver 2
has_row provideA PASS || bad "provideA not PASS"
has_row consumeB PASS || bad "consumeB not PASS (provides did not cross)"
[ ! -d "$RUNROOT/artifacts" ] || bad "clean run created artifacts"
[ "$RC" -eq 0 ] || bad "exit code should be 0 (got $RC)"
end

# ============================================================================
# Case 3 — a phase that fails before setting its provides makes the consumer's
# requires FAIL, naming BOTH the producer and the consumer.
# Mutation: drop the producer from the requires-miss message.
begin "case3: requires-miss names both phases" 3
mkphase "$PHASES_DIR/10-failA.sh" "fail A" no "" FOO "" <<'BODY'
fail "A boom before setting FOO"
BODY
mkphase "$PHASES_DIR/11-needB.sh" "need B" no FOO "" "" <<'BODY'
pass "should never run"
BODY
run_driver 3
has_row needB FAIL || bad "needB not recorded FAIL"
m="$(row_msg needB FAIL)"
contains "$m" needB || bad "message does not name needB: $m"
contains "$m" failA || bad "message does not name producer failA: $m"
end

# ============================================================================
# Case 4 — an `env:POLL=2s` requirement passes with the line in ENVFILE and
# FAILs without it, naming the phase.
# Mutation: skip the env: validation -> the without-line run reads PASS.
begin "case4a: env token present -> PASS" 4a
printf 'POLL=2s\n' > "$ENVFILE"
mkphase "$PHASES_DIR/10-needenv.sh" "need env" no "env:POLL=2s" "" "" <<'BODY'
pass ok
BODY
run_driver 4a
has_row needenv PASS || bad "needenv not PASS with POLL=2s present"
end

begin "case4b: env token absent -> FAIL" 4b
: > "$ENVFILE"   # no POLL line
mkphase "$PHASES_DIR/10-needenv.sh" "need env" no "env:POLL=2s" "" "" <<'BODY'
pass ok
BODY
run_driver 4b
has_row needenv FAIL || bad "needenv not FAIL with POLL missing"
m="$(row_msg needenv FAIL)"
contains "$m" needenv || bad "message does not name the phase: $m"
contains "$m" "env:POLL=2s" || bad "message does not name the missing token: $m"
end

# ============================================================================
# Case 5 — critical FAIL stops (rest SKIP); a non-critical FAIL lets the suite
# continue and a later requires-miss that intersects the failed provides is
# tagged suspect_cascade.
# Mutation A: drop `CRITICAL_FAILED=1` -> the after-phase runs instead of SKIP.
begin "case5a: critical FAIL stops the suite" 5a
mkphase "$PHASES_DIR/10-crit.sh" "critical boom" yes "" "" "" <<'BODY'
fail "crit boom"
BODY
mkphase "$PHASES_DIR/11-after.sh" "after critical" no "" "" "" <<'BODY'
pass "should be skipped"
BODY
run_driver 5a
has_row crit FAIL || bad "crit not FAIL"
has_row after SKIP || bad "after not SKIP"
contains "$(row_msg after SKIP)" "after critical failure" || bad "after SKIP reason wrong"
end

# Mutation B: drop the suspect_cascade tagging -> cascade line lacks the tag.
begin "case5b: non-critical continues + suspect_cascade" 5b
mkphase "$PHASES_DIR/10-prodfail.sh" "producer fails" no "" FOO "" <<'BODY'
fail "prod boom"
BODY
mkphase "$PHASES_DIR/11-mid.sh" "unrelated passes" no "" "" "" <<'BODY'
pass "suite continued"
BODY
mkphase "$PHASES_DIR/12-cascade.sh" "cascade victim" no FOO "" "" <<'BODY'
pass "never runs"
BODY
run_driver 5b
has_row prodfail FAIL || bad "prodfail not FAIL"
has_row mid PASS || bad "mid not PASS (suite did not continue)"
has_row cascade FAIL || bad "cascade not FAIL"
contains "$(row_msg cascade FAIL)" suspect_cascade || bad "cascade not tagged suspect_cascade"
end

# ============================================================================
# Case 6 — undeclared runs must settle; cancellation rejection is a hard
# boundary failure and never writes SQL status.
begin "case6a: undeclared run leaks and is cancelled" 6a
export FAKE_DB_IDS="00000000-0000-0000-0000-000000000014" FAKE_CANCEL_CODE=200 FAKE_DB_WHAT="mr_rework via poller, status queued, age 3s"
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 6a
has_row leaky FAIL || bad "no final FAIL row for leaky"
contains "$(row_msg leaky FAIL)" 00000000-0000-0000-0000-000000000014 || bad "boundary message does not name 00000000-0000-0000-0000-000000000014"

contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/00000000-0000-0000-0000-000000000014/inputs" || bad "cancel was not attempted via the API"
end

begin "case6b: handoff-declared id is not a leak" 6b
export FAKE_DB_IDS="00000000-0000-0000-0000-000000000015" FAKE_CANCEL_CODE=200
mkphase "$PHASES_DIR/10-held.sh" "hands a run forward" no "" RUN_B RUN_B <<'BODY'
RUN_B=00000000-0000-0000-0000-000000000015
pass "declared handoff RUN_B"
BODY
run_driver 6b
any_status LEAK && bad "handoff id was wrongly recorded as a LEAK"
[ -s "$FAKE_API_LOG" ] && bad "a cancel was attempted for a declared handoff id"
has_row held PASS || bad "held phase not PASS"
end

begin "case6c: refused API cancel fails without SQL fallback" 6c
export FAKE_DB_IDS="00000000-0000-0000-0000-000000000014" FAKE_CANCEL_CODE=403
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 6c
has_row leaky FAIL || bad "no final FAIL row for leaky"
contains "$(row_msg leaky FAIL)" "cancel rejected" || bad "missing rejected cancel failure"
[ ! -s "$FAKE_DB_LOG" ] || bad "SQL fallback ran"
end

# ============================================================================
# Case 7 — E2E_ONLY/E2E_SKIP honour critical:.
# Mutation: apply selection to critical phases too -> the critical phase is
# deselected instead of always running.
begin "case7a: E2E_ONLY still runs critical" 7a
export E2E_ONLY="alpha"
mkphase "$PHASES_DIR/10-crit.sh"  "critical always" yes "" "" "" <<'BODY'
pass "critical ran"
BODY
mkphase "$PHASES_DIR/11-alpha.sh" "alpha selected"  no  "" "" "" <<'BODY'
pass "alpha ran"
BODY
mkphase "$PHASES_DIR/12-beta.sh"  "beta deselected" no  "" "" "" <<'BODY'
pass "beta ran"
BODY
run_driver 7a
has_row crit PASS  || bad "critical did not run under E2E_ONLY"
has_row alpha PASS || bad "alpha (selected) did not run"
has_row beta SKIP  || bad "beta (unselected) was not SKIP"
end

begin "case7b: E2E_SKIP spares critical" 7b
export E2E_SKIP="alpha,crit"
mkphase "$PHASES_DIR/10-crit.sh"  "critical always" yes "" "" "" <<'BODY'
pass "critical ran"
BODY
mkphase "$PHASES_DIR/11-alpha.sh" "alpha skipped"   no  "" "" "" <<'BODY'
pass "alpha ran"
BODY
mkphase "$PHASES_DIR/12-beta.sh"  "beta runs"       no  "" "" "" <<'BODY'
pass "beta ran"
BODY
run_driver 7b
has_row crit PASS  || bad "critical was skipped by E2E_SKIP (must not be)"
has_row alpha SKIP || bad "alpha was not skipped by E2E_SKIP"
has_row beta PASS  || bad "beta (not skipped) did not run"
end

# ============================================================================
# Case 8 — E2E_FAULT_PHASE injects a verbatim fault; junit.xml is well-formed and
# results.tsv has 4 columns per row.
# Mutation: drop the fault line -> the phase PASSes / message is absent.
begin "case8: injected fault + artifact shapes" 8
export E2E_FAULT_PHASE="faultme"
mkphase "$PHASES_DIR/10-faultme.sh" "fault target" no "" "" "" <<'BODY'
pass "should not be reached"
BODY
# A second phase whose FAIL message carries the five XML metacharacters, to prove
# junit escaping (& < > ") keeps the document well-formed.
mkphase "$PHASES_DIR/11-xmlbad.sh" "xml & <hostile> \"msg\"" no "" "" "" <<'BODY'
fail "x & y < z > w \"q\""
BODY
run_driver 8
has_row faultme FAIL || bad "faultme not FAIL under E2E_FAULT_PHASE"
[ "$(row_msg faultme FAIL)" = "injected fault: faultme" ] || bad "fault message not verbatim: $(row_msg faultme FAIL)"
python3 -c 'import sys,xml.etree.ElementTree as T; T.parse(sys.argv[1])' "$RUNROOT/junit.xml" \
  || bad "junit.xml is not well-formed XML"
contains "$(cat "$RUNROOT/junit.xml")" "&amp;" || bad "junit did not XML-escape '&'"
contains "$(cat "$RUNROOT/junit.xml")" "&lt;"  || bad "junit did not XML-escape '<'"
contains "$(cat "$RUNROOT/junit.xml")" "&quot;" || bad "junit did not XML-escape '\"'"
awk -F'\t' 'NF!=4{bad=1} END{exit bad+0}' "$TSV" || bad "results.tsv has a row without exactly 4 columns"
[ "$RC" -eq 1 ] || bad "exit code should be 1 on the injected fault (got $RC)"
end

# ============================================================================
# Case 9 (PRD #966 M3) — on-red artifact capture. A FAILing phase populates
# $RUNROOT/artifacts/<NN-slug>/ (phase.log + per-service docker logs + the db
# enumerations) and writes the .keep-rundir sentinel.
# Mutation: remove the `_capture_artifacts` call in the FAIL branch -> the dir and
# the sentinel are absent.
begin "case9: artifact capture on FAIL" 9
mkphase "$PHASES_DIR/10-failcap.sh" "fail with capture" no "" "" "" <<'BODY'
fail "boom in failcap"
BODY
run_driver 9
adir="$RUNROOT/artifacts/10-failcap"
[ -d "$adir" ] || bad "artifacts dir $adir was not created on FAIL"
[ -f "$adir/phase.log" ] || bad "phase.log not captured"
contains "$(cat "$adir/phase.log" 2>/dev/null)" "boom in failcap" || bad "phase.log missing the phase output"
[ -f "$adir/api.log" ] || bad "api.log (docker logs) not captured"
contains "$(cat "$adir/api.log" 2>/dev/null)" "FAKE_DOCKER_LOGS" || bad "api.log missing the fake-docker output"
[ -f "$adir/runs.txt" ] || bad "runs.txt not captured"
contains "$(cat "$adir/runs.txt" 2>/dev/null)" "FAKE_DB_RUNS" || bad "runs.txt missing the fake-db output"
[ -f "$adir/run-counts.txt" ] || bad "run-counts.txt not captured"
[ -f "$RUNROOT/.keep-rundir" ] || bad ".keep-rundir sentinel not written on FAIL"
end

# ============================================================================
# Case 10 — no capture on an all-GREEN run: neither artifacts/ nor .keep-rundir.
# Mutation: capture on every phase (not just FAIL) -> artifacts/ appears green.
begin "case10: no capture on GREEN" 10
mkphase "$PHASES_DIR/10-greenA.sh" "green A" no "" "" "" <<'BODY'
pass "all good A"
BODY
mkphase "$PHASES_DIR/11-greenB.sh" "green B" no "" "" "" <<'BODY'
pass "all good B"
BODY
run_driver 10
[ "$RC" -eq 0 ] || bad "green run should exit 0 (got $RC)"
[ ! -d "$RUNROOT/artifacts" ] || bad "artifacts/ was created on an all-green run"
[ ! -e "$RUNROOT/.keep-rundir" ] || bad ".keep-rundir written on an all-green run"
end

# ============================================================================
# Case 11 — empty-FAIL-message fallback. A phase that dies on a BARE `false` (no
# fail() call) has no `FAIL ` line, so results.tsv gets the "(no fail message; last
# output: …)" fallback rather than an empty message.
# Mutation: drop the fallback -> the message column is empty.
begin "case11: empty-FAIL-message fallback" 11
mkphase "$PHASES_DIR/10-bare.sh" "bare false" no "" "" "" <<'BODY'
false
BODY
run_driver 11
has_row bare FAIL || bad "bare-false phase not recorded FAIL"
m="$(row_msg bare FAIL)"
[ -n "$m" ] || bad "FAIL message is empty (fallback did not fire)"
contains "$m" "no fail message" || bad "message is not the fallback: $m"
end

# ============================================================================
# Case 12 — race-sensitive annotation. A `race-sensitive: yes` phase that FAILs has
# "possible race" appended to its message; a non-race phase does not.
# Mutation: drop the annotation -> the race phase's message lacks "possible race".
begin "case12: race-sensitive annotation" 12
mkphase "$PHASES_DIR/10-racy.sh" "racy phase" no "" "" "" yes <<'BODY'
fail "timing assertion missed"
BODY
mkphase "$PHASES_DIR/11-calm.sh" "calm phase" no "" "" "" <<'BODY'
fail "plain failure"
BODY
run_driver 12
contains "$(row_msg racy FAIL)" "possible race" || bad "race-sensitive FAIL not annotated: $(row_msg racy FAIL)"
contains "$(row_msg calm FAIL)" "possible race" && bad "non-race FAIL wrongly annotated: $(row_msg calm FAIL)"
end

# ============================================================================
# Case 13 — the REAL e2e/phases/00-preflight.sh against a fixture $RUNROOT (real git,
# no docker). A shared bare + safe.directory gitconfig PASSES; re-initing one bare
# WITHOUT --shared (the E2E_FAULT_PREFLIGHT simulation) FAILs naming
# core.sharedRepository. fake_docker's `ps` returns no agent, so leg 4 defers.
# Mutation: weaken 00-preflight's assertion #1 -> the unshared run reads PASS.
begin "case13: 00-preflight assertion + positive control" 13
PF="$TMP/preflight-13"
rm -rf "$PF"; mkdir -p "$PF/fakeremote" "$PF/agent-gitconfig" "$PF/logs"
for r in repo repo2; do
  git init --bare -q --shared=0777 "$PF/fakeremote/$r.git"
  git -C "$PF/fakeremote/$r.git" symbolic-ref HEAD refs/heads/main
  git -C "$PF" clone -q "$PF/fakeremote/$r.git" ".seed-$r" 2>/dev/null
  git -C "$PF/.seed-$r" checkout -q -b main
  printf 'seed %s\n' "$r" > "$PF/.seed-$r/README.md"
  git -C "$PF/.seed-$r" add README.md
  git -C "$PF/.seed-$r" -c user.name=seed -c user.email=seed@uzi.e2e -c commit.gpgsign=false commit -q -m seed
  git -C "$PF/.seed-$r" push -q origin main
  rm -rf "$PF/.seed-$r"
done
printf '[safe]\n\tdirectory = *\n' > "$PF/agent-gitconfig/gitconfig"
PFRC=0; PFLOG=""
run_preflight() {
  PFLOG="$TMP/pflog-$RANDOM"
  set +e
  # shellcheck disable=SC1090
  ( set -euo pipefail; source "$FAKES"; RUNROOT="$1"; source "$REPO_ROOT/e2e/phases/00-preflight.sh" ) > "$PFLOG" 2>&1
  PFRC=$?
  set -e
}
run_preflight "$PF"
[ "$PFRC" -eq 0 ] || bad "preflight FAILED on a well-formed fixture (rc=$PFRC): $(cat "$PFLOG")"
# Break repo.git: re-init WITHOUT --shared (core.sharedRepository becomes unset).
rm -rf "$PF/fakeremote/repo.git"
git init --bare -q "$PF/fakeremote/repo.git"
git -C "$PF/fakeremote/repo.git" symbolic-ref HEAD refs/heads/main
run_preflight "$PF"
[ "$PFRC" -ne 0 ] || bad "preflight PASSED on an unshared bare (must FAIL)"
contains "$(cat "$PFLOG")" "core.sharedRepository" || bad "preflight FAIL did not name core.sharedRepository: $(cat "$PFLOG")"
end

# ============================================================================
# Case 14 — a phase that declares an `env:KEY=VALUE` provides token (alongside a
# shell-var provides) is recorded PASS, and the shell-var provides still crosses.
# An env: token is an ENVFILE fact (for requires: producer lookup), NOT a shell
# var; the provides round-trip must SKIP it, not `declare -p "env:KEY=VALUE"`
# (which errors "not found" and, under the phase subshell's set -e, would redden a
# phase whose body passed — the pre-existing PRD #966 mr-close / mr-rework crash).
# Mutation: drop the `case env:*) continue` skip in driver.sh's provides loop ->
# provEnv reddens with a `declare: env:...: not found`.
begin "case14: env: provides token does not crash the phase" 14
mkphase "$PHASES_DIR/10-provEnv.sh" "provide env + var" no "" "BAZ env:POLL=2s" "" <<'BODY'
BAZ=qux
pass "set BAZ and declared env:POLL=2s"
BODY
mkphase "$PHASES_DIR/11-consumeEnv.sh" "consume env + var" no "BAZ env:POLL=2s" "" "" <<'BODY'
[ "$BAZ" = qux ] || fail "BAZ was not qux (got '${BAZ:-}')"
pass "saw BAZ=qux and env:POLL=2s satisfied"
BODY
printf 'POLL=2s\n' > "$ENVFILE"   # the env: token the provider declares must exist in ENVFILE
run_driver 14
has_row provEnv PASS || bad "provEnv not PASS (env: provides token crashed the round-trip)"
has_row consumeEnv PASS || bad "consumeEnv not PASS (shell-var provides did not cross, or env: requires unsatisfied)"
[ "$RC" -eq 0 ] || bad "exit code should be 0 (got $RC)"
end

# ============================================================================
# Case 15 — boundary failure still emits summary, rollcall and suite exit.
begin "case15: boundary FAIL writes summary and rollcall" 15
export FAKE_DB_IDS="00000000-0000-0000-0000-000000000012" FAKE_CANCEL_CODE=200
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 15
has_row leaky FAIL || bad "no final FAIL row for leaky"
[ -f "$RUNROOT/summary.md" ] || bad "summary.md was not written on a leaky run"
contains "$(cat "$LOG")" "E2E FAILED:" \
  || bad "roll-call did not run after a boundary failure (driver aborted at _write_summary)"
[ "$RC" -eq 1 ] || bad "boundary failure must exit 1 without strict mode"
end

# ============================================================================
# Case 16 — preserve two separate run IDs and cancel both, in one final row.
begin "case16: two run IDs produce two cancels and one final row" 16
export FAKE_DB_IDS="00000000-0000-0000-0000-000000000010 00000000-0000-0000-0000-000000000011" FAKE_CANCEL_CODE=200
mkphase "$PHASES_DIR/10-leaky.sh" "leaks two runs" no "" "" "" <<'BODY'
pass "left two runs behind"
BODY
run_driver 16
leak_n="$(awk -F'\t' '$1=="leaky" && $2=="FAIL"{n++} END{print n+0}' "$TSV")"
[ "$leak_n" -eq 1 ] || bad "expected one final FAIL row, got $leak_n (rows fused?)"
contains "$(cat "$TSV")" "00000000-0000-0000-0000-000000000010" || bad "boundary failures do not name 00000000-0000-0000-0000-000000000010 (rows fused into one token)"
contains "$(cat "$TSV")" "00000000-0000-0000-0000-000000000011" || bad "boundary failures do not name 00000000-0000-0000-0000-000000000011 (rows fused into one token)"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/00000000-0000-0000-0000-000000000010/inputs" || bad "00000000-0000-0000-0000-000000000010 not cancelled via the API"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/00000000-0000-0000-0000-000000000011/inputs" || bad "00000000-0000-0000-0000-000000000011 not cancelled via the API"
end

# ============================================================================
# Case 17 — the first body capture survives a second capture attempt after
# boundary failure. fake_docker stamps sequence numbers to detect overwrites.
begin "case17: body+boundary failure capture is not overwritten by quarantine" 17
export FAKE_DB_IDS="00000000-0000-0000-0000-000000000013" FAKE_CANCEL_CODE=200 FAKE_DOCKER_SEQ="$TMP/dseq-17"
rm -f "$FAKE_DOCKER_SEQ"
mkphase "$PHASES_DIR/10-failleak.sh" "fail and leak" no "" "" "" <<'BODY'
fail "boom and leak"
BODY
run_driver 17
adir="$RUNROOT/artifacts/10-failleak"
has_row failleak FAIL || bad "failleak not FAIL"
contains "$(row_msg failleak FAIL)" "boundary timeout" || bad "missing boundary failure evidence"
[ -f "$adir/api.log" ] || bad "api.log not captured"
contains "$(cat "$adir/api.log" 2>/dev/null)" "seq=1" \
  || bad "api.log is not the FAIL-path (first) capture: $(cat "$adir/api.log" 2>/dev/null)"
contains "$(cat "$adir/api.log" 2>/dev/null)" "seq=5" \
  && bad "api.log was OVERWRITTEN by the leak-sweep re-capture (#990): $(cat "$adir/api.log" 2>/dev/null)"
[ -f "$adir/boundary/1-runs.exit" ] || bad "body+boundary failure lost mirrored raw read"
[ -f "$adir/boundary/cancel-00000000-0000-0000-0000-000000000013.body" ] || bad "body+boundary failure lost cancel response"
[ "$(cat "$adir/boundary-copy.exit" 2>/dev/null)" = 0 ] || bad "boundary mirror failed"
end

# Boundary milestone regressions use authoritative DB sets, including before phase 13.
R=11111111-1111-1111-1111-111111111111
R2=22222222-2222-2222-2222-222222222222
W=33333333-3333-3333-3333-333333333333
H=44444444-4444-4444-4444-444444444444
H2=55555555-5555-5555-5555-555555555555
boundary_fixture() {
  mkphase "$PHASES_DIR/01-origin.sh" boundary yes "" "${1:-}" "${2:-}" <<'BODY'
[ "${FAKE_BODY_FAIL:-0}" = 0 ] || fail 'body evidence retained'
[ "${FAKE_BODY_MARK:-0}" = 0 ] || echo ran > "$RUNROOT/body-ran"
RUN_HAND=11111111-1111-1111-1111-111111111111
HOLD_HAND=44444444-4444-4444-4444-444444444444
pass body
BODY
}
boundary_fail() {
  run_driver "$1"
  has_row origin FAIL || bad 'origin must FAIL'
  contains "$(row_msg origin FAIL)" "$2" || bad "failure must name $2"
  [ "$RC" -eq 1 ] || bad 'boundary must fail suite even without strict leaks'
  [ "$(wc -l < "$TSV")" -eq 1 ] || bad 'expected one final phase row'
}
boundary_evidence() {
  local dir="$RUNROOT/boundary/01-origin"
  if has_row origin FAIL; then
    diff -r "$dir" "$RUNROOT/artifacts/01-origin/boundary" > "$TMP/mirror-diff" 2>&1 || bad "uploaded boundary mirror differs: $(cat "$TMP/mirror-diff")"
  fi
  for f in owner.stdout owner.stderr owner.exit baseline.stdout baseline.exit initial-runs initial-workers initial-holds final-runs final-workers final-holds boundary_wait_seconds; do
    [ -f "$dir/$f" ] || bad "missing boundary evidence $f"
  done
  contains "$(cat "$FAKE_READ_LOG")" 'SELECT worker_id, run_id, claim_generation FROM worker_active_runs' || bad 'missing actual worker read'
  contains "$(cat "$FAKE_READ_LOG")" 'SELECT id, run_id, generation FROM recovery_custody_holds WHERE user_id' || bad 'missing actual hold read'
}
begin 'boundary: cancelled terminal run remains worker-active' b1
FAKE_DB_IDS="$R"; FAKE_RUN_SETTLE=30; FAKE_WORKERS="$W|$R|3"
boundary_fixture
boundary_fail b1 "$R"
boundary_evidence
[ -f "$RUNROOT/boundary/01-origin/cancel-$R.body" ] || bad 'cancel body missing'
end

begin 'boundary: every undeclared open hold fails admission-independent' b2
FAKE_HOLDS="$H|$R|9"
boundary_fixture
boundary_fail b2 "$H"
boundary_evidence
[ -f "$RUNROOT/boundary/01-origin/hold-history.stdout" ] || bad 'hold history missing'
end

begin 'boundary: old hold release cannot mask replacement with same count' b3
FAKE_BASE_HOLDS="$H|$R|1"; FAKE_HOLDS="$H2|$R|2"
export FAKE_HISTORY="$H|$R|1|released|archive|archive-proof
$H2|$R|2|open||"
boundary_fixture
boundary_fail b3 "$H2"
contains "$(cat "$RUNROOT/boundary/01-origin/hold-history.stdout")" 'released|archive|archive-proof' || bad 'release evidence not retained'
contains "$(cat "$RUNROOT/boundary/01-origin/hold-history.stdout")" "$H2" || bad 'new hold history missing'
diff -r "$RUNROOT/boundary/01-origin" "$RUNROOT/artifacts/01-origin/boundary" || bad 'release evidence not mirrored'
end

begin 'boundary: already DB-terminal run must leave worker listing' b4
FAKE_WORKERS="$W|$R|3"
boundary_fixture
boundary_fail b4 "$R"
[ ! -s "$FAKE_API_LOG" ] || bad 'terminal worker run should not be cancelled'
end

begin 'boundary: rejected cancellation sticky with no SQL status fallback' b5
FAKE_DB_IDS="$R"; FAKE_CANCEL_CODE=403; FAKE_RUN_SETTLE=30
boundary_fixture
boundary_fail b5 'cancel rejected'
[ ! -s "$FAKE_DB_LOG" ] || bad 'SQL status write attempted'
end

for set in worker hold; do
  for failure in nonzero malformed arity blank generation uuid; do
    begin "boundary: $set $failure read fails closed" "b6-$set-$failure"
    row="$W|$R|2"
    case "$failure" in
      nonzero) if [ "$set" = worker ]; then FAKE_WORKER_ERROR=1; else FAKE_HOLD_ERROR=1; fi ;;
      malformed) row=garbage ;;
      arity) row="$row|extra" ;;
      blank) row=$'\n' ;;
      generation) row="$W|$R|two" ;;
      uuid) row="not-uuid|$R|1" ;;
    esac
    if [ "$set" = worker ]; then FAKE_WORKERS="$row"; else FAKE_HOLDS="$row"; fi
    boundary_fixture
    boundary_fail "b6-$set-$failure" unverifiable
    end
  done
done

for mode in run hold; do
  begin "boundary: declared $mode passes with real boundary evidence" "b7-$mode"
  if [ "$mode" = run ]; then
    FAKE_DB_IDS="$R"; FAKE_WORKERS="$W|$R|3"
    boundary_fixture RUN_HAND RUN_HAND
  else
    FAKE_HOLDS="$H|$R|3"
    boundary_fixture HOLD_HAND ""
    echo '# handoff-holds: HOLD_HAND' >> "$PHASES_DIR/01-origin.sh"
  fi
  run_driver "b7-$mode"
  has_row origin PASS || bad "declared $mode must PASS"
  [ "$RC" = 0 ] || bad "declared $mode exit must be zero"
  [ ! -s "$FAKE_API_LOG" ] || bad 'declared run cancelled'
  boundary_evidence
  contains "$(cat "$RUNROOT/summary.md")" boundary_wait_seconds || bad 'missing timing summary'
  end
done

begin 'boundary: contamination annotates later PASS FAIL SKIP and requires, not itself' b8
FAKE_WORKERS="$W|$R|3"; FAKE_WORKER_SETTLE=120
boundary_fixture
mkphase "$PHASES_DIR/02-later.sh" later no "" "" "" <<'BODY'
pass later
BODY
mkphase "$PHASES_DIR/03-broken.sh" broken no "" "" "" <<'BODY'
fail later
BODY
mkphase "$PHASES_DIR/04-needs.sh" needs no MISSING "" "" <<'BODY'
pass unreachable
BODY
mkphase "$PHASES_DIR/05-skip.sh" skipped no "" "" "" <<'BODY'
pass unreachable
BODY
E2E_SKIP=skip
run_driver b8
has_row later PASS || bad 'critical boundary-only failure skipped later body'
for pair in later:PASS broken:FAIL needs:FAIL skip:SKIP; do
  contains "$(row_msg "${pair%:*}" "${pair#*:}")" 'contaminated after: origin' || bad "missing contamination on $pair"
done
contains "$(row_msg origin FAIL)" 'contaminated after' && bad 'origin annotated after itself'
end

begin 'boundary: transient eventual settlement passes' transient
FAKE_DB_IDS="$R"; FAKE_WORKERS="$W|$R|1"; FAKE_HOLDS="$H|$R|1"; FAKE_SETTLE=30
boundary_fixture
run_driver transient
has_row origin PASS || bad 'eventual settlement must pass'
boundary_evidence
[ "$(cat "$RUNROOT/boundary/01-origin/boundary_wait_seconds")" = 30 ] || bad 'wait not measured'
[ ! -s "$RUNROOT/boundary/01-origin/final-workers" ] || bad 'missing final clean sample'
end

begin 'boundary: worker and holds consume one shared 120 second budget' shared
FAKE_WORKERS="$W|$R|1"; FAKE_WORKER_SETTLE=90; FAKE_HOLDS="$H|$R|2"; FAKE_SETTLE=150
boundary_fixture
boundary_fail shared "$H"
[ "$(cat "$FAKE_CLOCK")" = 120 ] || bad 'worker and holds had separate budgets'
end

begin 'boundary: new queued run discovered and cancelled while settling' queued
FAKE_WORKERS="$W|$R|1"; FAKE_SETTLE=60; FAKE_QUEUED="$R2"
boundary_fixture
run_driver queued
has_row origin PASS || bad 'queued run settlement should PASS'
contains "$(cat "$FAKE_API_LOG")" "$R2" || bad 'new queued run not cancelled'
end

for mode in missing error baseline-error baseline-malformed; do
  begin "boundary: $mode snapshot is sticky but body and boundary execute" "$mode"
  case "$mode" in
    missing|error) FAKE_OWNER="$mode" ;;
    baseline-error) FAKE_BASE_ERROR=1 ;;
    baseline-malformed) FAKE_BASE_HOLDS=garbage ;;
  esac
  FAKE_BODY_MARK=1
  boundary_fixture
  boundary_fail "$mode" unverifiable
  [ -f "$RUNROOT/body-ran" ] || bad 'unverifiable baseline prevented body'
  contains "$(cat "$FAKE_READ_LOG")" worker_active_runs || bad 'boundary skipped'
  unset FAKE_BODY_MARK
  end
done

for declaration in MISSING 'bad-name' RUN_HAND; do
  begin "boundary: malformed or undefined handoff $declaration" "declaration-$declaration"
  boundary_fixture "" "$declaration"
  if [ "$declaration" = RUN_HAND ]; then echo 'RUN_HAND=bad-value' >> "$PHASES_DIR/01-origin.sh"; fi
  boundary_fail "declaration-$declaration" declaration
  end
done

begin 'boundary: run declaration does not declare its hold' separate
FAKE_DB_IDS="$R"; FAKE_WORKERS="$W|$R|1"; FAKE_HOLDS="$H|$R|2"
boundary_fixture RUN_HAND RUN_HAND
boundary_fail separate "$H"
end

begin 'boundary: failed body preserves both evidence and unavailable declarations' failed-body
FAKE_BODY_FAIL=1; FAKE_HOLDS="$H|$R|2"
boundary_fixture 'RUN_HAND HOLD_HAND' RUN_HAND
echo '# handoff-holds: HOLD_HAND' >> "$PHASES_DIR/01-origin.sh"
boundary_fail failed-body "$H"
contains "$(row_msg origin FAIL)" 'body evidence retained' || bad 'body evidence lost'
contains "$(cat "$RUNROOT/boundary/01-origin/diagnostics")" 'declarations unavailable' || bad 'missing failed declaration diagnostic'
[ -f "$RUNROOT/.keep-rundir" ] || bad 'rundir not preserved'
unset FAKE_BODY_FAIL
end

begin 'boundary: successful provides survive dirty boundary and become suspect' suspect
FAKE_HOLDS="$H|$R|2"; FAKE_SETTLE=120
boundary_fixture RUN_HAND ""
mkphase "$PHASES_DIR/02-consumer.sh" consumer no RUN_HAND "" "" <<'BODY'
[ "$RUN_HAND" = 11111111-1111-1111-1111-111111111111 ]
BODY
run_driver suspect
has_row consumer PASS || bad 'successful provides lost after boundary failure'
contains "$(row_msg origin FAIL)" 'provides suspect' || bad 'provides not marked suspect'
end

begin 'boundary: stalled cancel consumes remaining deadline' stalled
FAKE_DB_IDS="$R"; FAKE_CANCEL_STALL=1
boundary_fixture
boundary_fail stalled 'cancel rejected'
[ "$(cat "$FAKE_CLOCK")" = 120 ] || bad 'cancel not bounded by shared deadline'
end

begin 'boundary: stalled DB operation consumes remaining deadline' stalled-db
FAKE_DB_STALL=1
boundary_fixture
boundary_fail stalled-db unverifiable
[ "$(cat "$FAKE_CLOCK")" = 120 ] || bad 'DB not bounded by remaining deadline'
[ "$(cat "$RUNROOT/boundary/01-origin/1-workers.exit")" = 124 ] || bad 'DB exit not retained'
contains "$(cat "$RUNROOT/boundary/01-origin/1-workers.stderr")" 'stalled psql' || bad 'DB stderr missing'
end

for declaration in MISSING bad-name HOLD_HAND; do
  begin "boundary: invalid hold declaration $declaration" "hold-declaration-$declaration"
  boundary_fixture "" ""
  echo "# handoff-holds: $declaration" >> "$PHASES_DIR/01-origin.sh"
  if [ "$declaration" = HOLD_HAND ]; then echo 'HOLD_HAND=bad-value' >> "$PHASES_DIR/01-origin.sh"; fi
  boundary_fail "hold-declaration-$declaration" declaration
  end
done

begin 'boundary: nonterminal query fails closed' run-read
FAKE_DB_IDS=not-a-uuid
boundary_fixture
boundary_fail run-read unverifiable
[ ! -s "$FAKE_API_LOG" ] || bad 'malformed run was cancelled'
end

begin 'boundary: baseline hold persists without false contamination' baseline-hold
FAKE_BASE_HOLDS="$H|$R|1"; FAKE_HOLDS="$H|$R|1"
boundary_fixture
run_driver baseline-hold
has_row origin PASS || bad 'baseline hold should remain permitted'
boundary_evidence
end

for mode in whitespace newline newlines tab array multi trailing; do
  begin "boundary: exact scalar hold declaration $mode" "scalar-$mode"
  boundary_fixture HOLD_HAND ""
  echo '# handoff-holds: HOLD_HAND' >> "$PHASES_DIR/01-origin.sh"
  case "$mode" in
    whitespace) echo "HOLD_HAND='   '" >> "$PHASES_DIR/01-origin.sh" ;;
    newline) printf '%s\n' "HOLD_HAND=\$'\n'" >> "$PHASES_DIR/01-origin.sh" ;;
    newlines) printf '%s\n' "HOLD_HAND=\$'\n\n'" >> "$PHASES_DIR/01-origin.sh" ;;
    tab) printf '%s\n' "HOLD_HAND=\$'\t'" >> "$PHASES_DIR/01-origin.sh" ;;
    trailing)
      printf "HOLD_HAND='%s\n\n'\n" "$H" >> "$PHASES_DIR/01-origin.sh"
      FAKE_HOLDS="$H|$R|1" ;;
    array) echo 'HOLD_HAND=(44444444-4444-4444-4444-444444444444)' >> "$PHASES_DIR/01-origin.sh" ;;
    multi)
      printf "HOLD_HAND='%s\n%s'\n" "$H" "$H2" >> "$PHASES_DIR/01-origin.sh"
      FAKE_HOLDS="$H|$R|1
$H2|$R|2" ;;
  esac
  if [ "$mode" = multi ] || [ "$mode" = trailing ]; then
    run_driver "scalar-$mode"
    has_row origin PASS || bad 'exact whitespace-separated hold IDs rejected'
    boundary_evidence
  else boundary_fail "scalar-$mode" declaration; fi
  end
done

# Phase 52's SDK branch creates no holds, but exports the declared list explicitly.
for mode in clean leak; do
  begin "boundary: explicit empty conditional hold list $mode" "empty-holds-$mode"
  boundary_fixture HOLD_HAND ""
  echo '# handoff-holds: HOLD_HAND' >> "$PHASES_DIR/01-origin.sh"
  echo 'HOLD_HAND=""' >> "$PHASES_DIR/01-origin.sh"
  if [ "$mode" = leak ]; then
    FAKE_HOLDS="$H|$R|2"
    boundary_fail "empty-holds-$mode" "$H"
  else
    run_driver "empty-holds-$mode"
    has_row origin PASS || bad 'explicit no-hold handoff must PASS'
    [ "$RC" -eq 0 ] || bad 'no-hold handoff exit must be zero'
  fi
  boundary_evidence
  end
done

begin 'boundary: empty run handoff still fails closed' empty-runs
boundary_fixture RUN_HAND RUN_HAND
echo 'RUN_HAND=""' >> "$PHASES_DIR/01-origin.sh"
boundary_fail empty-runs 'empty handoff declaration'
end

# Helper and caller locals must never replace the imported declaration value.
for variable in id name value names kind declaration count run gen; do
  begin "boundary: lowercase handoff $variable survives dynamic scope" "lower-$variable"
  mkphase "$PHASES_DIR/01-origin.sh" boundary yes "" "$variable" "$variable" <<BODY
$variable=$R
pass body
BODY
  FAKE_DB_IDS="$R"; FAKE_WORKERS="$W|$R|3"
  run_driver "lower-$variable"
  has_row origin PASS || bad 'valid lowercase handoff rejected'
  [ "$RC" = 0 ] || bad 'valid handoff must exit zero'
  [ ! -s "$FAKE_API_LOG" ] || bad 'declared run cancelled'
  [ -f "$RUNROOT/junit.xml" ] || bad 'JUnit missing'
  end
done

for parent in absent stale; do
for mode in run hold; do
  begin "boundary: unset provided $mode with $parent parent" "unset-$parent-$mode"
  if [ "$parent" = stale ]; then export HOLD_HAND="$H"; else unset HOLD_HAND; fi
  mkphase "$PHASES_DIR/01-origin.sh" boundary yes "" HOLD_HAND "" <<'BODY'
unset HOLD_HAND
declare HOLD_HAND
pass body
BODY
  if [ "$mode" = run ]; then
    # Replace the existing empty header rather than adding a duplicate.
    sed 's/^# handoff:.*$/# handoff: HOLD_HAND/' "$PHASES_DIR/01-origin.sh" > "$TMP/phase"
    mv "$TMP/phase" "$PHASES_DIR/01-origin.sh"
  else
    echo '# handoff-holds: HOLD_HAND' >> "$PHASES_DIR/01-origin.sh"
  fi
  boundary_fail "unset-$parent-$mode" 'declaration HOLD_HAND'
  [ -f "$RUNROOT/junit.xml" ] || bad 'JUnit missing'
  [ -f "$RUNROOT/summary.md" ] || bad 'summary missing'
  contains "$(cat "$RUNROOT/junit.xml")" 'failures="1"' || bad 'JUnit failure missing'
  contains "$(cat "$RUNROOT/summary.md")" 'FAIL' || bad 'summary failure missing'
  unset HOLD_HAND
  end
done
done

# A declaration inherited from the caller remains valid when not provided here.
for mode in run hold; do
  begin "boundary: inherited $mode without provides remains valid" "inherited-$mode"
  export HOLD_HAND="$H"
  mkphase "$PHASES_DIR/01-origin.sh" boundary yes "" "" "" <<'BODY'
pass body
BODY
  if [ "$mode" = run ]; then
    sed 's/^# handoff:.*$/# handoff: HOLD_HAND/' "$PHASES_DIR/01-origin.sh" > "$TMP/phase"
    mv "$TMP/phase" "$PHASES_DIR/01-origin.sh"
    FAKE_DB_IDS="$H"
  else
    echo '# handoff-holds: HOLD_HAND' >> "$PHASES_DIR/01-origin.sh"
    FAKE_HOLDS="$H|$R|1"
  fi
  run_driver "inherited-$mode"
  has_row origin PASS || bad 'inherited declaration lost'
  [ "$RC" = 0 ] || bad 'inherited declaration must exit zero'
  [ ! -s "$FAKE_API_LOG" ] || bad 'inherited run cancelled'
  unset HOLD_HAND
  end
done

for mode in clean dirty; do
  begin "boundary: provided status survives $mode result reporting" "status-$mode"
  mkphase "$PHASES_DIR/01-origin.sh" boundary yes "" status status <<'BODY'
status=11111111-1111-1111-1111-111111111111
pass body
BODY
  mkphase "$PHASES_DIR/02-consumer.sh" consumer no status "" "" <<'BODY'
[ "$status" = 11111111-1111-1111-1111-111111111111 ] || fail 'provided status changed'
pass consumer
BODY
  if [ "$mode" = dirty ]; then FAKE_HOLD_ERROR=1; fi
  run_driver "status-$mode"
  expected=PASS; expected_rc=0; failures=0
  if [ "$mode" = dirty ]; then expected=FAIL; expected_rc=1; failures=2; fi
  has_row origin "$expected" || bad 'origin result replaced by provided status'
  has_row consumer "$expected" || bad 'consumer result incorrect'
  contains "$(cat "$RUNROOT/logs/02-consumer.log")" 'PASS' || bad 'consumer lost status scalar'
  [ "$RC" = "$expected_rc" ] || bad 'suite exit incorrect'
  contains "$(cat "$RUNROOT/junit.xml")" "failures=\"$failures\"" || bad 'JUnit result incorrect'
  end
done

for mode in run hold; do
  begin "boundary: $mode header glob cannot expand to a provided name" "glob-$mode"
  boundary_fixture 'RUN_HAND HOLD_HAND' ""
  header=handoff; pattern='RUN*'; filename=RUN_HAND
  if [ "$mode" = hold ]; then header=handoff-holds; pattern='HOLD*'; filename=HOLD_HAND; fi
  sed "s/^# handoff:.*$/# $header: $pattern/" "$PHASES_DIR/01-origin.sh" > "$TMP/phase"
  mv "$TMP/phase" "$PHASES_DIR/01-origin.sh"
  touch "$RUNROOT/$filename"
  # The real driver must run in Bash, with a matching file in its actual cwd.
  ( cd "$RUNROOT"; run_driver "glob-$mode" )
  # run_driver's RC is local to that subshell; final artifacts prove the result.
  has_row origin FAIL || bad 'header glob expanded into a valid declaration'
  contains "$(row_msg origin FAIL)" "malformed $header declaration $pattern" || bad 'literal malformed header missing'
  [ -f "$RUNROOT/junit.xml" ] || bad 'JUnit missing'
  end
done

begin 'artifacts: reused rundir cannot publish stale boundary evidence' capture-fresh
boundary_fixture
mkdir -p "$RUNROOT/boundary/01-origin"
printf 'old cancel response\n' > "$RUNROOT/boundary/01-origin/cancel-$R.body"
FAKE_WORKER_ERROR=1
run_driver capture-fresh
has_row origin FAIL || bad 'read failure lost'
[ ! -e "$RUNROOT/artifacts/01-origin/boundary/cancel-$R.body" ] || bad 'stale cancel evidence was published'
[ -f "$RUNROOT/artifacts/01-origin/boundary/1-workers.exit" ] || bad 'current evidence was not published'
end

begin 'artifacts: replace an uploaded boundary mirror without replacing body evidence' capture-existing
boundary_fixture
mkdir -p "$RUNROOT/artifacts/01-origin/boundary"
printf 'old cancel response\n' > "$RUNROOT/artifacts/01-origin/boundary/cancel-$R.body"
printf 'current body evidence\n' > "$RUNROOT/artifacts/01-origin/phase.log"
FAKE_WORKER_ERROR=1
run_driver capture-existing
has_row origin FAIL || bad 'read failure lost'
[ ! -e "$RUNROOT/artifacts/01-origin/boundary/cancel-$R.body" ] || bad 'old uploaded cancel evidence survived'
[ -f "$RUNROOT/artifacts/01-origin/boundary/1-workers.exit" ] || bad 'current boundary was nested instead of replaced'
[ ! -d "$RUNROOT/artifacts/01-origin/boundary/01-origin" ] || bad 'nested boundary directory was published'
[ "$(cat "$RUNROOT/artifacts/01-origin/phase.log")" = 'current body evidence' ] || bad 'body evidence was replaced'
end

begin 'artifacts: mirror status I/O failure cannot suppress the phase verdict' capture-status-io
FAKE_WORKER_ERROR=1
mkphase "$PHASES_DIR/01-origin.sh" boundary yes "" "" "" <<'BODY'
mkdir -p "$RUNROOT/artifacts/01-origin/boundary-copy.exit"
pass body
BODY
run_driver capture-status-io
has_row origin FAIL || bad 'artifact metadata failure suppressed final FAIL row'
[ -f "$RUNROOT/junit.xml" ] || bad 'artifact metadata failure prevented JUnit'
[ "$RC" = 1 ] || bad 'suite exit incorrect'
contains "$(row_msg origin FAIL)" 'copy status unavailable' || bad 'metadata write failure not explicit'
end

begin 'artifacts: TERM-ignoring compose is killed at each boundary capture budget' capture-kill
FAKE_WORKER_ERROR=1
mkphase "$PHASES_DIR/01-origin.sh" boundary yes "" "" "" <<'BODY'
pass body
BODY
cat > "$TMP/stubborn-compose" <<'COMPOSE_EOF'
#!/usr/bin/env bash
trap '' TERM
sleep 3
echo delayed-artifact
COMPOSE_EOF
# shellcheck disable=SC2034  # consumed by the sourced driver
COMPOSE=(bash "$TMP/stubborn-compose")
capture_start=$SECONDS
run_driver capture-kill
capture_elapsed=$((SECONDS - capture_start))
[ "$capture_elapsed" -le 10 ] || bad "four two-second captures took ${capture_elapsed}s"
for service in api agent forge-fake db; do
  contains "$(cat "$RUNROOT/artifacts/01-origin/$service.log")" delayed-artifact && bad "capture exceeded budget for $service"
  [ -s "$RUNROOT/artifacts/01-origin/$service.log.exit" ] || bad "truncated capture status missing"
  [ "$(cat "$RUNROOT/artifacts/01-origin/$service.log.exit" 2>/dev/null)" != 0 ] || bad "truncated capture reported success"
done
has_row origin FAIL || bad 'boundary failure lost'
[ "$RC" = 1 ] || bad 'suite exit incorrect'
end

# One real delayed log proves body capture survives two seconds; other operations
# are immediate. The shim records budgets and simulates one truncated operation.
begin 'artifacts: body capture allows delayed logs and records operation statuses' capture-body
mkphase "$PHASES_DIR/01-origin.sh" body yes "" "" "" <<'BODY'
fail body
BODY
mkdir -p "$TMP/limiter-bin"
export FAKE_LIMITER_LOG="$TMP/limiter-args"
export FAKE_REAL_LIMITER
FAKE_REAL_LIMITER="$(command -v timeout || command -v gtimeout)"
cat > "$TMP/limiter-bin/timeout" <<'LIMITER_EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_LIMITER_LOG"
if [ "${FAKE_COPY_FAIL:-0}" = 1 ] && [ "$2" = 2 ]; then exit 124; fi
if [[ "$*" = *'forge-fake' ]]; then echo truncated >&2; exit 137; fi
exec "$FAKE_REAL_LIMITER" "$@"
LIMITER_EOF
chmod +x "$TMP/limiter-bin/timeout"
cat > "$TMP/delayed-compose" <<'COMPOSE_EOF'
#!/usr/bin/env bash
if [ "${!#}" = api ]; then sleep 3; echo delayed-body-log; fi
COMPOSE_EOF
# shellcheck disable=SC2034
COMPOSE=(bash "$TMP/delayed-compose")
PATH="$TMP/limiter-bin:$PATH" run_driver capture-body
adir="$RUNROOT/artifacts/01-origin"
contains "$(cat "$adir/api.log")" delayed-body-log || bad 'body log arriving after two seconds lost'
contains "$(cat "$FAKE_LIMITER_LOG")" '--signal=KILL 30' || bad 'body limiter budget is not 30 seconds'
contains "$(cat "$FAKE_READ_LOG")" 'timeout=30' || bad 'body DB budget is not 30 seconds'
[ "$(cat "$adir/api.log.exit" 2>/dev/null)" = 0 ] || bad 'successful log status missing'
[ "$(cat "$adir/forge-fake.log.exit" 2>/dev/null)" = 137 ] || bad 'truncated log status missing'
[ "$(cat "$adir/runs.txt.exit" 2>/dev/null)" = 0 ] || bad 'DB capture status missing'
end

begin 'artifacts: boundary mirror timeout is explicit' capture-copy
FAKE_WORKER_ERROR=1
export FAKE_COPY_FAIL=1
boundary_fixture
PATH="$TMP/limiter-bin:$PATH" run_driver capture-copy
adir="$RUNROOT/artifacts/01-origin"
[ "$(cat "$adir/boundary-copy.exit" 2>/dev/null)" = 124 ] || bad 'copy timeout status missing'
contains "$(row_msg origin FAIL)" 'boundary evidence copy failed' || bad 'copy failure absent from final result'
unset FAKE_COPY_FAIL
end

begin 'artifacts: missing limiter is explicit for capture and mirror' capture-no-limiter
export FAKE_NO_LIMITER=1
FAKE_WORKER_ERROR=1
boundary_fixture
run_driver capture-no-limiter
adir="$RUNROOT/artifacts/01-origin"
[ "$(cat "$adir/api.log.exit" 2>/dev/null)" = 127 ] || bad 'missing capture limiter status absent'
contains "$(cat "$adir/api.log")" 'no timeout/gtimeout limiter' || bad 'missing capture limiter diagnostic absent'
[ "$(cat "$adir/boundary-copy.exit" 2>/dev/null)" = 127 ] || bad 'missing copy limiter status absent'
contains "$(row_msg origin FAIL)" 'boundary evidence copy failed' || bad 'missing limiter silently dropped boundary evidence'
unset FAKE_NO_LIMITER
end

# ============================================================================
printf '\ncases=%s passed=%s\n' "$cases" "$passed"
[ "$cases" -eq "$passed" ] || exit 1
