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
    *"group by status"*)
      printf 'FAKE_DB_COUNTS running issue 1\n' ;;
  esac
}
# db_psql_rows: the row-preserving twin (lib.sh) the driver now uses for the leak
# sweep and the multi-row artifact dumps. Mirrors db_psql's read cases (there is no
# write case here — the forced-cancel UPDATE stays on scalar db_psql). The leak-sweep
# case emits ONE id PER LINE so a multi-id FAKE_DB_IDS ("r1 r2") yields two rows, which
# is what the row-preservation cases assert on.
db_psql_rows() {
  case "$1" in
    *"status not in"*)
      # shellcheck disable=SC2086  # deliberate split: one id per line
      printf '%s\n' ${FAKE_DB_IDS:-} ;;
    *"from runs order by"*)
      printf 'FAKE_DB_RUNS r1 admin running issue\n' ;;
    *"group by status"*)
      printf 'FAKE_DB_COUNTS running issue 1\n' ;;
  esac
}
apipost() { printf 'apipost %s\n' "$1" >> "${FAKE_API_LOG:-/dev/null}"; printf '{}'; }
# apipost_code performs the cancel POST AND returns the HTTP status (FAKE_CANCEL_CODE).
apipost_code() { printf 'cancel %s\n' "$1" >> "${FAKE_API_LOG:-/dev/null}"; printf '%s' "${FAKE_CANCEL_CODE:-200}"; }
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
  unset E2E_ONLY E2E_SKIP E2E_STRICT_LEAKS E2E_FAULT_PHASE FAKE_DB_IDS FAKE_CANCEL_CODE FAKE_DOCKER_SEQ 2>/dev/null || true
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
# Mutation: capture on every phase (not just FAIL/LEAK) -> artifacts/ appears green.
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
# Case 15 (PRD #966 M1) — a LEAK must not abort the results/roll-call/exit path.
# _write_summary's final `[ "$any" = 0 ] && …` returns 1 when a LEAK exists (any=1);
# without the unconditional `return 0`, the bare `_write_summary` call under the
# driver's `set -e` aborts BEFORE `cat summary.md`, `_rollcall` and the exit-code loop.
# Mutation: revert _write_summary to end on the bare `[ "$any" = 0 ] && …` -> the
# roll-call line is absent from the log and the run exits non-zero.
begin "case15: a LEAK still writes summary + runs roll-call/exit" 15
export FAKE_DB_IDS="r-leak15" FAKE_CANCEL_CODE=200
mkphase "$PHASES_DIR/10-leaky.sh" "leaks a run" no "" "" "" <<'BODY'
pass "left a run behind"
BODY
run_driver 15
has_row leaky LEAK || bad "no LEAK row for leaky"
[ -f "$RUNROOT/summary.md" ] || bad "summary.md was not written on a leaky run"
contains "$(cat "$LOG")" "All E2E checks passed." \
  || bad "roll-call did not run after a LEAK (driver aborted at _write_summary)"
[ "$RC" -eq 0 ] || bad "a non-strict LEAK run must exit 0 (got $RC — driver aborted mid-way?)"
end

# ============================================================================
# Case 16 (PRD #966 M1) — TWO undeclared leaked rows are each cancelled and recorded
# as a SEPARATE LEAK row. Guards the db_psql -> db_psql_rows switch in the sweep:
# db_psql collapses newlines (tr -d '\r\n'), fusing 2+ ids into one garbage token, so
# only db_psql_rows (one row per line) yields two distinct LEAK rows + two cancels.
# Mutation: switch the sweep query back to db_psql -> the two ids fuse into one LEAK
# row for a bogus fused id, and only one cancel is attempted.
begin "case16: two leaked rows record as two separate LEAKs" 16
export FAKE_DB_IDS="r-leak-a r-leak-b" FAKE_CANCEL_CODE=200
mkphase "$PHASES_DIR/10-leaky.sh" "leaks two runs" no "" "" "" <<'BODY'
pass "left two runs behind"
BODY
run_driver 16
leak_n="$(awk -F'\t' '$1=="leaky" && $2=="LEAK"{n++} END{print n+0}' "$TSV")"
[ "$leak_n" -eq 2 ] || bad "expected 2 separate LEAK rows, got $leak_n (rows fused?)"
contains "$(cat "$TSV")" "r-leak-a" || bad "LEAK rows do not name r-leak-a (rows fused into one token)"
contains "$(cat "$TSV")" "r-leak-b" || bad "LEAK rows do not name r-leak-b (rows fused into one token)"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/r-leak-a/inputs" || bad "r-leak-a not cancelled via the API"
contains "$(cat "$FAKE_API_LOG")" "cancel /api/runs/r-leak-b/inputs" || bad "r-leak-b not cancelled via the API"
end

# ============================================================================
# Case 17 (#990) — a FAIL+LEAK phase captures artifacts TWICE for the same slug (the
# FAIL path, then the quarantine sweep with secs=0 -> a 30s window). The FAIL capture's
# window is strictly wider, so the second pass must NOT overwrite the failing-phase
# container logs. FAKE_DOCKER_SEQ stamps a monotonic call number: the FAIL capture is
# seq 1-4 (api,agent,forge-fake,db), a would-be LEAK re-capture seq 5-8. api.log must
# keep the FIRST capture (seq=1), never the overwriting one (seq=5).
# Mutation: revert driver.sh's `[ -d "$dir" ] && return 0` guard -> api.log becomes
# seq=5 (the 30s LEAK window that dropped the failing-phase logs in the real bug).
begin "case17: FAIL+LEAK capture is not overwritten by the leak sweep" 17
export FAKE_DB_IDS="r-leak17" FAKE_CANCEL_CODE=200 FAKE_DOCKER_SEQ="$TMP/dseq-17"
rm -f "$FAKE_DOCKER_SEQ"
mkphase "$PHASES_DIR/10-failleak.sh" "fail and leak" no "" "" "" <<'BODY'
fail "boom and leak"
BODY
run_driver 17
adir="$RUNROOT/artifacts/10-failleak"
has_row failleak FAIL || bad "failleak not FAIL"
has_row failleak LEAK || bad "no LEAK row — the double-capture scenario did not reproduce"
[ -f "$adir/api.log" ] || bad "api.log not captured"
contains "$(cat "$adir/api.log" 2>/dev/null)" "seq=1" \
  || bad "api.log is not the FAIL-path (first) capture: $(cat "$adir/api.log" 2>/dev/null)"
contains "$(cat "$adir/api.log" 2>/dev/null)" "seq=5" \
  && bad "api.log was OVERWRITTEN by the leak-sweep re-capture (#990): $(cat "$adir/api.log" 2>/dev/null)"
end

# ============================================================================
printf '\ncases=%s passed=%s\n' "$cases" "$passed"
[ "$cases" -eq "$passed" ] || exit 1
