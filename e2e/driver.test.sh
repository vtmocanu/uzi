#!/usr/bin/env bash
# Hermetic behavioural test for e2e/driver.sh (PRD #966 M2), the shape of
# e2e/reclaim-leaked-e2e.test.sh: fake helper FUNCTIONS + fixture phase files,
# no stack, no docker, no network. Each case runs the REAL driver in a subshell
# that defines the fakes and sources driver.sh AT TOP LEVEL (never inside a
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
# Fixture repos must not read host git config: a global core.fsmonitor=true
# leaves a detached fsmonitor daemon per repo on macOS.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRIVER="$REPO_ROOT/e2e/driver.sh"
[ -f "$DRIVER" ] || { echo "driver.sh not found at $DRIVER" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

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
mkdir() {
  if [ "${FAKE_STAGE_FAILURE:-}" = yes ] && [[ "$*" == *"/boundary/"* ]]; then return 1; fi
  command mkdir "$@"
}

# A VIRTUAL CLOCK. The boundary measures time with `date +%s` and waits with `sleep`; the
# fakes below replace both with a counter in $FAKE_SEQ/.clock that only `sleep` advances. A
# 120 s deadline then costs no wall time, every elapsed-seconds assertion is exact, and no
# case can flake on a second-boundary tick.
date() {
  if [ "$1" = "+%s" ]; then cat "$FAKE_SEQ/.clock"; else command date "$@"; fi
}
sleep() { printf '%s' $(( $(cat "$FAKE_SEQ/.clock") + $1 )) > "$FAKE_SEQ/.clock"; }

# db_psql_rows (SQL [SECS]) is the ONE database seam of the boundary. Every call is logged
# to $FAKE_Q_LOG as `epoch<TAB>phase<TAB>key<TAB>secs<TAB>rows<TAB>sql`, with the SECS
# deadline argument the driver passed, and answered from a per-(phase, query key) SEQUENCE
# file: line N answers the Nth call of that key in that phase (the last line repeats).
# The key is owner | runs | workers | holds | history | capture. A line is `-` (zero
# rows), rows separated by `;`, or `!N` (print an error to stderr, exit N). The FIRST
# `holds` call of a phase is the phase-start baseline; later ones are samples. The phase
# is read from the driver's own `_bdir` (the fake runs in the driver's shell), so a
# multi-phase case can script each phase apart: file `<NN-slug>.<key>`, else `<key>`.
fake_seq() {
  local key="$1" ph="${_bdir##*/}" f cnt n line rc
  FAKE_ROWS=""
  f="$FAKE_SEQ/$ph.$key"; [ -f "$f" ] || f="$FAKE_SEQ/$key"
  if [ ! -f "$f" ]; then
    if [ "$key" = owner ]; then FAKE_ROWS="$FAKE_OWNER"; fi
    return 0
  fi
  cnt="$FAKE_SEQ/.cnt.$ph.$key"
  n=$(( $(cat "$cnt" 2>/dev/null || echo 0) + 1 ))
  printf '%s' "$n" > "$cnt"
  line="$(sed -n "${n}p" "$f")"
  if [ -z "$line" ]; then line="$(tail -n 1 "$f")"; fi
  case "$line" in
    '!'*) rc="${line#!}"; echo "fake psql error ($key)" >&2; return "$rc" ;;
    -)    ;;
    *)    FAKE_ROWS="${line//;/$'\n'}" ;;
  esac
  return 0
}
db_psql_rows() {
  local key rc=0
  case "$1" in
    *"from runs order by"*)  key=capture; FAKE_ROWS="FAKE_DB_RUNS r1 admin running issue" ;;
    *"group by status"*)     key=capture; FAKE_ROWS="FAKE_DB_COUNTS running issue 1" ;;
    *"FROM users WHERE email"*)       key=owner ;;
    *"final_disposition"*)            key=history ;;
    *"worker_active_runs"*)           key=workers ;;
    *"recovery_custody_holds"*)       key=holds ;;
    *"status not in"*)                key=runs ;;
    *) echo "fake db_psql_rows: unexpected query: $1" >&2; return 99 ;;
  esac
  if [ "$key" != capture ]; then fake_seq "$key" || rc=$?; fi
  # FAKE_RUNS_SLEEP=N: a runs read costs N virtual seconds (a slow DB on a real clock).
  if [ "$key" = runs ] && [ -n "${FAKE_RUNS_SLEEP:-}" ]; then sleep "$FAKE_RUNS_SLEEP"; fi
  if [ "$key" = runs ] && [ -f "$FAKE_SEQ/${_bdir##*/}.runs-cost" ]; then
    sleep "$(sed -n "$(cat "$FAKE_SEQ/.cnt.${_bdir##*/}.runs")p" "$FAKE_SEQ/${_bdir##*/}.runs-cost")"
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$(date +%s)" "${_bdir##*/}" "$key" "${2:-}" \
    "${FAKE_ROWS//$'\n'/;}" "$1" >> "$FAKE_Q_LOG"
  if [ "$key" = owner ] && [ -n "${FAKE_OWNER_SLEEP:-}" ]; then sleep "$FAKE_OWNER_SLEEP"; fi
  [ "$rc" -eq 0 ] || return "$rc"
  if [ -n "$FAKE_ROWS" ]; then printf '%s\n' "$FAKE_ROWS"; fi
  return 0
}
# The driver must NEVER write a run status (no db_psql at all): any call is logged so a
# case can assert the log stays empty.
db_psql() { printf '%s\n' "$1" >> "${FAKE_DB_LOG:-/dev/null}"; }
# boundary_cancel PATH SECS PREFIX: log the cancel (with its SECS bound), write the same
# evidence files lib.sh's real helper writes, and answer FAKE_CANCEL_CODE / FAKE_CANCEL_EXIT.
boundary_cancel() {
  local code="${FAKE_CANCEL_CODE:-200}" ex="${FAKE_CANCEL_EXIT:-0}"
  printf 'cancel %s %s\n' "$1" "$2" >> "${FAKE_API_LOG:-/dev/null}"
  printf '%s' "$code" > "$3.http"; printf '%s\n' "$ex" > "$3.exit"
  printf '{}' > "$3.body"; : > "$3.stderr"
  [ "$ex" -eq 0 ] || return 1
  case "$code" in 2??) return 0 ;; *) return 1 ;; esac
}
wait_status() { :; }
# fake_docker stands in for the `COMPOSE` array the driver uses ONLY in on-red
# artifact capture: `docker compose logs …` (echo a marker so the captured svc.log
# has content) and `docker compose ps …` (print nothing, i.e. no service running, so
# 00-preflight's leg 4 takes the deferred-note path). COMPOSE=(fake_docker) is set in
# the parent test shell and inherited by run_driver's subshell.
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
# mkphase PATH TITLE CRITICAL REQUIRES PROVIDES HANDOFF [RACESENS [HOLDS]]  <<'BODY' ... BODY
# Header via printf (needs the arg values); body via the caller's QUOTED heredoc
# on stdin so `$FOO` in a body stays literal until the phase runs. The optional 7th
# arg emits `# race-sensitive: <val>` (default: line omitted); the optional 8th emits
# `# handoff-holds: <val>` (vars holding custody-hold UUIDs; default: line omitted).
mkphase() {
  local path="$1" title="$2" crit="$3" req="$4" prov="$5" hand="$6" rs="${7:-}" hh="${8:-}" slug
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
    [ -z "$hh" ] || printf '# handoff-holds: %s\n' "$hh"
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
  FAKE_SEQ="$TMP/seq-$2"; FAKE_Q_LOG="$TMP/q-$2.log"; QL="$FAKE_Q_LOG"
  rm -rf "$RUNROOT" "$PHASES_DIR" "$FAKE_SEQ"; mkdir -p "$RUNROOT" "$PHASES_DIR" "$FAKE_SEQ"
  printf '%s' 1000000 > "$FAKE_SEQ/.clock"
  : > "$ENVFILE"; : > "$FAKE_API_LOG"; : > "$FAKE_DB_LOG"; : > "$FAKE_Q_LOG"
  unset E2E_ONLY E2E_SKIP E2E_FAULT_PHASE E2E_BOUNDARY_TIMEOUT E2E_BOUNDARY_INTERVAL \
    FAKE_CANCEL_CODE FAKE_CANCEL_EXIT FAKE_DOCKER_SEQ FAKE_OWNER_SLEEP FAKE_RUNS_SLEEP FAKE_STAGE_FAILURE 2>/dev/null || true
  # A small boundary budget (virtual seconds, see the fake clock); a case needing more sets its own.
  export E2E_BOUNDARY_TIMEOUT=2 E2E_BOUNDARY_INTERVAL=1
}
# seqf KEY LINE... — script the global sequence for a query key; seqp PHASE KEY LINE... —
# the same for one phase (`<NN>-<slug>`). See fake_seq in FAKES for the line grammar.
seqf() { local k="$1"; shift; printf '%s\n' "$@" > "$FAKE_SEQ/$k"; }
seqp() { local ph="$1" k="$2"; shift 2; printf '%s\n' "$@" > "$FAKE_SEQ/$ph.$k"; }
# qcount KEY [PHASE] — logged calls of a query key (optionally in one phase); qargs KEY [PHASE]
# — the SECS argument of each such call, one per line.
qcount() { awk -F'\t' -v k="$1" -v p="${2:-}" '$3==k && (p=="" || $2==p){n++} END{print n+0}' "$QL"; }
qargs() { awk -F'\t' -v k="$1" -v p="${2:-}" '$3==k && (p=="" || $2==p){print $4}' "$QL"; }
# args_within MAX KEY... — every logged call of the keys carries a positive integer SECS <= MAX.
args_within() {
  local max="$1" k a; shift
  for k in "$@"; do
    while IFS= read -r a; do
      [[ "$a" =~ ^[0-9]+$ ]] && [ "$a" -ge 1 ] && [ "$a" -le "$max" ] || { printf '%s=%s' "$k" "$a"; return 1; }
    done < <(qargs "$k")
  done
  return 0
}
# bw_field PHASE N — column N (2 start, 3 end, 4 total) of a phase's boundary_waits.tsv row.
bw_field() { awk -F'\t' -v s="$1" -v n="$2" '$1==s{print $n; exit}' "$RUNROOT/boundary_waits.tsv"; }
bad() { printf '  - %s\n' "$1"; case_fail=1; }
contains() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }
end() {
  cases=$((cases + 1))
  if [ "$case_fail" -eq 0 ]; then passed=$((passed + 1)); printf 'PASS: %s\n' "$cur"
  else printf 'FAIL: %s\n' "$cur"; fi
}
# run_driver — source the fakes + driver AT SUBSHELL TOP LEVEL. NOT wrapped in a
# function: a function would make the driver's top-level `source phase.env.next`
# function-local and silently break provides (that IS case 2's mutation).
LOG=""; RC=0
run_driver() {
  LOG="$TMP/log-$1"
  set +e
  # shellcheck disable=SC1090
  ( set -e; source "$FAKES"; source "$DRIVER" ) > "$LOG" 2>&1
  RC=$?
  set -e
}
# Shared globals the driver reads — exported because they are consumed by the
# driver sourced INSIDE run_driver's subshell (which shellcheck cannot see); the
# same reason applies to the per-case E2E_*/FAKE_* knobs set below.
export ROOT="$REPO_ROOT" FORGE=gitlab EXECUTOR=stub ADMIN_EMAIL=admin@uzi.e2e
# UUID fixtures: the boundary validates every row shape, so ids must be real UUIDs.
export FAKE_OWNER=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa0
R1=11111111-1111-4111-8111-111111111111; R2=22222222-2222-4222-8222-222222222222
W1=33333333-3333-4333-8333-333333333331
H1=44444444-4444-4444-8444-444444444441; H2=44444444-4444-4444-8444-444444444442
# COMPOSE is an ARRAY (can't be exported), but run_driver's `( … )` is a subshell of
# THIS shell, so a plain assignment here is visible inside it. The driver uses it only
# for on-red artifact capture; fake_docker (in FAKES) is the command it resolves to.
# shellcheck disable=SC2034  # consumed by driver.sh inside the run_driver subshell
COMPOSE=(fake_docker)

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
# Case 6 (#2892) — the end-of-phase boundary. An undeclared non-terminal run is cancelled
# ONCE through the API and, if it does not settle, FAILS the phase naming it; a
# handoff-declared id is not residue; a refused cancel FAILS and the driver never writes a
# run status (no DB fallback).
# Mutation: record the leak as a non-fatal row again / skip the cancel -> no FAIL, no cancel.
begin "case6a: undeclared run that never settles is cancelled once and FAILs the phase" 6a
export FAKE_CANCEL_CODE=200
seqf runs "$R1"
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 6a
has_row leaky FAIL || bad "leaky not FAIL"
any_status LEAK && bad "a LEAK status still exists"
contains "$(row_msg leaky FAIL)" "boundary:" || bad "message does not say boundary: $(row_msg leaky FAIL)"
contains "$(row_msg leaky FAIL)" "run=$R1" || bad "message does not name the run: $(row_msg leaky FAIL)"
[ "$(grep -c "^cancel /api/runs/$R1/inputs" "$FAKE_API_LOG")" -eq 1 ] || bad "run was not cancelled exactly once: $(cat "$FAKE_API_LOG")"
[ ! -s "$FAKE_DB_LOG" ] || bad "driver wrote through db_psql: $(cat "$FAKE_DB_LOG")"
[ "$RC" -eq 1 ] || bad "exit code should be 1 (got $RC)"
end

