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

# db_psql: a SELECT of non-terminal runs echoes FAKE_DB_IDS (one id per line);
# any UPDATE (the forced-cancel fallback) is logged to FAKE_DB_LOG.
db_psql() {
  case "$1" in
    *"status not in"*)
      # shellcheck disable=SC2086  # deliberate split: one id per line
      printf '%s\n' ${FAKE_DB_IDS:-} ;;
    *"update runs"*)
      printf '%s\n' "$1" >> "${FAKE_DB_LOG:-/dev/null}" ;;
  esac
}
apipost() { printf 'apipost %s\n' "$1" >> "${FAKE_API_LOG:-/dev/null}"; printf '{}'; }
# apipost_code performs the cancel POST AND returns the HTTP status (FAKE_CANCEL_CODE).
apipost_code() { printf 'cancel %s\n' "$1" >> "${FAKE_API_LOG:-/dev/null}"; printf '%s' "${FAKE_CANCEL_CODE:-200}"; }
wait_status() { :; }
FAKES_EOF

# --- fixture writer ----------------------------------------------------------
# mkphase PATH TITLE CRITICAL REQUIRES PROVIDES HANDOFF  <<'BODY' ... BODY
# Header via printf (needs the arg values); body via the caller's QUOTED heredoc
# on stdin so `$FOO` in a body stays literal until the phase runs.
mkphase() {
  local path="$1" title="$2" crit="$3" req="$4" prov="$5" hand="$6" slug
  slug="$(basename "$path" .sh | sed 's/^[0-9][0-9]-//')"
  {
    printf '# shellcheck shell=bash\n'
    printf '# phase:    %s\n' "$slug"
    printf '# title:    %s\n' "$title"
    printf '# critical: %s\n' "$crit"
    printf '# lane:     gitlab\n'
    printf '# executor: any\n'
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
  unset E2E_ONLY E2E_SKIP E2E_STRICT_LEAKS E2E_FAULT_PHASE FAKE_DB_IDS FAKE_CANCEL_CODE 2>/dev/null || true
}
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
  ( source "$FAKES"; source "$DRIVER" ) > "$LOG" 2>&1
  RC=$?
  set -e
}
# Shared globals the driver reads — exported because they are consumed by the
# driver sourced INSIDE run_driver's subshell (which shellcheck cannot see); the
# same reason applies to the per-case E2E_*/FAKE_* knobs set below.
export ROOT="$REPO_ROOT" FORGE=gitlab EXECUTOR=stub

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
# Case 6 — quarantine: an undeclared non-terminal run is cancelled and recorded
# LEAK; a handoff-declared id is not; a refused API cancel falls back to DB.
# Mutation: skip the cancel/record -> no LEAK row / no cancel logged.
begin "case6a: undeclared run leaks and is cancelled" 6a
export FAKE_DB_IDS="r-leak" FAKE_CANCEL_CODE=200
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 6a
has_row leaky LEAK || bad "no LEAK row for leaky"
contains "$(row_msg leaky LEAK)" r-leak || bad "LEAK message does not name r-leak"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/r-leak/inputs" || bad "cancel was not attempted via the API"
end

begin "case6b: handoff-declared id is not a leak" 6b
export FAKE_DB_IDS="r-held" FAKE_CANCEL_CODE=200
mkphase "$PHASES_DIR/10-held.sh" "hands a run forward" no "" RUN_B RUN_B <<'BODY'
RUN_B=r-held
pass "declared handoff RUN_B"
BODY
run_driver 6b
any_status LEAK && bad "handoff id was wrongly recorded as a LEAK"
[ -s "$FAKE_API_LOG" ] && bad "a cancel was attempted for a declared handoff id"
has_row held PASS || bad "held phase not PASS"
end

begin "case6c: refused API cancel falls back to DB" 6c
export FAKE_DB_IDS="r-leak" FAKE_CANCEL_CODE=403
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 6c
has_row leaky LEAK || bad "no LEAK row for leaky"
contains "$(row_msg leaky LEAK)" "forced via DB" || bad "LEAK message does not note the DB fallback"
contains "$(cat "$FAKE_DB_LOG")" "update runs set status='cancelled' where id='r-leak'" || bad "DB fallback update did not run"
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
printf '\ncases=%s passed=%s\n' "$cases" "$passed"
[ "$cases" -eq "$passed" ] || exit 1