begin "case6b: handoff-declared run (and its worker row) is not residue" 6b
export FAKE_CANCEL_CODE=200
seqf runs "$R1"
seqf workers "$W1|$R1|1"
mkphase "$PHASES_DIR/10-held.sh" "hands a run forward" no "" RUN_B RUN_B <<BODY
RUN_B=$R1
pass "declared handoff RUN_B"
BODY
run_driver 6b
has_row held PASS || bad "held phase not PASS: $(cat "$TSV")"
[ ! -s "$FAKE_API_LOG" ] || bad "a cancel was attempted for a declared handoff id"
[ "$RC" -eq 0 ] || bad "exit code should be 0 (got $RC)"
end

begin "case6c: refused cancel FAILs, forges nothing, even when the run is terminal next sample" 6c
export FAKE_CANCEL_CODE=403
seqf runs "$R1" "-"
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 6c
has_row leaky FAIL || bad "rejected cancel did not FAIL the phase (run settled): $(cat "$TSV")"
contains "$(row_msg leaky FAIL)" "cancel rejected run=$R1 http=403" || bad "message: $(row_msg leaky FAIL)"
[ ! -s "$FAKE_DB_LOG" ] || bad "driver wrote through db_psql (DB status fallback): $(cat "$FAKE_DB_LOG")"
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
# Case 15 (PRD #966 M1, #2892) — a FAILED boundary must not abort the results/roll-call/
# exit path: summary.md is written, the roll-call runs and the exit code is 1.
# Mutation: make _write_summary end on a bare failing `[ … ] && …` -> the roll-call is
# absent from the log and the driver aborts before the exit-code loop.
begin "case15: a failed boundary still writes summary + runs roll-call/exit 1" 15
export FAKE_CANCEL_CODE=200
seqf runs "$R1"
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 15
has_row leaky FAIL || bad "no FAIL row for leaky"
[ -f "$RUNROOT/summary.md" ] || bad "summary.md was not written on a leaky run"
contains "$(cat "$LOG")" "E2E FAILED: 1 phase(s): leaky" || bad "roll-call did not run after a failed boundary"
[ "$RC" -eq 1 ] || bad "a boundary FAIL must exit 1 (got $RC)"
end

# ============================================================================
# Case 16 (PRD #966 M1, #2892) — TWO undeclared runs are each cancelled once and each named
# on the ONE FAIL row. Guards the row-preserving read: the sample is validated row by row,
# so a fused `r1r2` token would be a malformed row, not two runs.
begin "case16: two leaked runs are each cancelled and named" 16
export FAKE_CANCEL_CODE=200
seqf runs "$R1;$R2"
mkphase "$PHASES_DIR/10-leaky.sh" "leaks two runs" no "" "" "" <<'BODY'
pass "left two runs behind"
BODY
run_driver 16
[ "$(awk -F'\t' '$1=="leaky" && $2=="FAIL"{n++} END{print n+0}' "$TSV")" -eq 1 ] || bad "expected exactly ONE FAIL row for the phase"
contains "$(row_msg leaky FAIL)" "run=$R1" || bad "FAIL row does not name $R1: $(row_msg leaky FAIL)"
contains "$(row_msg leaky FAIL)" "run=$R2" || bad "FAIL row does not name $R2: $(row_msg leaky FAIL)"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/$R1/inputs" || bad "$R1 not cancelled via the API"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/$R2/inputs" || bad "$R2 not cancelled via the API"
end

# ============================================================================
# Case 17 (#990, #2892) — a phase whose BODY fails AND whose boundary is dirty is ONE FAIL
# row (body message, then `; boundary: …`, noting the declarations were unavailable) and
# captures artifacts ONCE, with the widened window. FAKE_DOCKER_SEQ stamps a monotonic
# number per `logs` call: one capture is calls 1-4, a second capture would reach 8.
# Mutation: capture on the body FAIL and again for the boundary -> the counter reaches 8.
begin "case17: body FAIL + dirty boundary is one row and one capture" 17
export FAKE_CANCEL_CODE=200 FAKE_DOCKER_SEQ="$TMP/dseq-17"
rm -f "$FAKE_DOCKER_SEQ"
seqf runs "$R1"
mkphase "$PHASES_DIR/10-failleak.sh" "fail and leak" no "" "" "" <<'BODY'
fail "boom and leak"
BODY
run_driver 17
adir="$RUNROOT/artifacts/10-failleak"
[ "$(awk -F'\t' '$1=="failleak"{n++} END{print n+0}' "$TSV")" -eq 1 ] || bad "expected ONE row for failleak: $(cat "$TSV")"
has_row failleak FAIL || bad "failleak not FAIL"
m="$(row_msg failleak FAIL)"
contains "$m" "boom and leak" || bad "body message lost: $m"
contains "$m" "; boundary: " || bad "boundary reasons not appended: $m"
contains "$m" "declarations unavailable: body failed" || bad "no declarations-unavailable note: $m"
contains "$m" "run=$R1" || bad "message does not name the run: $m"
contains "$(cat "$adir/api.log" 2>/dev/null)" "seq=1" || bad "api.log is not the first capture: $(cat "$adir/api.log" 2>/dev/null)"
[ "$(cat "$FAKE_DOCKER_SEQ")" = 4 ] || bad "artifacts captured more than once (docker logs calls: $(cat "$FAKE_DOCKER_SEQ"))"
[ -f "$adir/boundary/reasons.txt" ] || bad "boundary evidence not copied under artifacts/<NN-slug>/boundary/"
end

# ============================================================================
# Case 18 — a cancelled run that is still on a worker, and a run already terminal in the DB
# that is still on a worker, both FAIL past the deadline. Runs and worker rows get NO
# baseline exemption.
# Mutation: judge the run by its DB status alone -> case 18b reads PASS.
begin "case18a: run cancelled but still on the worker FAILs naming the run" 18a
seqf runs "$R1" "-"
seqf workers "$W1|$R1|2"
mkphase "$PHASES_DIR/10-onworker.sh" "run lingers on the worker" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 18a
has_row onworker FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg onworker FAIL)" "worker=$W1 run=$R1 generation=2" || bad "message: $(row_msg onworker FAIL)"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/$R1/inputs" || bad "run was not cancelled"
end

begin "case18b: DB-terminal run still on a worker FAILs past the deadline" 18b
export E2E_BOUNDARY_TIMEOUT=3
seqf workers "$W1|$R1|2"
mkphase "$PHASES_DIR/10-onworker.sh" "run lingers on the worker" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 18b
has_row onworker FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg onworker FAIL)" "worker=$W1 run=$R1 generation=2" || bad "message: $(row_msg onworker FAIL)"
[ ! -s "$FAKE_API_LOG" ] || bad "a terminal run must not be cancelled"
[ "$(qcount workers)" -ge 2 ] || bad "boundary did not wait for the worker row (samples: $(qcount workers))"
end

# ============================================================================
# Case 19 — an undeclared open custody hold FAILs; a baseline hold that stays is exempt.
# Mutation: ignore holds -> 19a reads PASS. Mutation: drop the baseline exemption -> 19b FAILs.
begin "case19a: undeclared new hold FAILs naming it" 19a
seqf holds "-" "$H1|$R1|3"
mkphase "$PHASES_DIR/10-holder.sh" "opens a hold" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 19a
has_row holder FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg holder FAIL)" "hold=$H1 run=$R1 generation=3" || bad "message: $(row_msg holder FAIL)"
end

begin "case19b: a hold already open at phase start is not residue" 19b
seqf holds "$H1|$R1|3"
mkphase "$PHASES_DIR/10-holder.sh" "keeps an old hold" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 19b
has_row holder PASS || bad "baseline hold wrongly counted: $(cat "$TSV")"
end

# ============================================================================
# Case 20 — the baseline hold RELEASES while a NEW one appears (the open-hold count is
# unchanged): compared by ID this is residue. The history read covers both holds.
# Mutation: compare hold COUNTS -> reads PASS.
begin "case20: baseline hold releases while a new one appears" 20
seqf holds "$H1|$R1|1" "$H2|$R2|1"
seqf history "$H1|$R1|1|released|published|evidence-a;$H2|$R2|1|open|-|-"
mkphase "$PHASES_DIR/10-swap.sh" "swaps holds" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 20
has_row swap FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg swap FAIL)" "hold=$H2 run=$R2 generation=1" || bad "message does not name the new hold: $(row_msg swap FAIL)"
contains "$(row_msg swap FAIL)" "hold=$H1 state=released disposition=published" || bad "history of the released baseline hold missing: $(row_msg swap FAIL)"
[ "$(qcount history)" -eq 1 ] || bad "expected exactly one history read (got $(qcount history))"
contains "$(awk -F'\t' '$3=="history"{print $6}' "$QL")" "'$H1'" || bad "history query omits the baseline hold"
end

# ============================================================================
# Case 21 — every settlement read, on a non-zero exit AND on malformed output, is an
# `unverifiable` FAIL; the body still ran (the begin failure does not skip it).
# Mutation: treat a failed/garbled read as zero rows -> every variant reads PASS.
read_case() {   # ID LABEL KEY PHASE-CALL BAD-LINE EXPECT
  local id="$1" label="$2" key="$3" at="$4" badline="$5" expect="$6"
  begin "case21-$id: $label read, $expect -> FAIL unverifiable" "21$id"
  if [ "$at" = 1 ]; then seqf "$key" "$badline"; else seqf "$key" "-" "$badline"; fi
  mkphase "$PHASES_DIR/10-reader.sh" "reads" no "" "" "" <<BODY
touch "$RUNROOT/body-ran"
pass "body ok"
BODY
  run_driver "21$id"
  has_row reader FAIL || bad "not FAIL: $(cat "$TSV")"
  contains "$(row_msg reader FAIL)" "unverifiable $label ($expect)" || bad "message: $(row_msg reader FAIL)"
  [ -f "$RUNROOT/body-ran" ] || bad "body did not run"
  end
}
read_case a owner owner 1 '!2' "psql exit 2"
read_case b owner owner 1 'garbage' "malformed row"
read_case c "baseline holds" holds 1 '!2' "psql exit 2"
read_case d "baseline holds" holds 1 'garbage' "malformed row"
read_case e runs runs 1 '!2' "psql exit 2"
read_case f runs runs 1 'garbage' "malformed row"
read_case g runs runs 1 "$R1;oops" "malformed row"
read_case h workers workers 1 '!2' "psql exit 2"
read_case i workers workers 1 "$W1|$R1|x" "malformed row"
read_case j holds holds 2 '!2' "psql exit 2"
read_case k holds holds 2 "$H1|$R1|1|extra" "malformed row"

# ============================================================================
# Case 22 — a read or a cancel that TIMES OUT FAILs with its exit status, and every
# settlement call is handed a positive remaining-seconds bound no larger than the budget.
# Mutation: pass no bound / the whole budget at every call -> args_within or the
# decreasing-bound assertion fails.
begin "case22a: read timeout (exit 124) FAILs" 22a
seqf runs "!124"
mkphase "$PHASES_DIR/10-slow.sh" "slow read" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 22a
has_row slow FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg slow FAIL)" "unverifiable runs (timed out, exit 124)" || bad "message: $(row_msg slow FAIL)"
args_within 2 runs workers || bad "a settlement call got a bad bound: $(args_within 2 runs workers)"
end

begin "case22b: cancel timeout (curl 28) FAILs and the bounds shrink" 22b
export E2E_BOUNDARY_TIMEOUT=3 FAKE_CANCEL_EXIT=28 FAKE_CANCEL_CODE=000
seqf runs "$R1" "-"
mkphase "$PHASES_DIR/10-slow.sh" "slow cancel" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 22b
has_row slow FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg slow FAIL)" "cancel timed out run=$R1 (exit 28)" || bad "message: $(row_msg slow FAIL)"
args_within 3 runs workers || bad "settlement bound out of range: $(args_within 3 runs workers)"
cs="$(awk '/^cancel /{print $3}' "$FAKE_API_LOG")"
{ [[ "$cs" =~ ^[0-9]+$ ]] && [ "$cs" -ge 1 ] && [ "$cs" -le 3 ]; } || bad "cancel got a bad bound: [$cs]"
end

# ============================================================================
# Case 23 — deadline exhaustion: FAIL, no settlement call starts after the deadline, the
# bounds shrink, and exactly ONE history read (fixed 5 s bound) runs AFTER the loop; its
# state/disposition is in the message and in artifacts/<NN>/boundary/. A failing history
# read says `release evidence unavailable (exit N)`, keeps its stderr, verdict unchanged.
# Mutation: no history read / read it inside the loop / give it the spent deadline.
begin "case23a: deadline exhaustion + history evidence" 23a
export E2E_BOUNDARY_TIMEOUT=3
seqf holds "-" "$H1|$R1|1"
seqf history "$H1|$R1|1|open|keep|ev-1"
mkphase "$PHASES_DIR/10-stuck.sh" "hold never settles" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 23a
has_row stuck FAIL || bad "not FAIL: $(cat "$TSV")"
m="$(row_msg stuck FAIL)"
contains "$m" "hold=$H1" || bad "message: $m"
contains "$m" "state=open disposition=keep" || bad "history state/disposition not in message: $m"
contains "$m" "not settled within 3s" || bad "deadline not reported: $m"
[ "$(qcount history)" -eq 1 ] || bad "expected one history read, got $(qcount history)"
[ "$(qargs history)" = 5 ] || bad "history read bound should be 5, got $(qargs history)"
args_within 3 workers runs || bad "bad settlement bound: $(args_within 3 workers runs)"
first="$(awk -F'\t' '$3=="runs"{print $1; exit}' "$QL")"
last="$(awk -F'\t' '$3=="runs"||$3=="workers"{t=$1} END{print t}' "$QL")"
[ "$last" -le $((first + 3)) ] || bad "a settlement call started after the deadline (first=$first last=$last)"
r1="$(qargs runs | sed -n 1p)"; rl="$(qargs runs | tail -1)"
[ "$rl" -lt "$r1" ] || bad "remaining-seconds bound did not shrink ($r1 -> $rl)"
contains "$(cat "$RUNROOT/artifacts/10-stuck/boundary/history.out" 2>/dev/null)" "$H1|$R1|1|open|keep" || bad "history not staged under artifacts/<NN>/boundary/"
[ -f "$RUNROOT/artifacts/10-stuck/boundary/first-runs.out" ] && [ -f "$RUNROOT/artifacts/10-stuck/boundary/last-runs.out" ] || bad "first/last sample reads not staged"
end

begin "case23b: failing history read says so and keeps its stderr" 23b
seqf holds "-" "$H1|$R1|1"
seqf history "!3"
mkphase "$PHASES_DIR/10-stuck.sh" "hold never settles" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 23b
has_row stuck FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg stuck FAIL)" "release evidence unavailable (exit 3)" || bad "message: $(row_msg stuck FAIL)"
contains "$(cat "$RUNROOT/artifacts/10-stuck/boundary/history.stderr" 2>/dev/null)" "fake psql error" || bad "history stderr not kept"
end

# ============================================================================
# Case 24 — the same ID, declared vs undeclared. Undeclared FAILs naming it; declared PASSes
# AND the query log proves the holds / worker queries ran and observed the ID.
# Use independent driver invocations: a declaration cannot forgive a prior known leak.
# Mutation: skip the worker/holds reads for a declared handoff -> the log assertions fail.
begin "case24a: undeclared hold FAILs, declared hold PASSes (and was observed)" 24a
seqf holds "-" "$H1|$R1|1"
mkphase "$PHASES_DIR/10-undeclared.sh" "no declaration" no "" "" "" <<BODY
HOLD_X=$H1
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-declared.sh" "declared hold" no "" HOLD_X "" "" HOLD_X <<BODY
HOLD_X=$H1
pass "body ok"
BODY
export E2E_ONLY=undeclared
run_driver 24a-undeclared
has_row undeclared FAIL || bad "undeclared hold did not FAIL: $(cat "$TSV")"
contains "$(row_msg undeclared FAIL)" "hold=$H1" || bad "message does not name the hold"
export E2E_ONLY=declared
run_driver 24a-declared
has_row declared PASS || bad "declared hold did not PASS: $(cat "$TSV")"
contains "$(awk -F'\t' '$2=="11-declared" && $3=="holds"{print $5}' "$QL")" "$H1" || bad "holds query never observed $H1 in the declared phase"
end

begin "case24b: undeclared run FAILs, declared run PASSes (and was observed)" 24b
seqf runs "$R1"
seqf workers "$W1|$R1|1"
mkphase "$PHASES_DIR/10-undeclared.sh" "no declaration" no "" "" "" <<BODY
RUN_X=$R1
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-declared.sh" "declared run" no "" "" RUN_X <<BODY
RUN_X=$R1
pass "body ok"
BODY
export E2E_ONLY=undeclared
run_driver 24b-undeclared
has_row undeclared FAIL || bad "undeclared run did not FAIL: $(cat "$TSV")"
contains "$(row_msg undeclared FAIL)" "run=$R1" || bad "message does not name the run"
export E2E_ONLY=declared
run_driver 24b-declared
has_row declared PASS || bad "declared run did not PASS: $(cat "$TSV")"
contains "$(awk -F'\t' '$2=="11-declared" && $3=="workers"{print $5}' "$QL")" "$R1" || bad "worker query never observed $R1 in the declared phase"
contains "$(awk -F'\t' '$2=="11-declared" && $3=="runs"{print $5}' "$QL")" "$R1" || bad "runs query never observed $R1 in the declared phase"
end

# ============================================================================
# Case 25 — a declaration is per phase. A declares a run in handoff, B does not: B cancels
# it and FAILs if it does not settle. A non-UUID declared value is a boundary error; an
# empty one declares nothing.
# Mutation: keep declarations across phases -> B reads PASS.
begin "case25a: A hands a run forward, B does not declare it -> B FAILs" 25a
seqp 10-alpha runs "$R1"
seqp 11-beta runs "$R1"
mkphase "$PHASES_DIR/10-alpha.sh" "A declares" no "" RUN_X RUN_X <<BODY
RUN_X=$R1
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-beta.sh" "B forgets" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 25a
has_row alpha PASS || bad "alpha not PASS: $(cat "$TSV")"
has_row beta FAIL || bad "beta did not FAIL for the undeclared run: $(cat "$TSV")"
contains "$(row_msg beta FAIL)" "run=$R1" || bad "beta message: $(row_msg beta FAIL)"
[ "$(grep -c "^cancel /api/runs/$R1/inputs" "$FAKE_API_LOG")" -eq 1 ] || bad "expected one cancel (by beta): $(cat "$FAKE_API_LOG")"
end

begin "case25b: non-UUID declaration FAILs; empty declaration declares nothing" 25b
mkphase "$PHASES_DIR/10-baddecl.sh" "bad value" no "" RUN_X RUN_X <<'BODY'
RUN_X=not-a-uuid
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-emptydecl.sh" "empty value" no "" RUN_Y RUN_Y <<'BODY'
RUN_Y=
pass "body ok"
BODY
run_driver 25b
has_row "baddecl" FAIL || bad "non-UUID declaration did not FAIL: $(cat "$TSV")"
contains "$(row_msg "baddecl" FAIL)" "malformed declaration RUN_X" || bad "message: $(row_msg baddecl FAIL)"
has_row emptydecl PASS || bad "empty declaration should declare nothing and PASS: $(cat "$TSV")"
end

# ============================================================================
# Case 26 — contamination. After a failed boundary every later recorded row (PASS rows
# included) carries ` [ran after contaminated boundary: <slug>[ <slug>…]]`; later phases
# still RUN (also after a critical phase's boundary failure — only a BODY failure stops).
# Mutation: no annotation / stop after a critical boundary failure.
begin "case26: contamination annotation and critical-boundary continuation" 26
seqp 10-leaka runs "$R1"
seqp 11-leakb runs "$R2"
mkphase "$PHASES_DIR/10-leaka.sh" "leak A" yes "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-leakb.sh" "leak B" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/12-clean.sh" "clean C" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 26
has_row leaka FAIL || bad "leaka not FAIL"
contains "$(row_msg leaka FAIL)" "ran after contaminated" && bad "the failing row itself must not be annotated"
has_row leakb FAIL || bad "leakb did not run after a critical boundary failure: $(cat "$TSV")"
contains "$(row_msg leakb FAIL)" "[ran after contaminated boundary: leaka]" || bad "leakb: $(row_msg leakb FAIL)"
has_row clean PASS || bad "clean not PASS: $(cat "$TSV")"
case "$(row_msg clean PASS)" in *" [ran after contaminated boundary: leaka leakb]") ;; *) bad "clean message: [$(row_msg clean PASS)]" ;; esac
end

# ============================================================================
# Case 27 — the summary's `## Boundary waits` section and boundary_waits.tsv: one row per
# phase that RAN with start_secs / end_secs / total_secs, then a sum line; no `## Leaks`.
# The owner read sleeps 1 s so start_secs is measurably non-zero; an unsettled run makes
# end_secs non-zero.
# Mutation: omit the section / drop a column / leave the Leaks section.
begin "case27: Boundary waits section" 27
export FAKE_OWNER_SLEEP=1
seqp 10-waita runs "$R1"
mkphase "$PHASES_DIR/10-waita.sh" "waits A" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-waitb.sh" "waits B" no "" "" "" <<'BODY'
pass "body ok"
BODY
E2E_SKIP=waitb
export E2E_SKIP
mkphase "$PHASES_DIR/12-waitc.sh" "waits C" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 27
sm="$(cat "$RUNROOT/summary.md")"
contains "$sm" "## Boundary waits" || bad "no Boundary waits section"
contains "$sm" "## Leaks" && bad "the Leaks section still exists"
contains "$sm" "| waita |" || bad "no row for waita"
[ -z "$(bw_field waitb 1)" ] || bad "a SKIPped phase must have no boundary row"
[ -n "$(bw_field waitc 4)" ] || bad "no row for waitc"
awk -F'\t' 'NF!=4{bad=1} END{exit bad+0}' "$RUNROOT/boundary_waits.tsv" || bad "boundary_waits.tsv row without 4 columns"
[ "$(bw_field waita 2)" -ge 1 ] || bad "start_secs for waita should include the 1s owner read: $(bw_field waita 2)"
[ "$(bw_field waita 3)" -ge 1 ] || bad "end_secs for waita should include the settle wait: $(bw_field waita 3)"
[ "$(bw_field waita 4)" -eq $(( $(bw_field waita 2) + $(bw_field waita 3) )) ] || bad "total_secs != start + end"
sum="$(awk -F'\t' '{s+=$4} END{print s+0}' "$RUNROOT/boundary_waits.tsv")"
contains "$sm" "Total boundary seconds: $sum" || bad "sum line missing or wrong (want $sum)"
end

# ============================================================================
# Case 28 — KNOWN residue (D3a). A leaks a run that never leaves the worker; B and C are
# otherwise clean. A FAILs after its deadline; B and C each FAIL `still present from A`
# naming the run after ONE sample (no sleep, no re-cancel), and carry the annotation.
# Mutation: wait out the deadline again at B/C, or forget the known map -> end_secs > 1,
# several samples, a second cancel.
begin "case28: known residue is sampled once and re-reported, never waited on again" 28
seqf runs "$R1"
seqf workers "$W1|$R1|1"
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bclean.sh" "B clean" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/12-cclean.sh" "C clean" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 28
has_row aleak FAIL || bad "aleak not FAIL"
[ "$(bw_field aleak 3)" -ge 1 ] || bad "A should have waited out its deadline (end_secs=$(bw_field aleak 3))"
for s in bclean cclean; do
  has_row "$s" FAIL || bad "$s did not FAIL for the known residue: $(cat "$TSV")"
  m="$(row_msg "$s" FAIL)"
  contains "$m" "still present from aleak: run=$R1" || bad "$s message: $m"
  contains "$m" "still present from aleak: worker=$W1 run=$R1 generation=1" || bad "$s does not name the worker row: $m"
  contains "$m" "[ran after contaminated boundary: aleak" || bad "$s not annotated: $m"
  [ "$(bw_field "$s" 3)" -le 1 ] || bad "$s end_secs=$(bw_field "$s" 3) > 1 (waited again)"
done
[ "$(qcount runs 11-bclean)" -eq 1 ] || bad "bclean sampled runs $(qcount runs 11-bclean) times, want 1"
[ "$(qcount runs 12-cclean)" -eq 1 ] || bad "cclean sampled runs $(qcount runs 12-cclean) times, want 1"
[ "$(qcount workers 11-bclean)" -eq 1 ] || bad "bclean sampled workers $(qcount workers 11-bclean) times, want 1"
[ "$(qcount workers 12-cclean)" -eq 1 ] || bad "cclean sampled workers $(qcount workers 12-cclean) times, want 1"
[ "$(grep -c '^cancel ' "$FAKE_API_LOG")" -eq 1 ] || bad "known run was re-cancelled: $(cat "$FAKE_API_LOG")"
end

begin "case29: a known hold in the next baseline is not exempted" 29
seqp 10-aleak holds "-" "$H1|$R1|1"
seqp 11-bclean holds "$H1|$R1|1"
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks a hold" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bclean.sh" "B clean" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 29
has_row bclean FAIL || bad "bclean did not FAIL: $(cat "$TSV")"
contains "$(row_msg bclean FAIL)" "still present from aleak: hold=$H1" || bad "message: $(row_msg bclean FAIL)"
[ "$(bw_field bclean 3)" -le 1 ] || bad "bclean waited again (end_secs=$(bw_field bclean 3))"
end

# ============================================================================
# Case 30 — a NEW identity at a known boundary still waits. A's run is known; B ALSO gets a
# new hold. Never settles -> B waits (several samples) and FAILs at the deadline naming the
# hold; settles -> B FAILs only for the known run. Known residue positively absent -> PASS.
# Mutation: skip waiting whenever anything is known / drop the absent-known pass.
begin "case30a: new hold at a known boundary waits and FAILs at the deadline" 30a
export E2E_BOUNDARY_TIMEOUT=3
seqf runs "$R1"
seqp 11-bnew holds "-" "$H1|$R2|1"
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bnew.sh" "B new hold" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 30a
has_row bnew FAIL || bad "bnew did not FAIL: $(cat "$TSV")"
contains "$(row_msg bnew FAIL)" "hold=$H1 run=$R2 generation=1" || bad "bnew does not name the new hold: $(row_msg bnew FAIL)"
contains "$(row_msg bnew FAIL)" "still present from aleak: run=$R1" || bad "bnew does not name the known run"
[ "$(qcount holds 11-bnew)" -ge 3 ] || bad "bnew did not wait for the new hold (holds reads: $(qcount holds 11-bnew), want baseline + >=2 samples)"
end

begin "case30b: new hold settles -> B FAILs only for the known run" 30b
export E2E_BOUNDARY_TIMEOUT=4
seqf runs "$R1"
seqp 11-bnew holds "-" "$H1|$R2|1" "-"
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bnew.sh" "B new hold" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 30b
has_row bnew FAIL || bad "bnew did not FAIL for the known run: $(cat "$TSV")"
contains "$(row_msg bnew FAIL)" "still present from aleak: run=$R1" || bad "message: $(row_msg bnew FAIL)"
contains "$(row_msg bnew FAIL)" "hold=" && bad "settled hold wrongly reported: $(row_msg bnew FAIL)"
[ "$(qcount holds 11-bnew)" -eq 3 ] || bad "expected baseline + 2 samples (got $(qcount holds 11-bnew))"
end

begin "case30c: known residue positively absent -> PASS (annotated)" 30c
seqp 10-aleak runs "$R1"
seqp 11-bclean runs "-"
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bclean.sh" "B clean" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 30c
has_row aleak FAIL || bad "aleak not FAIL"
has_row bclean PASS || bad "bclean should PASS once the known run is gone: $(cat "$TSV")"
contains "$(row_msg bclean PASS)" "[ran after contaminated boundary: aleak]" || bad "bclean not annotated: [$(row_msg bclean PASS)]"
end

# ============================================================================
# Case 31 — knobs: a non-positive-integer E2E_BOUNDARY_TIMEOUT falls back to 120, and the
# runtime state is left clean (no stray LEAK status anywhere: that status was removed).
# Mutation: honour 'abc' as 0 / drop the fallback -> the settlement bound is not 120.
begin "case31: invalid E2E_BOUNDARY_TIMEOUT falls back to the 120s default" 31
export E2E_BOUNDARY_TIMEOUT=abc
seqf runs "$R1" "-"
mkphase "$PHASES_DIR/10-knob.sh" "knob" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 31
has_row knob PASS || bad "run that settles should PASS: $(cat "$TSV")"
contains "$(row_msg knob PASS)" "boundary cancelled undeclared run $R1 (settled)" || bad "PASS row lost the cancel trace: [$(row_msg knob PASS)]"
f="$(qargs runs | sed -n 1p)"
{ [ "$f" -ge 119 ] && [ "$f" -le 120 ]; } || bad "first settlement bound should be the 120s default, got $f"
end

# ============================================================================
# Case 32 — a sticky error ends the settle loop after the current sample: a rejected cancel
# and a phase-start (D1) unverifiable read each FAIL after ONE sample although the run
# persists and the deadline is far away (virtual clock: end_secs stays 0).
# Mutation: break only on a failed read -> the loop keeps sampling to the deadline.
begin "case32a: rejected cancel with a persisting run ends the loop after one sample" 32a
export E2E_BOUNDARY_TIMEOUT=10 FAKE_CANCEL_CODE=403
seqf runs "$R1"
mkphase "$PHASES_DIR/10-stickycancel.sh" "rejected cancel" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 32a
has_row stickycancel FAIL || bad "not FAIL: $(cat "$TSV")"
[ "$(qcount runs)" -eq 1 ] || bad "expected exactly 1 runs sample, got $(qcount runs)"
[ "$(bw_field stickycancel 3)" -eq 0 ] || bad "end_secs should be 0, got $(bw_field stickycancel 3)"
end

begin "case32b: phase-start owner read failure with a persisting new run ends after one sample" 32b
export E2E_BOUNDARY_TIMEOUT=10
seqf owner "!1"
seqf runs "$R1"
mkphase "$PHASES_DIR/10-stickyowner.sh" "owner unreadable" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 32b
has_row stickyowner FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg stickyowner FAIL)" "unverifiable owner" || bad "message: $(row_msg stickyowner FAIL)"
[ "$(qcount runs)" -eq 1 ] || bad "expected exactly 1 runs sample, got $(qcount runs)"
end

# Case 32c — residue from the sample a sticky error ended the loop on becomes KNOWN. A's
# cancel is rejected (403) and R1 persists; B is otherwise clean and still sees R1: it FAILs
# `still present from <A>` after ONE runs sample and no wait.
# Mutation: skip the known-map append when _B_REASONS is non-empty -> B treats R1 as new,
# so B re-cancels R1 (another 403) and reports it as new, without `still present from`.
begin "case32c: residue present when a sticky error ended the loop is recorded as known" 32c
export E2E_BOUNDARY_TIMEOUT=10 FAKE_CANCEL_CODE=403
seqf runs "$R1"
mkphase "$PHASES_DIR/10-astick.sh" "A rejected cancel" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bclean.sh" "B clean" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 32c
has_row astick FAIL || bad "astick not FAIL: $(cat "$TSV")"
has_row bclean FAIL || bad "bclean not FAIL: $(cat "$TSV")"
contains "$(row_msg bclean FAIL)" "still present from astick: run=$R1" || bad "bclean message: $(row_msg bclean FAIL)"
[ "$(qcount runs 11-bclean)" -eq 1 ] || bad "bclean sampled runs $(qcount runs 11-bclean) times, want 1"
[ "$(bw_field bclean 3)" -eq 0 ] || bad "bclean end_secs should be 0, got $(bw_field bclean 3)"
end

# Case 32d — no sleep past the deadline. Timeout 3 s, interval 2 s, a worker row that
# persists: sample 1, sleep 2 (1 s left), sample 2, then ONE final in-budget sample with no
# further sleep (another interval does not fit). Three samples, end_secs 2 (sleeping the
# remainder would make it 3).
# Mutation: sleep the remaining second before giving up -> end_secs 3.
begin "case32d: no sleep after the last sample that fits" 32d
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=2
seqf workers "$W1|$R1|1"
mkphase "$PHASES_DIR/10-lastsleep.sh" "persisting worker row" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 32d
has_row lastsleep FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg lastsleep FAIL)" "not settled within 3s" || bad "message: $(row_msg lastsleep FAIL)"
[ "$(qcount workers)" -eq 3 ] || bad "expected 3 worker samples (one final in-budget sample), got $(qcount workers)"
[ "$(bw_field lastsleep 3)" -eq 2 ] || bad "end_secs should be 2 (no sleep past the deadline), got $(bw_field lastsleep 3)"
end

# Case 35a (cross-check) — residue that settles in the final, shorter-than-an-interval
# window before the deadline is observed, so the phase PASSes rather than timing out early.
# Mutation: stop as soon as remaining <= interval -> the third sample never runs -> FAIL.
begin "case35a: residue settling in the final window before the deadline PASSes" 35a
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=2
seqf workers "$W1|$R1|1" "$W1|$R1|1" "-"
mkphase "$PHASES_DIR/10-lastwin.sh" "worker row settles late" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 35a
has_row lastwin PASS || bad "settled-in-final-window phase not PASS: $(cat "$TSV")"
[ "$(qcount workers)" -eq 3 ] || bad "expected 3 worker samples, got $(qcount workers)"
end

# Case 35d — a final-window sample whose reads run out of budget (slow DB) must not erase
# the residue of the last complete sample: the leak is still named, timed out, and known.
# Mutation: let the final sample's deadline-exhausted reads stand -> message is only
# "unverifiable ... (deadline exhausted before read)", no worker named, no timeout tag.
begin "case35d: an out-of-budget final sample keeps the previous residue" 35d
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=2 FAKE_RUNS_SLEEP=1
seqf workers "$W1|$R1|1"
mkphase "$PHASES_DIR/10-slowdb.sh" "persisting worker row, slow reads" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 35d
has_row slowdb FAIL || bad "not FAIL: $(cat "$TSV")"
m="$(row_msg slowdb FAIL)"
contains "$m" "worker=$W1 run=$R1 generation=1" || bad "leaked worker row not named: $m"
contains "$m" "not settled within 3s" || bad "timeout tag missing: $m"
contains "$m" "deadline exhausted" && bad "final-window budget exhaustion reported as unverifiable: $m"
end

# Case 35e — a run FIRST seen in a complete final-window sample whose cancel times out is
# named, keeps its cancel reason, and becomes known (it must not be blamed on the next phase).
# Mutation: drop the final sample's residue/cancel reasons -> run=R2 vanishes.
begin "case35e: a new run in the final sample with a timed-out cancel is reported and known" 35e
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=2 FAKE_CANCEL_EXIT=28
seqf runs "-" "-" "$R2"
seqf workers "$W1|$R1|1" "$W1|$R1|1" "-"
mkphase "$PHASES_DIR/10-finalnew.sh" "new run in the final sample" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 35e
has_row finalnew FAIL || bad "not FAIL: $(cat "$TSV")"
m="$(row_msg finalnew FAIL)"
contains "$m" "run=$R2" || bad "final-sample run not named: $m"
contains "$m" "cancel timed out run=$R2" || bad "cancel reason dropped: $m"
contains "$(cat "$RUNROOT/boundary/10-finalnew/known-residue.txt" 2>/dev/null)" "run:$R2" || bad "run:$R2 not recorded as known"
end

# Case 35f — the final sample's runs read succeeds (new run R2) but the later reads run out of
# budget: R2 is still named (its cancel was never sent) and the out-of-budget reads fall back
# to the previous sample's rows.
begin "case35f: a new run in a partly read final sample is still reported" 35f
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=2 FAKE_RUNS_SLEEP=1
seqf runs "$R1" "$R2"
mkphase "$PHASES_DIR/10-partial.sh" "partly read final sample" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 35f
has_row partial FAIL || bad "not FAIL: $(cat "$TSV")"
m="$(row_msg partial FAIL)"
contains "$m" "run=$R2" || bad "final-sample run not named: $m"
contains "$m" "cancel not sent run=$R2" || bad "unsent cancel not reported: $m"
contains "$m" "deadline exhausted before read" && bad "read-budget reasons should become the timeout: $m"
contains "$(cat "$RUNROOT/boundary/10-partial/known-residue.txt" 2>/dev/null)" "run:$R2" || bad "run:$R2 not recorded as known"
end

# Case 35g — a declared variable holding newline-separated hold IDs declares each one.
# Mutation: drop the newline split -> only the first line is declared -> FAIL naming H2.
begin "case35g: newline-separated IDs in one declaration are each declared" 35g
seqf holds "-" "$H1|$R1|1;$H2|$R2|1"
mkphase "$PHASES_DIR/10-nl.sh" "newline list" no "" "" "" "" HOLDS_N <<BODY
HOLDS_N="$H1
$H2"
pass "body ok"
BODY
run_driver 35g
has_row nl PASS || bad "newline-separated declaration did not PASS: $(cat "$TSV")"
end

# Case 35h — a final sample whose later reads never ran cannot PASS, even when the earlier
# sample had no worker or hold rows: the boundary FAILs naming the unread reads.
# Mutation: let a budget fallback with empty residue report clean -> PASS "(settled)".
begin "case35h: an incomplete final sample FAILs even with no residue" 35h
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=3 FAKE_RUNS_SLEEP=1
seqf runs "$R2" "-"
mkphase "$PHASES_DIR/10-pone.sh" "final reads unread" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 35h
has_row pone FAIL || bad "incomplete final sample did not FAIL: $(cat "$TSV")"
contains "$(row_msg pone FAIL)" "final sample incomplete: workers holds unread" || bad "message: $(row_msg pone FAIL)"
contains "$(row_msg pone FAIL)" "not settled within 3s" || bad "timeout tag missing: $(row_msg pone FAIL)"
end

# Case 35i — an out-of-budget final holds read falls back to the previous holds rows, so the
# release history still covers a baseline hold that was no longer open.
# Mutation: drop the holds fallback -> history omits the released baseline hold.
begin "case35i: holds fallback keeps the baseline-release history" 35i
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=3 FAKE_RUNS_SLEEP=1
seqf holds "$H1|$R1|1" "-"
seqf workers "$W1|$R1|1"
seqf history "$H1|$R1|1|released|ok|x"
mkphase "$PHASES_DIR/10-ptwo.sh" "holds read out of budget" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 35i
has_row ptwo FAIL || bad "not FAIL: $(cat "$TSV")"
contains "$(row_msg ptwo FAIL)" "hold=$H1" || bad "history lacks baseline hold H1: $(row_msg ptwo FAIL)"
end

# Case 35b (cross-check) — one handoff-holds variable may hold several whitespace-separated
# hold UUIDs; each is declared. A non-UUID token in the list is still malformed.
# Mutation: validate the whole value as one UUID -> the two-ID phase FAILs as malformed.
begin "case35b: a handoff-holds variable with several hold IDs declares each" 35b
seqf holds "-" "$H1|$R1|1;$H2|$R2|1"
mkphase "$PHASES_DIR/10-multi.sh" "two declared holds" no "" "" "" "" HOLDS_X <<BODY
HOLDS_X="$H1 $H2"
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-badtok.sh" "one bad token" no "" "" "" "" HOLDS_Y <<BODY
HOLDS_Y="$H1 not-a-uuid"
pass "body ok"
BODY
run_driver 35b
has_row multi PASS || bad "two declared holds did not PASS: $(cat "$TSV")"
has_row badtok FAIL || bad "bad token not FAIL: $(cat "$TSV")"
contains "$(row_msg badtok FAIL)" "malformed declaration HOLDS_Y" || bad "message: $(row_msg badtok FAIL)"
end

# Case 35c (cross-check) — a KNOWN hold that released before a later failed boundary is still
# in that boundary's release-history read (it is neither residue nor baseline any more).
# Mutation: build the history set from current residue and baseline only -> H1 missing.
begin "case35c: release history includes known holds that have disappeared" 35c
export E2E_BOUNDARY_TIMEOUT=2
seqp 10-aleak holds "-" "$H1|$R1|1"
seqp 11-bleak holds "-"
seqp 11-bleak workers "$W1|$R2|1"
seqf history "$H1|$R1|1|released|published|ev-1"
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks a hold" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bleak.sh" "B leaks a worker row" no "" "" "" <<'BODY'
pass "body ok"
BODY
run_driver 35c
has_row bleak FAIL || bad "bleak not FAIL: $(cat "$TSV")"
contains "$(awk -F'\t' '$2=="11-bleak" && $3=="history"{print $6}' "$QL")" "'$H1'" || bad "B's history read omits the known hold $H1"
end

# Case 32e — a failed body whose boundary is clean keeps the boundary's settled-cancel note
# in the FAIL row's message.
# Mutation: fmsg="$msg" for a clean boundary -> the note is dropped.
begin "case32e: failed body with a clean boundary keeps the settled-cancel note" 32e
seqf runs "$R1" "-"
mkphase "$PHASES_DIR/10-bodyfail.sh" "body fails" no "" "" "" <<'BODY'
fail "body boom"
BODY
run_driver 32e
has_row bodyfail FAIL || bad "not FAIL: $(cat "$TSV")"
m="$(row_msg bodyfail FAIL)"
contains "$m" "body boom" || bad "body message lost: $m"
contains "$m" "boundary cancelled undeclared run $R1 (settled)" || bad "settled-cancel note dropped: $m"
contains "$m" "; boundary:" && bad "clean boundary reported as failed: $m"
end

# ============================================================================
# Case 33 — a requires-miss FAIL row never ran a body, so (like a SKIP) it is not annotated
# as having run after a contaminated boundary.
# Mutation: annotate every non-SKIP row.
begin "case33: requires-miss FAIL after a contaminated boundary is not annotated" 33
seqp 10-dirty runs "$R1"
mkphase "$PHASES_DIR/10-dirty.sh" "leaks" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-needs.sh" "needs FOO" no FOO "" "" <<'BODY'
pass "never runs"
BODY
run_driver 33
has_row dirty FAIL || bad "dirty not FAIL: $(cat "$TSV")"
has_row needs FAIL || bad "needs not FAIL: $(cat "$TSV")"
contains "$(row_msg needs FAIL)" "ran after contaminated" && bad "requires-miss row must not be annotated: $(row_msg needs FAIL)"
end

# ============================================================================
# Case 34 — the top-level unset of declared names before the body: A provides and hands
# forward RUN_X; B declares `handoff: RUN_X` without setting it while that run is still
# residue -> B must FAIL (the value inherited from A must not count as B's declaration).
# Mutation: remove the unset loop in driver.sh -> B reads PASS.
begin "case34: a name declared by B but not set by B does not inherit A's value" 34
seqp 10-ahands runs "$R1"
seqp 11-bnoset runs "$R1"
mkphase "$PHASES_DIR/10-ahands.sh" "A hands RUN_X" no "" RUN_X RUN_X <<BODY
RUN_X=$R1
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bnoset.sh" "B declares but never sets" no "" "" RUN_X <<'BODY'
pass "body ok"
BODY
run_driver 34
has_row ahands PASS || bad "ahands not PASS: $(cat "$TSV")"
has_row bnoset FAIL || bad "bnoset did not FAIL (inherited declaration): $(cat "$TSV")"
contains "$(row_msg bnoset FAIL)" "run=$R1" || bad "bnoset message: $(row_msg bnoset FAIL)"
end

# ============================================================================
# A later declaration cannot forgive residue an earlier failed boundary reported.
begin "case36a: known hold stays residue despite a later handoff-holds declaration" 36a
seqp 10-aleak holds "-" "$H1|$R1|1"
seqp 11-bdeclare holds "$H1|$R1|1"
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bdeclare.sh" "B declares known hold" no "" "" "" "" OWNED_HOLD <<BODY
OWNED_HOLD=$H1
pass "body ok"
BODY
run_driver 36a
has_row aleak FAIL || bad "setup did not report A's hold leak: $(cat "$TSV")"
has_row bdeclare FAIL || bad "B forgave known hold: $(cat "$TSV")"
contains "$(row_msg bdeclare FAIL)" "still present from aleak: hold=$H1" || bad "B lost the hold's origin: $(row_msg bdeclare FAIL)"
[ "$(qcount holds 11-bdeclare)" -eq 2 ] || bad "B did not take only baseline plus one hold sample"
[ "$(bw_field bdeclare 3)" -le 1 ] || bad "B waited again for the known hold"
end

begin "case36b: known run and worker stay residue despite a later handoff declaration" 36b
for ph in 10-aleak 11-bdeclare; do
  seqp "$ph" runs "$R1"
  seqp "$ph" workers "$W1|$R1|1"
done
mkphase "$PHASES_DIR/10-aleak.sh" "A leaks" no "" "" "" <<'BODY'
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-bdeclare.sh" "B declares known run" no "" "" OWNED_RUN <<BODY
OWNED_RUN=$R1
pass "body ok"
BODY
run_driver 36b
has_row aleak FAIL || bad "setup did not report A's run leak: $(cat "$TSV")"
has_row bdeclare FAIL || bad "B forgave known run: $(cat "$TSV")"
contains "$(row_msg bdeclare FAIL)" "still present from aleak: run=$R1" || bad "B lost the run's origin: $(row_msg bdeclare FAIL)"
contains "$(row_msg bdeclare FAIL)" "still present from aleak: worker=$W1 run=$R1 generation=1" || bad "B lost the worker's origin: $(row_msg bdeclare FAIL)"
[ "$(qcount runs 11-bdeclare)" -eq 1 ] && [ "$(qcount workers 11-bdeclare)" -eq 1 ] || bad "B sampled known run/worker more than once"
[ "$(grep -c '^cancel ' "$FAKE_API_LOG")" -eq 1 ] || bad "B cancelled the known run again"
[ "$(bw_field bdeclare 3)" -le 1 ] || bad "B waited again for the known run/worker"
end

begin "case37a: a known run whose cancellation was unsent gets one later attempt" 37a
export E2E_BOUNDARY_TIMEOUT=3 E2E_BOUNDARY_INTERVAL=2 FAKE_RUNS_SLEEP=1
seqp 10-partial runs "$R1" "$R2"
seqp 11-retry runs "$R2"
seqp 12-later runs "$R2"
for ph in 10-partial 11-retry 12-later; do
  mkphase "$PHASES_DIR/$ph.sh" "$ph" no "" "" "" <<'BODY'
pass "body ok"
BODY
done
run_driver 37a
contains "$(row_msg partial FAIL)" "cancel not sent run=$R2" || bad "setup did not exhaust the cancellation budget"
[ "$(grep -c "cancel /api/runs/$R2/inputs" "$FAKE_API_LOG")" -eq 1 ] || bad "unsent cancellation not attempted exactly once later"
contains "$(row_msg retry FAIL)" "still present from partial: run=$R2" || bad "lost original attribution"
[ "$(qcount runs 11-retry)" -eq 1 ] || bad "waited again for known run"
end

begin "case37b: a non-final exhausted sample preserves the previous worker rows" 37b
export E2E_BOUNDARY_TIMEOUT=4 E2E_BOUNDARY_INTERVAL=1
seqp 10-slow runs "-" "-"
seqp 10-slow runs-cost 0 3
seqp 10-slow workers "$W1|$R1|1"
seqp 11-later workers "$W1|$R1|1"
for ph in 10-slow 11-later; do
  mkphase "$PHASES_DIR/$ph.sh" "$ph" no "" "" "" <<'BODY'
pass "body ok"
BODY
done
run_driver 37b
contains "$(row_msg slow FAIL)" "worker=$W1 run=$R1 generation=1" || bad "exhausted sample erased worker"
contains "$(row_msg later FAIL)" "still present from slow: worker=$W1" || bad "worker was not remembered"
[ "$(qcount workers 11-later)" -eq 1 ] || bad "later boundary waited again"
end

begin "case37c: unavailable staging still records the phase FAIL and summary" 37c
export FAKE_STAGE_FAILURE=yes
mkphase "$PHASES_DIR/10-stage.sh" "staging failure" no "" "" "" <<'BODY'
pass "body still runs"
BODY
run_driver 37c
has_row stage FAIL || bad "missing FAIL row: $(cat "$TSV")"
contains "$(row_msg stage FAIL)" "staging unavailable" || bad "staging failure not named"
contains "$(cat "$LOG")" "body still runs" || bad "body skipped"
[ -f "$RUNROOT/junit.xml" ] || bad "summary was abandoned"
end

begin "case37d: failed known-residue evidence write does not abandon results" 37d
seqf holds "-" "$H1|$R1|1"
mkphase "$PHASES_DIR/10-stage.sh" "staging write failure" no "" "" "" <<'BODY'
mkdir "$RUNROOT/boundary/10-stage/known-residue.txt"
pass "body ok"
BODY
run_driver 37d
has_row stage FAIL || bad "missing FAIL row: $(cat "$TSV")"
contains "$(row_msg stage FAIL)" "evidence write failed" || bad "evidence failure not named"
[ -f "$RUNROOT/junit.xml" ] || bad "summary was abandoned"
end

begin "case37e: an unset required handoff clears its inherited value in the parent" 37e
seqp 10-first runs "$R1"
seqp 11-unsets runs "$R1"
mkphase "$PHASES_DIR/10-first.sh" "retain a handoff" no "" RUN_X RUN_X <<BODY
RUN_X=$R1
pass "body ok"
BODY
mkphase "$PHASES_DIR/11-unsets.sh" "unset a required handoff" no RUN_X "" RUN_X <<'BODY'
unset RUN_X
pass "body ok"
BODY
run_driver 37e
has_row first PASS || bad "retained handoff did not round-trip"
has_row unsets FAIL || bad "unset handoff was still exempted"
contains "$(row_msg unsets FAIL)" "run=$R1" || bad "unset run not named"
[ "$(grep -c '^cancel ' "$FAKE_API_LOG")" -eq 1 ] || bad "unset handoff did not lose its exemption"
end

printf '\ncases=%s passed=%s\n' "$cases" "$passed"
[ "$cases" -eq "$passed" ] || exit 1
