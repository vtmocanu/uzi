#!/usr/bin/env bash
# Hermetic regression for e2e/lib.sh's _psql_strip_tags (#1351). A psql DML command-completion tag
# (`INSERT 0 1`, `UPDATE 1`, `DELETE n`, `MERGE n`) welded onto a db_psql scalar read — either same
# statement (`INSERT … RETURNING id`) or bled cross-invocation from a prior phase's discarded DML —
# must be stripped, while a legitimate scalar passes through untouched. No stack, no docker: it pulls
# the two REAL definitions out of lib.sh (which cannot be sourced here — its top level provisions the
# whole e2e stack) and drives them over stdin. Also drives lib.sh's wait_regated (#1739) over a
# scripted apiget, plus evidence-preserving custody admission and forge-park waits.
# Prints PASS:/FAIL: per case and a MANDATORY
# `cases=N passed=N` tally, so a crashed or gutted zero-case run cannot read green.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="$ROOT/e2e/lib.sh"
[ -f "$LIB" ] || { echo "lib.sh not found at $LIB" >&2; exit 2; }

# Extract ONLY the two single-line tag-strip definitions (the RE var and the function) from the
# shipped lib.sh and eval them. This exercises the real code; if the fix is reverted (the function
# removed, back to an inline `tr -d '\r\n'`) the extraction yields nothing and the guard below fails.
# NOTE: this couples to _psql_strip_tags being ONE line in lib.sh; a future multi-line reformat would
# extract a truncated body — it fails safe (the guard/cases go red), so keep the def single-line.
eval "$(awk '/^_PSQL_TAG_RE=/ || /^_psql_strip_tags\(\)/' "$LIB")"
if ! type _psql_strip_tags >/dev/null 2>&1; then
  echo "FAIL: _psql_strip_tags not defined — extraction failed or the tag-strip fix was reverted" >&2
  echo "cases=1 passed=0"
  exit 1
fi

cases=0
passed=0
# check NAME INPUT EXPECT — INPUT is a printf %b format (so \n and \r are real), piped through the
# real _psql_strip_tags and then collapsed like db_psql's scalar path (`| tr -d '\n'`).
check() {
  cases=$((cases + 1))
  local name="$1" got
  got="$(printf '%b' "$2" | _psql_strip_tags | tr -d '\n')"
  if [ "$got" = "$3" ]; then
    passed=$((passed + 1))
    echo "PASS: $name"
  else
    echo "FAIL: $name — got [$got] want [$3]"
  fi
}

UUID=b4dec5cf-e6d4-413b-b3a8-0005a2835767
check "INSERT tag welded after the value"   "$UUID\nINSERT 0 1\n"  "$UUID"
check "INSERT tag bled before the value"    "INSERT 0 1\n$UUID\n"  "$UUID"
check "UPDATE tag stripped"                 "5\nUPDATE 1\n"        "5"
check "DELETE tag stripped"                 "2\nDELETE 3\n"        "2"
check "MERGE tag stripped"                  "$UUID\nMERGE 2\n"     "$UUID"
check "clean uuid untouched"                "$UUID\n"              "$UUID"
check "numeric scalar untouched"            "55\n"                 "55"
check "enum scalar untouched"               "usage_endpoint\n"     "usage_endpoint"
check "CRLF-terminated tag stripped"        "abc\r\nINSERT 0 1\r\n" "abc"
check "non-tag INSERT-ish text kept"        "INSERT INTO foo\n"    "INSERT INTO foo"

# Chokepoint guard (#1511): the strip only helps callers that go through it. A phase that defines
# its own `… db psql … | tr -d '\r\n'` helper re-welds `<id>UPDATE 1` on a DML RETURNING read, as
# the forgejo/github lane flips once did. Every phase must read the DB via db_psql/db_psql_rows.
cases=$((cases + 1))
raw="$(grep -nF 'db psql' "$ROOT"/e2e/phases/*.sh || true)"
if [ -z "$raw" ]; then
  passed=$((passed + 1))
  echo "PASS: no phase invokes psql outside lib.sh's db_psql/db_psql_rows"
else
  echo "FAIL: phases invoke psql directly (use db_psql/db_psql_rows):"
  printf '%s\n' "$raw"
fi

# wait_regated (#1739): after a restart the pre-disruption row still reads awaiting_approval, so
# only a NEW claimed_at on an awaiting_approval row proves a new claim re-showed the gate. The real
# multi-line definition is extracted from lib.sh and driven by a scripted apiget: each call returns
# the next run DTO from the sequence, sticking on the last.
eval "$(awk '/^wait_regated\(\) \{/,/^\}/' "$LIB")"
cases=$((cases + 1))
if type wait_regated >/dev/null 2>&1; then
  passed=$((passed + 1))
  echo "PASS: wait_regated extracted from lib.sh"
else
  echo "FAIL: wait_regated not defined — extraction failed or the helper was removed"
fi
mkdir -p "$ROOT/.uzi/scratch"
SEQ_DIR="$(mktemp -d "$ROOT/.uzi/scratch/e2e-lib.XXXXXX")"
trap 'rm -rf "$SEQ_DIR"' EXIT

# compose_build_logged (#3468): extract the shipped multiline helper without
# sourcing lib.sh's stack setup. A missing helper counts as a failure, then lets
# the other regression groups continue producing useful diagnostics.
eval "$(awk '/^compose_build_logged\(\) \{/,/^\}/' "$LIB")"
cases=$((cases + 1))
if type compose_build_logged >/dev/null 2>&1; then
  passed=$((passed + 1))
  echo "PASS: compose_build_logged extracted from lib.sh"
else
  echo "FAIL: compose_build_logged not defined — extraction failed or build logging fix is absent"
fi
build_check() {
  cases=$((cases + 1))
  local name="$1"; shift
  if "$@"; then
    passed=$((passed + 1))
    echo "PASS: $name"
  else
    echo "FAIL: $name"
  fi
}
if type compose_build_logged >/dev/null 2>&1; then
  # Each invocation is synchronous; the only loop emits exactly 105 fixture lines.
  fake_build_compose() {
    printf '%s\n' "$@" > "$BUILD_FIXTURE/argv"
    if [ "${BUILD_LINES:-0}" -eq 1 ]; then
      local i
      for ((i = 1; i <= 105; i++)); do
        printf 'build-line-%03d\n' "$i"
      done
    else
      printf 'build stdout sentinel\n'
      printf 'build stderr sentinel\n' >&2
    fi
    return "$BUILD_EXIT"
  }
  (
    BUILD_FIXTURE="$SEQ_DIR/compose-build"
    mkdir -p "$BUILD_FIXTURE"
    RUNROOT="$BUILD_FIXTURE/failure run"
    COMPOSE=(fake_build_compose --project-name "fixture project" --file "fixture compose.yml")
    printf '%s\n' --project-name "fixture project" --file "fixture compose.yml" --progress=plain build > "$BUILD_FIXTURE/expected-argv"
    printf 'build stdout sentinel\nbuild stderr sentinel\n' > "$BUILD_FIXTURE/expected-log"
    BUILD_EXIT=3
    build_rc=0
    compose_build_logged > "$BUILD_FIXTURE/stdout" 2> "$BUILD_FIXTURE/stderr" || build_rc=$?
    build_check "failed build preserves exit 3" test "$build_rc" -eq 3
    build_check "failed build creates artifact directory automatically" test -d "$RUNROOT/artifacts/build"
    build_check "failed build retains both output streams" cmp -s "$BUILD_FIXTURE/expected-log" "$RUNROOT/artifacts/build/compose-build.log"
    build_check "failed build leaves terminal stdout empty" test ! -s "$BUILD_FIXTURE/stdout"
    build_check "failed build prints log only to terminal stderr" cmp -s "$BUILD_FIXTURE/expected-log" "$BUILD_FIXTURE/stderr"
    build_check "compose globals precede plain progress and build in exact argv order" cmp -s "$BUILD_FIXTURE/expected-argv" "$BUILD_FIXTURE/argv"

    RUNROOT="$BUILD_FIXTURE/success run"
    BUILD_EXIT=0
    build_rc=0
    compose_build_logged > "$BUILD_FIXTURE/stdout" 2> "$BUILD_FIXTURE/stderr" || build_rc=$?
    build_check "successful build returns zero" test "$build_rc" -eq 0
    build_check "successful build retains both output streams" cmp -s "$BUILD_FIXTURE/expected-log" "$RUNROOT/artifacts/build/compose-build.log"
    build_check "successful build leaves terminal stdout empty" test ! -s "$BUILD_FIXTURE/stdout"
    build_check "successful build leaves terminal stderr empty" test ! -s "$BUILD_FIXTURE/stderr"

    RUNROOT="$BUILD_FIXTURE/long run"
    BUILD_EXIT=3 BUILD_LINES=1
    # Independently specify the expected last 80 of 105 deterministic lines.
    for ((i = 26; i <= 105; i++)); do
      printf 'build-line-%03d\n' "$i"
    done > "$BUILD_FIXTURE/expected-tail"
    compose_build_logged > "$BUILD_FIXTURE/stdout" 2> "$BUILD_FIXTURE/stderr" || :
    build_check "failed build prints exactly the last 80 lines" cmp -s "$BUILD_FIXTURE/expected-tail" "$BUILD_FIXTURE/stderr"

    # The helper is standalone in a fresh Bash with errexit, not inside an
    # if/OR-list that would disable errexit throughout a called function.
    declare -f compose_build_logged fake_build_compose > "$BUILD_FIXTURE/definitions.sh"
    build_rc=0
    bash -c '
      set -e
      source "$1"
      RUNROOT="$2"
      BUILD_FIXTURE="$3"
      BUILD_EXIT=3
      COMPOSE=(fake_build_compose --project-name "fixture project")
      compose_build_logged
      printf "post-helper marker\n" > "$BUILD_FIXTURE/post-helper"
    ' _ "$BUILD_FIXTURE/definitions.sh" "$BUILD_FIXTURE/errexit run" "$BUILD_FIXTURE" > "$BUILD_FIXTURE/child-stdout" 2> "$BUILD_FIXTURE/child-stderr" || build_rc=$?
    build_check "standalone helper under child Bash errexit exits 3" test "$build_rc" -eq 3
    build_check "child Bash errexit prevents post-helper marker" test ! -e "$BUILD_FIXTURE/post-helper"

    BUILD_LINES=0
    tail() { return 7; }
    build_rc=0
    compose_build_logged > "$BUILD_FIXTURE/stdout" 2> "$BUILD_FIXTURE/stderr" || build_rc=$?
    build_check "tail failure does not replace build exit 3" test "$build_rc" -eq 3
    # Preserve the parent tally while keeping all fixture globals in this subshell.
    printf '%s %s\n' "$cases" "$passed" > "$BUILD_FIXTURE/tally"
  )
  read -r cases passed < "$SEQ_DIR/compose-build/tally"
fi
record_margin() { :; }
fail() { echo "$*" >&2; exit 1; }
apiget() {
  local n; n="$(cat "$SEQ_DIR/n")"
  sed -n "${n}p" "$SEQ_DIR/seq"
  [ "$n" -ge "$(wc -l < "$SEQ_DIR/seq")" ] || echo $((n + 1)) > "$SEQ_DIR/n"
}
# Bash's SECONDS loses its wall-clock behavior when unset. Reset it inside each test's
# subshell, then advance it on every poll without waiting in real time.
sleep() { SECONDS=$((SECONDS + 1)); }
# regate NAME WANT(pass|fail) DIAGNOSTIC_PREFIX STATUS:CLAIMED_AT...
regate() {
  cases=$((cases + 1))
  local name="$1" want="$2" diagnostic="$3" got output timeout=2; shift 3
  [ "$want" = pass ] && timeout=5
  : > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
  for step in "$@"; do
    jq -nc --arg s "${step%%:*}" --arg c "${step#*:}" '{run: {status: $s, claimed_at: (if $c == "" then null else $c end)}}' >> "$SEQ_DIR/seq"
  done
  if output="$(unset SECONDS; SECONDS=0; wait_regated R T0 "$timeout" 2>&1)"; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ] && [[ "$output" == "$diagnostic"* ]]; then
    passed=$((passed + 1)); echo "PASS: $name"
  else
    echo "FAIL: $name — got $got want $want; output [$output] (want prefix [$diagnostic])"
  fi
}
regate "stale pre-restart gate row never passes"  fail "timeout: run R was never re-gated" awaiting_approval:T0
regate "requeued then re-gated by a new claim"      pass "" awaiting_approval:T0 queued: running:T1 awaiting_approval:T1
regate "new claim not yet at the gate"              fail "timeout: run R was never re-gated" awaiting_approval:T0 running:T1
regate "gate row with no claimed_at"                fail "timeout: run R was never re-gated" awaiting_approval:
regate "terminal failure while waiting"             fail "run R entered 'failed'" awaiting_approval:T0 failed:T1
regate "cancelled while waiting"                    fail "run R entered 'cancelled'" awaiting_approval:T0 cancelled:

# settle_runs_terminal: best-effort, so it must return 0 both when a run settles and when it
# never does, and it must stop polling a run once that run is terminal. The virtual clock
# makes the never-terminal timeout require exactly five status reads.
eval "$(awk '/^settle_runs_terminal\(\) \{/,/^\}/' "$LIB")"
# run_status_quick stub: serves the scripted statuses in order (sticking on the last) and counts calls.
run_status_quick() {
  local n calls
  n="$(cat "$SEQ_DIR/n")"; calls="$(cat "$SEQ_DIR/calls")"
  echo $((calls + 1)) > "$SEQ_DIR/calls"
  sed -n "${n}p" "$SEQ_DIR/seq"
  [ "$n" -ge "$(wc -l < "$SEQ_DIR/seq")" ] || echo $((n + 1)) > "$SEQ_DIR/n"
}
# settle NAME WANT_CALLS STATUS... — one run (plus an empty id, which must be skipped).
settle() {
  cases=$((cases + 1))
  local name="$1" want="$2" rc calls; shift 2
  : > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"; echo 0 > "$SEQ_DIR/calls"
  printf '%s\n' "$@" > "$SEQ_DIR/seq"
  ( unset SECONDS; SECONDS=0; settle_runs_terminal 5 R "" ); rc=$?
  calls="$(cat "$SEQ_DIR/calls")"
  if [ "$rc" = 0 ] && [ "$calls" = "$want" ]; then
    passed=$((passed + 1)); echo "PASS: $name"
  else echo "FAIL: $name — rc=$rc (want 0), status reads=$calls (want $want)"; fi
}
settle "stops polling as soon as the run turns terminal" 2 running cancelled
settle "never-terminal run: returns 0 once the timeout passes" 5 running

# Custody waits exercise the real helpers with a virtual clock and read-only fixtures.
# The archive-backed and source-only holds stay byte-for-byte intact on success AND
# timeout. Any SQL or write endpoint invoked by the admission helper fails the test.
eval "$(awk '/^wait_custody_headroom\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^wait_forge_park_release\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^held_custody_receipt_valid\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^assert_earlier_held_custody_receipt\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^wait_completed_custody_receipt\(\) \{/,/^\}/' "$LIB")"
pass() { echo "$*"; }
apiget() {
  case "$1" in
    /api/recovery/holds) ;;
    /api/runs/*) printf '%s\n' '{"run":{"id":"R","status":"completed","claim_generation":3}}'; return ;;
    *) fail "unexpected API route $1" ;;
  esac
  local n; n="$(cat "$SEQ_DIR/n")"
  sed -n "${n}p" "$SEQ_DIR/seq"
  [ "$n" -ge "$(wc -l < "$SEQ_DIR/seq")" ] || echo $((n + 1)) > "$SEQ_DIR/n"
}
apipost() { fail "unexpected custody write"; }
apidelete() { fail "unexpected custody discard"; }
db_psql() {
  [[ "$1" == SELECT* && "$1" != *DELETE* && "$1" != *UPDATE* ]] || fail "unexpected custody SQL mutation"
  if [[ "$1" == *json_agg* ]]; then
    apiget /api/recovery/holds | jq -sc '[.[] | if type=="array" then .[] else . end]'
  else
    apiget /api/recovery/holds | jq -c 'if type=="array" then .[] else . end'
  fi
}
custody_case() {
  cases=$((cases + 1))
  local name="$1" want="$2" diagnostic="$3" helper="$4" output got; shift 4
  if output="$(unset SECONDS; SECONDS=0; "$helper" "$@" 2>&1)"; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ] && [[ "$output" == *"$diagnostic"* ]] \
      && ! [[ "$output" == *"unexpected custody"* || "$output" == *"unexpected receipt"* ]]; then
    passed=$((passed + 1)); echo "PASS: $name"
  else echo "FAIL: $name: got $got want $want; output [$output]"; fi
}
headroom_fixture() {
  : > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
  local count
  for count in "$@"; do
    jq -nc --argjson count "$count" '{aggregate:{open_holds:$count,admission_counted_holds:$count,custody_hold_limit:8},holds:[
      {id:"archive",run_id:"R",generation:2,state:"open",attention:"archive_ready",capture_state:"available"},
      {id:"source",run_id:"S",generation:1,state:"open",attention:"source_only"}]}' >> "$SEQ_DIR/seq"
  done
}
headroom_fixture 7 6 5
before="$(cat "$SEQ_DIR/seq")"
custody_case "saturated then free admission preserves custody" pass "open=5 limit=8 needed=3" wait_custody_headroom 3 5
cases=$((cases + 1))
if [ "$(cat "$SEQ_DIR/seq")" = "$before" ]; then passed=$((passed + 1)); echo "PASS: available capture and source-only hold unchanged"; else echo "FAIL: custody fixtures changed"; fi
headroom_fixture 6
before="$(cat "$SEQ_DIR/seq")"
custody_case "saturated admission timeout reports limit and needed" fail "open=6 limit=8 needed=3" wait_custody_headroom 3 2
cases=$((cases + 1))
if [ "$(cat "$SEQ_DIR/seq")" = "$before" ]; then passed=$((passed + 1)); echo "PASS: timeout preserves archive and source-only custody"; else echo "FAIL: timeout changed custody"; fi
headroom_fixture 6
custody_case "timeout lists preserved source-only attention" fail '"attention":"source_only"' wait_custody_headroom 3 2
headroom_fixture 5
custody_case "exactly sufficient admission headroom" pass "open=5 limit=8 needed=3" wait_custody_headroom 3 2
headroom_fixture 0
custody_case "invalid requested headroom fails" fail "invalid needed=0" wait_custody_headroom 0 2
printf '%s\n' '{"aggregate":{"open_holds":0,"admission_counted_holds":0},"holds":[]}' > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
custody_case "missing ceiling cannot read green" fail "invalid admission limit" wait_custody_headroom 1 2

headroom_accounting_fixture() {
  jq -nc --argjson open "$1" --argjson counted "$2" \
    '{aggregate:{open_holds:$open,admission_counted_holds:$counted,custody_hold_limit:8},holds:[]}' > "$SEQ_DIR/seq"
  echo 1 > "$SEQ_DIR/n"
}
headroom_accounting_fixture 12 3
custody_case "healthy live holds do not consume admission headroom" pass 'counted=3 open=12' wait_custody_headroom 2 2
headroom_accounting_fixture 12 0
custody_case "zero counted holds leaves headroom despite retained total" pass 'counted=0 open=12' wait_custody_headroom 3 2
headroom_accounting_fixture 12 7
custody_case "counted saturation reports both pressure and total" fail 'counted=7 open=12' wait_custody_headroom 2 2
for invalid in null -1 1.5 '"2"' true; do
  headroom_accounting_fixture 0 "$invalid"
  custody_case "admission headroom refuses counted=$invalid" fail 'invalid admission count' wait_custody_headroom 1 2
done
printf '%s\n' '{"aggregate":{"open_holds":0,"custody_hold_limit":8},"holds":[]}' > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
custody_case "missing counted field cannot fall back to total" fail 'invalid admission count' wait_custody_headroom 1 2

park_fixture() {
  : > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
  local step status
  for step in "$@"; do
    status=recovery_wait
    if [[ "$step" == failed:* ]]; then status=failed; step="${step#failed:}"; fi
    jq -nc --arg step "$step" --arg status "$status" '{state:"open",inventory_guarded:true,generation:1,claim_generation:1,run_status:$status,final_disposition:null,release_evidence:null,after_outcome:null}
      | if $step=="released" then .+{state:"released",final_disposition:"settled",release_evidence:"forge_no_output",after_outcome:true}
        elif $step=="wrong" then .+{state:"released",final_disposition:"no_adopted_source",release_evidence:"no_adopted_source",after_outcome:true}
        elif $step=="early" then .+{state:"released",final_disposition:"settled",release_evidence:"forge_no_output",after_outcome:false}
        elif $step=="reclaimed" then .claim_generation=2
        elif $step=="promoted" then .run_status="queued"
        elif $step=="legacy" then .+{inventory_guarded:false,state:"released",release_evidence:"no_adopted_source"}
        elif $step=="legacy-open" then .inventory_guarded=false
        elif $step=="duplicate" then [., .+{state:"released",final_disposition:"settled",release_evidence:"forge_no_output",after_outcome:true}]
        else . end' >> "$SEQ_DIR/seq"
  done
}
park_fixture open released
custody_case "guarded park waits for exact final receipt" pass "" wait_forge_park_release R 1 5
park_fixture open
custody_case "guarded park never settles" fail "release timeout" wait_forge_park_release R 1 2
park_fixture wrong
custody_case "wrong release evidence cannot pass" fail "wrong release evidence" wait_forge_park_release R 1 2
park_fixture early
custody_case "release predating park cannot pass" fail "wrong release evidence" wait_forge_park_release R 1 2
park_fixture reclaimed
custody_case "generation change fails immediately" fail "changed run/generation" wait_forge_park_release R 1 2
park_fixture promoted
custody_case "status change fails immediately" fail "changed run/generation" wait_forge_park_release R 1 2
park_fixture legacy
custody_case "legacy atomic release still passes" pass "" wait_forge_park_release R 1 2
park_fixture legacy-open
custody_case "legacy open hold cannot wait to pass" fail "did not atomically release" wait_forge_park_release R 1 2
printf '\n' > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
custody_case "missing exact hold fails closed" fail "missing receipt" wait_forge_park_release R 1 2
park_fixture duplicate
custody_case "two matching holds cannot mask the open first row" fail "expected exactly one hold" wait_forge_park_release R 1 2
park_fixture released
custody_case "null generation cannot reach SQL" fail "invalid generation" wait_forge_park_release R null 2
park_fixture failed:open failed:released
custody_case "guarded cap failure waits for its final receipt" pass "" wait_forge_park_release R 1 5 failed
park_fixture failed:wrong
custody_case "cap failure refuses wrong final evidence" fail "wrong release evidence" wait_forge_park_release R 1 2 failed
park_fixture failed:early
custody_case "cap release predating terminal outcome is refused" fail "wrong release evidence" wait_forge_park_release R 1 2 failed
park_fixture promoted
custody_case "cap failure refuses status change" fail "changed run/generation" wait_forge_park_release R 1 2 failed

# Drive the real sequential outbox creation seam, not just the admission helper.
# Each fresh claim must observe a saturated-then-free owner before create_run.
eval "$(awk '/^make_outbox_run\(\) \{/,/^\}/' "$ROOT/e2e/phases/52-api-outage-outbox.sh")"
headroom_fixture 8 7 8 7
echo 0 > "$SEQ_DIR/claims"
export REPO_ID=repo
apipost() { printf '%s\n' '{"card":{"iid":1}}'; }
wait_status() { :; }
tick_count() { echo 3; }
create_run() {
  local calls expected
  calls="$(cat "$SEQ_DIR/claims")"
  expected=$((calls + 3))
  [ "$(cat "$SEQ_DIR/n")" = "$expected" ] || { echo 'fresh claim before its admission wait' >&2; return 1; }
  echo $((calls + 1)) > "$SEQ_DIR/claims"
  echo R
}
outbox_sequence() {
  make_outbox_run
  make_outbox_run
  [ "$(cat "$SEQ_DIR/claims")" = 2 ] || fail "outbox did not create both claims"
  echo 'two fresh claims after their admission waits'
}
custody_case "each sequential outbox claim waits for one slot" pass 'two fresh claims after their admission waits' outbox_sequence

# Exercise actual phase statements with the real run DTO shape (no generation).
db_psql() {
  case "$1" in
    'SELECT id FROM users'*) echo owner;;
    'SELECT claim_generation FROM runs'*) echo 1;;
    *"user_id = 'owner'"*) echo 1;;
    *) fail "unexpected owner/generation query: $1";;
  esac
}
readoption_owner_queries() {
  unset RA_ADMIN_ID
  local -x X=R ADMIN_EMAIL=admin@example.invalid
  eval "$(awk '/^RA_ADMIN_ID=/' "$ROOT/e2e/phases/42-api-outage-readoption.sh")"
  eval "$(awk '/^CLAIMABLE_[NX]=/' "$ROOT/e2e/phases/42-api-outage-readoption.sh")"
  [ "$CLAIMABLE_N" = 1 ] && [ "$CLAIMABLE_X" = 1 ] || fail "owner-scoped queries failed"
  echo 'owner resolved for the later sibling precondition'
}
custody_case "readoption keeps its later owner-scoped precondition" pass 'owner resolved' readoption_owner_queries
wait_forge_park_release() {
  [ "$2" = 1 ] || fail 'phase did not use authoritative DB generation'
  [ "$1" != B ] || [ "${4:-}" = failed ] || fail 'cap call lacks the expected terminal status'
  park_calls=$((park_calls + 1))
}
park_phase_call() {
  local park_calls=0
  local -x RUN_A=R RUN_B=B FA='{"run":{"status":"recovery_wait"}}'
  eval "$(awk '/^PARK_GENERATION(_B)?=/ || /^wait_forge_park_release /' "$ROOT/e2e/phases/73-forge-unreachable-park.sh")"
  [ "$park_calls" = 2 ] || fail 'phase did not wait for both park and cap receipts'
  echo 'phase used authoritative DB generation'
}
custody_case "park phase gets generation absent from run DTO" pass 'phase used authoritative DB generation' park_phase_call

# Guarded and legacy typed transcripts both obey #2512's same-claim recovery:
# exact available capture before the fresh seed, without another claim or park.
eval "$(awk '/^f42_assert_capture_resume\(\) \{/,/^\}/' "$ROOT/e2e/phases/52-api-outage-outbox.sh")"
eval "$(awk '/^wait_same_claim_capture_resume\(\) \{/,/^\}/' "$LIB")"
fake_compose() { cat "$SEQ_DIR/events"; }
# shellcheck disable=SC2034  # read by the extracted production capture decoder
COMPOSE=(fake_compose)
export RUNROOT="$SEQ_DIR"
uzi_cli() { echo '[{"generation":2,"captures":[{"id":"capture","state":"available"}]}]'; }
capture_fixture() {
  jq -nc --arg variant "$1" '
    {run_id:"R",msg:"run claimed"} as $claim |
    {run_id:"R",msg:"stub executor committed marker",branch:"agent/issue-1"} as $branch |
    {run_id:"R",msg:"recovery: bundle uploaded (durable capture)",server_capture_id:"capture",server_state:"available"} as $capture |
    {run_id:"R",msg:"recovery: guarded inventory disposition",generation:2,capture_id:"local-capture",state:"uploaded"} as $typed |
    {msg:"runner clone: seeding",branch:"agent/issue-1",path:"/data/runner/issue-1.attempt-time-g2-0123456789abcdef"} as $seed |
    {run_id:"R",msg:"recovery: prior open custody holds inventoried after clone",claim_generation:2} as $ready |
    [$claim,$branch,$claim,$capture,$capture,
      (if $variant=="legacy" then ($typed | del(.generation) | .msg="recovery: park/early-terminal disposition outcome" | .claim_generation=2) else $typed end),$seed,$ready] |
    if $variant=="single-upload" then del(.[4])
    elif $variant=="wrong-generation" then .[7].claim_generation=3
    elif $variant=="wrong-order" then [.[0],.[1],.[2],.[6],.[3],.[4],.[5],.[7]]
    elif $variant=="two-captures" then .[4].server_capture_id="other"
    elif $variant=="missing-id" then .[3].server_capture_id=null | .[4].server_capture_id=null
    elif $variant=="extra-claim" then . + [$claim]
    elif $variant=="wrong-seed-generation" then .[6].path="/data/runner/issue-1.attempt-time-g3-0123456789abcdef"
    else . end | .[]' > "$SEQ_DIR/events"
}
capture_fixture guarded
custody_case "guarded typed G+1 same-claim retries retain one distinct capture" pass 'exact generation 2 captured' f42_assert_capture_resume R 2 0
capture_fixture legacy
custody_case "legacy typed G+1 same-claim capture still accepted" pass 'exact generation 2 captured' f42_assert_capture_resume R 2 0
capture_fixture single-upload
custody_case "same-claim capture precedes fresh reseed" pass 'exact generation 2 captured' f42_assert_capture_resume R 2 0
for variant in wrong-generation wrong-order two-captures missing-id extra-claim wrong-seed-generation; do
  capture_fixture "$variant"
  custody_case "capture decoder refuses $variant" fail 'lacks the exact G+1' f42_assert_capture_resume R 2 0
done

cap2_recovery_contract() {
  local mode="$1" RUN_KA=R RUN_KB=S fixture_generation=2
  # shellcheck disable=SC2034  # used by the old real phase statements in the regression
  local RWC_KA RWC_KB
  [ "$mode" != extra-generation ] || fixture_generation=3
  db_psql() { case "$1" in *claim_generation*) echo "$fixture_generation";; *) echo 0;; esac; }
  wait_same_claim_capture_resume() {
    [ "$mode" != missing-capture ] || fail 'cap2 recovery missing exact capture'
    [ "$2:$4" = 2:agent/fixture ] || fail 'cap2 recovery lost generation or branch'
  }
  local definition
  definition="$(awk '/^assert_cap2_recovered_run\(\) \{/,/^\}/' "$ROOT/e2e/phases/45-bounded-concurrency.sh")"
  if [ -n "$definition" ]; then
    eval "$definition"
    assert_cap2_recovered_run R 1 agent/fixture
  else
    # Run the old real counter assertion against a valid same-claim recovery.
    eval "$(awk '/^  RWC_KA=/ { inside=1 } inside {print} /^  pass "restarted worker/ {inside=0}' "$ROOT/e2e/phases/45-bounded-concurrency.sh")"
  fi
  echo 'cap2 capture verified inside the one successor claim'
}
custody_case "cap2 kill recovery accepts same-claim capture without a park" pass 'cap2 capture verified' cap2_recovery_contract valid
custody_case "cap2 kill recovery refuses an extra generation" fail 'recovery generation' cap2_recovery_contract extra-generation
custody_case "cap2 kill recovery requires capture evidence" fail 'missing exact capture' cap2_recovery_contract missing-capture

# The spent finalize allowance preserves recoverable work after #2426. Exercise
# the phase's exact outcome assertions without a stack or timing dependency.
eval "$(awk '/^assert_finalize_allowance_spent\(\) \{/,/^\}/' "$ROOT/e2e/phases/52-api-outage-outbox.sh")"
rb_run_field() { jq -r --arg key "$2" '.[$key] // empty' "$SEQ_DIR/finalize"; }
finalize_fixture() {
  jq -nc '{status:"recovery_wait",recovery_wait_cause:"worker_requeue_exhausted",recovery_retry_not_before:null,
    claim_generation:3,finalize_resume_generation:1,requeue_count:4}' > "$SEQ_DIR/finalize"
}
finalize_fixture
custody_case "spent allowance parks preserved work without another claim" pass "" assert_finalize_allowance_spent R 1 3 3
for change in 'status="failed"' 'recovery_wait_cause="provider_outage"' 'recovery_retry_not_before="later"' 'claim_generation=4' 'finalize_resume_generation=3' 'requeue_count=3'; do
  finalize_fixture
  jq ".$change" "$SEQ_DIR/finalize" > "$SEQ_DIR/changed"
  mv "$SEQ_DIR/changed" "$SEQ_DIR/finalize"
  custody_case "spent allowance refuses $change" fail "case 8:" assert_finalize_allowance_spent R 1 3 3
done

# Drive the actual manual-worker setup. These workers advertise no recovery
# capability, so they add no hold but still need owner count < limit to claim.
binding_fake_stop() {
  if [ "$(cat "$SEQ_DIR/n")" -gt 1 ]; then echo ready > "$SEQ_DIR/stopped"; else echo early > "$SEQ_DIR/stopped"; fi
}
binding_stop_setup() {
  headroom_fixture 8 7
  # shellcheck disable=SC2034  # read by the extracted phase setup
  COMPOSE=(binding_fake_stop)
  eval "$(awk '/^wait_custody_headroom / || /^"\$\{COMPOSE\[@\]\}" stop agent/' "$ROOT/e2e/phases/50-worker-token-binding.sh")"
  [ "$(cat "$SEQ_DIR/stopped")" = ready ] || fail 'agent stopped before final inventories had admission headroom'
  echo 'binding stop waited for headroom'
}
custody_case "binding waits while the real agent can still settle" pass 'binding stop waited' binding_stop_setup
asw_claim() {
  [ "$(cat "$SEQ_DIR/n")" -gt 1 ] || return 1
  local calls; calls="$(cat "$SEQ_DIR/claims")"
  echo $((calls + 1)) > "$SEQ_DIR/claims"
  echo "R$calls"
}
autostop_claim_setup() {
  headroom_fixture 8 7
  echo 0 > "$SEQ_DIR/claims"
  eval "$(awk '/^wait_custody_headroom / || /^C[12]=/' "$ROOT/e2e/phases/51-auto-stop-poison.sh")"
  [ "$(cat "$SEQ_DIR/claims")" = 2 ] || fail 'manual claims did not both occur'
  echo 'both non-recovery claims followed headroom'
}
custody_case "auto-stop manual claims require one slot without adding holds" pass 'both non-recovery claims followed headroom' autostop_claim_setup
open_holds_db() { if [ "$(cat "$SEQ_DIR/n")" -gt 1 ]; then echo 7; else echo 8; fi; }
wedge_headroom_setup() {
  headroom_fixture 8 7
  eval "$(awk '/^wait_custody_headroom / || /^C0=/' "$ROOT/e2e/phases/72-custody-lifecycle.sh")"
  [ "$C0" -lt 8 ] || fail 'custody wedge precondition still saturated'
  echo 'custody wedge can seed its own scoped fixture'
}
custody_case "custody wedge waits before reading its seed precondition" pass 'custody wedge can seed' wedge_headroom_setup

# A completed rework can still have an open hold while its FINAL is retrying.
# Run the actual phase boundary: seeding the next phase before both own receipts
# arrived would make its baseline disappear during the queued negative control.
rework_receipt_boundary() {
  RUN_MRR=b4dec5cf-e6d4-413b-b3a8-0005a2835767
  RUN_MRR2=b4dec5cf-e6d4-413b-b3a8-0005a2835768
  ADMIN_EMAIL=owner@example.test
  printf 'open\n' > "$SEQ_DIR/$RUN_MRR"
  printf 'open\n' > "$SEQ_DIR/$RUN_MRR2"
  db_psql() {
    [[ "$1" == SELECT* && "$1" != *DELETE* && "$1" != *UPDATE* ]] || fail 'unexpected receipt write'
    local run state
    for run in "$RUN_MRR" "$RUN_MRR2"; do
      [[ "$1" == *"r.id='$run'"* ]] || continue
      state="$(cat "$SEQ_DIR/$run")"
      printf 'released\n' > "$SEQ_DIR/$run"
      jq -nc --arg run "$run" --arg state "$state" '{run_id:$run,status:"completed",generation:1,holds:[
        {id:"own-hold",generation:1,inventory_guarded:true,state:$state,after_outcome:true,
          final_disposition:(if $state=="released" then "archive" else null end),
          release_evidence:(if $state=="released" then "archive" else null end),archive_matches:true}]}'
      return
    done
    fail 'receipt query not scoped to either own run'
  }
  eval "$(awk '/^wait_completed_custody_receipt /' "$ROOT/e2e/phases/71-mr-rework.sh")"
  [ "$(cat "$SEQ_DIR/$RUN_MRR")" = released ] && [ "$(cat "$SEQ_DIR/$RUN_MRR2")" = released ] \
    || fail 'boundary would seed custody with a completed rework receipt still open'
  echo 'both completed rework receipts arrived before custody seeding'
}
custody_case "rework boundary settles both own receipts before custody seeding" pass 'both completed rework receipts arrived' rework_receipt_boundary

completed_receipt_fixture() {
  : > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
  local step
  for step in "$@"; do
    jq -nc --arg run "$UUID" --arg step "$step" '{run_id:$run,status:"completed",generation:1,holds:[
      {id:"own-hold",generation:1,inventory_guarded:true,state:"released",after_outcome:true,
        final_disposition:"archive",release_evidence:"archive",archive_matches:true}]}
      | if $step=="completed-publication" then
          . += {owner_id:"owner",worker_id:"worker",repo_id:"repo",mr_iid:7,branch:"agent/issue-1",final_head:("a"*40)} |
          {hold_id:"own-hold",run_id:$run,owner_id:"owner",worker_id:"worker",generation:1,repo_id:"repo",
            connection_id:"connection",project_id:1,forge_type:"gitlab",base_url:"https://example.test",
            branch:"agent/issue-1",mr_iid:7,final_head:("a"*40)} as $identity |
          .holds[0] += {final_disposition:"completed_publication",release_evidence:"completed_publication",
            final_capture_id:null,final_source_sha:null,final_coverage_digest:null,final_publication_id:null,
            completion_identity:$identity,completed_publication_receipt:($identity+{observed_branch_head:("b"*40)})}
        elif $step=="open" then .holds[0] += {state:"open",final_disposition:null,release_evidence:null}
        elif $step=="missing" then .holds=[]
        elif $step=="duplicate" then .holds += .holds
        elif $step=="wrong-generation" then .holds[0].generation=2
        elif $step=="generation-change" then .generation=2 | .holds[0].generation=2
        elif $step=="wrong-archive" then .holds[0].archive_matches=false
        elif $step=="old-receipt" then .holds[0].after_outcome=false
        elif $step=="discarded" then .holds[0] += {state:"discarded",release_evidence:"owner_discard"}
        elif $step=="settled" then .holds[0] += {final_disposition:"settled",release_evidence:"publication",
          final_capture_id:null,final_source_sha:null,final_coverage_digest:"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
        elif $step=="legacy" then .holds[0] += {inventory_guarded:false,final_disposition:null}
        elif $step=="legacy-before-completion" then .holds[0] += {inventory_guarded:false,final_disposition:null,after_outcome:false}
        elif $step=="failed" then .status="failed"
        else . end' >> "$SEQ_DIR/seq"
  done
}
ADMIN_EMAIL=owner@example.test
db_psql() {
  [[ "$1" == SELECT* && "$1" != *DELETE* && "$1" != *UPDATE* \
    && "$1" == *"r.id='$UUID'"* && "$1" == *"r.user_id=(SELECT id FROM users"* ]] \
    || fail 'unexpected receipt query or write'
  [[ "$1" == *"h.run_id=r.id AND h.user_id=r.user_id"* &&
     "$1" == *"FROM run_held_publications p WHERE p.id=h.final_publication_id"* ]] \
    || fail 'unexpected receipt publication/owner scope'
  for binding in "'id',p.id" "'run_id',p.run_id" "'user_id',p.user_id" "'repo_id',p.repo_id" "'hold_id',p.hold_id" "'generation',p.generation" "'worker_id',p.worker_id" "'ref',p.ref" "'tip',p.tip" "'coverage_digest',p.coverage_digest"; do
    [[ "$1" == *"$binding"* ]] || fail 'unexpected receipt missing publication identity'
  done
  if [[ "$1" == *"h.generation<r.claim_generation"* ]]; then
    [[ "$1" == *"h.generation="*" AND h.generation>0"* ]] || fail 'unexpected receipt earlier generation scope'
  else
    [[ "$1" == *"h.generation=r.claim_generation"* ]] || fail 'unexpected receipt current generation scope'
  fi
  local n; n="$(cat "$SEQ_DIR/n")"
  awk -v n="$n" 'NR==n' "$SEQ_DIR/seq"
  [ "$n" -ge "$(wc -l < "$SEQ_DIR/seq")" ] || echo $((n + 1)) > "$SEQ_DIR/n"
}
completed_receipt_fixture open released
custody_case "completed run waits for its exact archive FINAL" pass 'completed custody receipt' wait_completed_custody_receipt "$UUID" 5
completed_receipt_fixture open
custody_case "completed run with an open hold times out" fail 'receipt timeout' wait_completed_custody_receipt "$UUID" 2
for variant in missing duplicate wrong-generation; do
  completed_receipt_fixture "$variant"
  custody_case "completed receipt refuses $variant" fail 'missing exact run/generation/hold' wait_completed_custody_receipt "$UUID" 2
done
for variant in wrong-archive old-receipt; do
  completed_receipt_fixture "$variant"
  custody_case "completed receipt refuses $variant" fail 'lacks final custody proof' wait_completed_custody_receipt "$UUID" 2
done
completed_receipt_fixture open generation-change
custody_case "completed receipt refuses a moving generation" fail 'generation changed' wait_completed_custody_receipt "$UUID" 2
completed_receipt_fixture discarded
custody_case "owner discard cannot replace this final receipt" fail 'unexpected disposition' wait_completed_custody_receipt "$UUID" 2
completed_receipt_fixture settled
custody_case "completed publication accepts an empty settled FINAL" pass 'completed custody receipt' wait_completed_custody_receipt "$UUID" 2
completed_receipt_fixture legacy
custody_case "completed legacy hold accepts its authoritative release" pass 'completed custody receipt' wait_completed_custody_receipt "$UUID" 2
completed_receipt_fixture legacy-before-completion
custody_case "legacy archive receipt may precede completion" pass 'completed custody receipt' wait_completed_custody_receipt "$UUID" 2
completed_receipt_fixture failed
custody_case "failed run cannot satisfy completed custody boundary" fail 'missing exact run/generation/hold' wait_completed_custody_receipt "$UUID" 2
completed_receipt_fixture completed-publication
custody_case "completed publication accepts its exact immutable receipt" pass 'completed custody receipt' wait_completed_custody_receipt "$UUID" 2
for field in hold_id run_id owner_id worker_id generation repo_id connection_id project_id forge_type base_url branch mr_iid final_head; do
  completed_receipt_fixture completed-publication
  jq -c --arg field "$field" '.holds[0].completed_publication_receipt[$field]="other"' "$SEQ_DIR/seq" > "$SEQ_DIR/changed"
  mv "$SEQ_DIR/changed" "$SEQ_DIR/seq"
  custody_case "publication receipt refuses mismatched $field" fail 'lacks final custody proof' wait_completed_custody_receipt "$UUID" 2
done
completed_receipt_fixture completed-publication
jq -c '.holds[0].completed_publication_receipt.observed_branch_head="invalid"' "$SEQ_DIR/seq" > "$SEQ_DIR/changed"
mv "$SEQ_DIR/changed" "$SEQ_DIR/seq"
custody_case "publication receipt refuses an invalid observed head" fail 'lacks final custody proof' wait_completed_custody_receipt "$UUID" 2

# Immutable held FINALs retain their proof after publication expiry/ref deletion.
held_receipt_fixture() {
  completed_receipt_fixture released
  jq -c --arg run "$UUID" '. += {owner_id:"owner",repo_id:"repo",worker_id:"current-worker"} |
    .holds[0] += {run_id:$run,user_id:"owner",repo_id:"repo",original_worker_id:"original-worker",
      final_disposition:"held_publication",release_evidence:"held_publication",final_capture_id:null,
      final_publication_id:"publication",final_source_sha:("a"*40),final_coverage_digest:("b"*64),
      publication:{id:"publication",run_id:$run,user_id:"owner",repo_id:"repo",hold_id:"own-hold",
        generation:1,worker_id:"original-worker",ref:("refs/uzi-held/"+$run+"/1"),
        tip:("a"*40),coverage_digest:("b"*64),state:"expired",ref_deleted:true}}' "$SEQ_DIR/seq" > "$SEQ_DIR/changed"
  mv "$SEQ_DIR/changed" "$SEQ_DIR/seq"
}
held_receipt_fixture
custody_case "current held FINAL accepts original custodian after ref expiry" pass 'completed custody receipt' wait_completed_custody_receipt "$UUID" 2

for mode in current earlier; do
  held_receipt_fixture
  if [ "$mode" = earlier ]; then
    jq -c '.generation=3 | .status="running" | .holds[0].after_outcome=false' "$SEQ_DIR/seq" > "$SEQ_DIR/changed"
    mv "$SEQ_DIR/changed" "$SEQ_DIR/seq"
    receipt_helper=assert_earlier_held_custody_receipt; receipt_arg=1; receipt_success='earlier held custody receipt'; receipt_failure='exact held FINAL proof'
  else
    receipt_helper=wait_completed_custody_receipt; receipt_arg=2; receipt_success='completed custody receipt'; receipt_failure='lacks final custody proof'
  fi
  cp "$SEQ_DIR/seq" "$SEQ_DIR/held-valid"
  custody_case "$mode held FINAL exact identity and original worker" pass "$receipt_success" "$receipt_helper" "$UUID" "$receipt_arg"
  for field in id run_id user_id repo_id hold_id generation worker_id ref tip coverage_digest; do
    jq -c --arg field "$field" '.holds[0].publication[$field]="forged"' "$SEQ_DIR/held-valid" > "$SEQ_DIR/seq"
    custody_case "$mode held publication refuses forged $field" fail "$receipt_failure" "$receipt_helper" "$UUID" "$receipt_arg"
  done
  for change in 'run_id="forged"' 'user_id="forged"' 'repo_id="forged"' 'id="forged"' 'generation=2' 'final_publication_id="forged"' 'original_worker_id="forged"' 'final_source_sha="invalid"' 'final_coverage_digest="invalid"' 'final_capture_id="capture"' 'inventory_guarded=false' 'release_evidence="unknown"' 'final_disposition="unknown"' 'publication=null'; do
    jq -c ".holds[0].$change" "$SEQ_DIR/held-valid" > "$SEQ_DIR/seq"
    if [ "$mode" = current ] && [ "$change" = 'generation=2' ]; then diagnostic='missing exact run/generation/hold'; else diagnostic="$receipt_failure"; fi
    custody_case "$mode held FINAL refuses $change" fail "$diagnostic" "$receipt_helper" "$UUID" "$receipt_arg"
  done
  for state in open discarded unknown; do
    jq -c --arg state "$state" '.holds[0].state=$state' "$SEQ_DIR/held-valid" > "$SEQ_DIR/seq"
    if [ "$mode" = current ]; then diagnostic='unexpected disposition'; else diagnostic="$receipt_failure"; fi
    custody_case "$mode held FINAL refuses $state" fail "$diagnostic" "$receipt_helper" "$UUID" "$receipt_arg"
  done
  # Forge every related field together: shape/provenance must fail on its own,
  # without a coincidental tip/digest/ref mismatch masking the regression.
  for change in \
    '.holds[0].final_source_sha="invalid" | .holds[0].publication.tip="invalid"' \
    '.holds[0].final_coverage_digest="invalid" | .holds[0].publication.coverage_digest="invalid"' \
    '.holds[0].run_id="forged" | .holds[0].publication.run_id="forged" | .holds[0].publication.ref="refs/uzi-held/forged/1"' \
    '.holds[0].user_id="forged" | .holds[0].publication.user_id="forged"' \
    '.holds[0].repo_id="forged" | .holds[0].publication.repo_id="forged"' \
    '.holds[0].original_worker_id=null | .holds[0].publication.worker_id=null' \
    '.holds[0].final_publication_id=null | .holds[0].publication.id=null'; do
    jq -c "$change" "$SEQ_DIR/held-valid" > "$SEQ_DIR/seq"
    custody_case "$mode held FINAL refuses internally matching forgery $change" fail "$receipt_failure" "$receipt_helper" "$UUID" "$receipt_arg"
  done
  jq -c '.holds=[]' "$SEQ_DIR/held-valid" > "$SEQ_DIR/seq"
  if [ "$mode" = current ]; then diagnostic='missing exact run/generation/hold'; else diagnostic="$receipt_failure"; fi
  custody_case "$mode held FINAL refuses missing hold" fail "$diagnostic" "$receipt_helper" "$UUID" "$receipt_arg"
done
for gen in 0 3 4; do
  held_receipt_fixture
  jq -c --argjson gen "$gen" --arg run "$UUID" '.generation=3 | .holds[0].generation=$gen |
    .holds[0].publication.generation=$gen | .holds[0].publication.ref=("refs/uzi-held/"+$run+"/"+($gen|tostring))' \
    "$SEQ_DIR/seq" > "$SEQ_DIR/changed"; mv "$SEQ_DIR/changed" "$SEQ_DIR/seq"
  custody_case "earlier helper refuses non-earlier generation $gen" fail 'earlier receipt' assert_earlier_held_custody_receipt "$UUID" "$gen"
done
held_receipt_fixture
jq -c '.holds[0].after_outcome=false' "$SEQ_DIR/seq" > "$SEQ_DIR/changed"; mv "$SEQ_DIR/changed" "$SEQ_DIR/seq"
custody_case "current held FINAL still requires after-outcome" fail 'lacks final custody proof' wait_completed_custody_receipt "$UUID" 2
held_receipt_fixture
jq -c '.holds[0].final_source_sha=("a"*64) | .holds[0].publication.tip=("a"*64)' "$SEQ_DIR/seq" > "$SEQ_DIR/changed"; mv "$SEQ_DIR/changed" "$SEQ_DIR/seq"
custody_case "held FINAL accepts SHA256 source" pass 'completed custody receipt' wait_completed_custody_receipt "$UUID" 2

handoff_receipt_boundary() {
  # shellcheck disable=SC2034  # consumed by the evaluated real phase statements
  local RUN_T=T RUN_M=M settled=""
  wait_completed_custody_receipt() { settled="$settled $1"; }
  assert_settled() { case " $settled " in *" $1 "*) ;; *) fail "handoff $1 leaves a pending receipt for custody seeding";; esac; }
  assert_no_receipt_wait() { case " $settled " in *" $1 "*) fail "no-MR handoff $1 must not require an unstated custody receipt";; esac; }
  eval "$(awk '
    /^wait_completed_custody_receipt / { print }
    /^RM_JSON=.*handoff rm/ { print "assert_no_receipt_wait T" }
    /^pass "task\/handoff coverage complete/ { print "assert_settled M" }
  ' "$ROOT/e2e/phases/70-handoff.sh")"
  echo 'MR handoff receipt settles before the next phase; no-MR cleanup has no receipt gate'
}
custody_case "only the MR handoff owes the prior receipt boundary" pass 'MR handoff receipt settles' handoff_receipt_boundary

diagnostic_trap() {
  local phase="$1" status="$2"
  RUNROOT="$SEQ_DIR"
  inventory_diagnostic_snapshot() { echo 'selected inventory diagnostic'; }
  eval "$(awk '/^trap .*inventory_diagnostic_snapshot/' "$ROOT/e2e/phases/$phase.sh")"
  exit "$status"
}
# The custody phase's trap also removes its own seeds on failure only, scoped to the owner
# and SEED_IDENT; the cleanup function is extracted from the real phase.
custody_trap_cleanup() {
  local status="$1" wedge="${2:-}" log="$SEQ_DIR/trap-sql"
  local src="${CUSTODY_PHASE_TEST_SOURCE:-$ROOT/e2e/phases/72-custody-lifecycle.sh}"
  : > "$log"
  RUNROOT="$SEQ_DIR"
  # shellcheck disable=SC2034  # consumed by the extracted cleanup function
  ADMIN_ID=owner SEED_IDENT=e2e-72-custody-seed
  # shellcheck disable=SC2034  # consumed by the extracted cleanup function
  WEDGE_RUN="$wedge"
  db_psql() { printf 'SQL %s\n' "$1" >> "$log"; }
  apipost() { printf 'POST %s %s\n' "$1" "$2" >> "$log"; }
  inventory_diagnostic_snapshot() { :; }
  eval "$(awk '/^custody_seed_cleanup\(\) \{/,/^\}/' "$src")"
  eval "$(awk '/^trap .*inventory_diagnostic_snapshot/' "$src")"
  exit "$status"
}
cases=$((cases + 3))
rc=0; (custody_trap_cleanup 7) >/dev/null 2>&1 || rc=$?
sql="$(cat "$SEQ_DIR/trap-sql")"
if [ "$rc" = 7 ] && [[ "$sql" == "SQL DELETE"*"user_id = 'owner'"*"original_worker_identity = 'e2e-72-custody-seed'"* && "$sql" != *POST* ]]; then
  passed=$((passed + 1)); echo "PASS: custody trap deletes only its own seeds on failure and keeps the status"
else echo "FAIL: custody trap seed cleanup on failure: rc=$rc sql=[$sql]"; fi
rc=0; (custody_trap_cleanup 7 wedge-run) >/dev/null 2>&1 || rc=$?
sql="$(cat "$SEQ_DIR/trap-sql")"
if [ "$rc" = 7 ] && [[ "$sql" == "POST /api/runs/wedge-run/inputs "*cancel*$'\n'"SQL DELETE"*"original_worker_identity = 'e2e-72-custody-seed'"* ]]; then
  passed=$((passed + 1)); echo "PASS: custody trap cancels the wedge run before deleting the seeds"
else echo "FAIL: custody trap cancel-before-delete: rc=$rc log=[$sql]"; fi
rc=0; (custody_trap_cleanup 0 wedge-run) >/dev/null 2>&1 || rc=$?
if [ "$rc" = 0 ] && [ ! -s "$SEQ_DIR/trap-sql" ]; then
  passed=$((passed + 1)); echo "PASS: custody trap leaves seeds alone on success"
else echo "FAIL: custody trap touched the DB on success: rc=$rc"; fi

for phase in 60-schedules-sweep 72-custody-lifecycle; do
  custody_case "$phase preserves failure while emitting diagnostics" fail 'selected inventory diagnostic' diagnostic_trap "$phase" 7
  cases=$((cases + 1))
  if output="$(diagnostic_trap "$phase" 0)" && [ -z "$output" ]; then
    passed=$((passed + 1)); echo "PASS: $phase skips the failure snapshot on success"
  else echo "FAIL: $phase invoked diagnostic snapshot on success"; fi
done

# A terminal row alone does not free the worker's snapshot. Drive the real
# shared boundary with a virtual clock, including the actual phase-42 calls.
readopt_settlement() {
  local mode="$1" run X=X ED=ED
  dump_fixture_settlement() { echo 'settlement diagnostic requested'; }
  # shellcheck disable=SC2034  # consumed by the evaluated real phase statements
  local E=E G=G EA=EA GA=GA
  eval "$(awk '/^wait_runs_terminal_off_worker\(\) \{/,/^\}/' "$LIB")"
  for run in R E G EA GA X ED; do
    echo 0 > "$SEQ_DIR/settle-$run"
    echo pending > "$SEQ_DIR/off-$run"
  done
  run_status_quick() {
    local n status=cancelled
    n="$(cat "$SEQ_DIR/settle-$1")"; echo $((n + 1)) > "$SEQ_DIR/settle-$1"
    case "$mode" in
      delayed|phase) [ "$n" -ge 1 ] || status=running ;;
      timeout) status=running ;;
      failed|completed) status="$mode" ;;
      unknown) return 1 ;;
    esac
    echo "$status"
  }
  db_psql() {
    local run n
    run="$(printf '%s' "$1" | awk -F "'" '{print $2}')"
    [ "$1" = "SELECT count(*) FROM worker_active_runs WHERE run_id = '$run'" ] \
      || fail 'unexpected fixture settlement query'
    n="$(cat "$SEQ_DIR/settle-$run")"
    if [ "$mode" = snapshot-timeout ] || { [ "$mode" = delayed-removal ] && [ "$n" -lt 3 ]; }; then
      echo 1
    else echo off > "$SEQ_DIR/off-$run"; echo 0
    fi
  }
  cancel_run() { :; }
  if [ "$mode" = phase ]; then
    # These are real phase statements, not a copy of the fixed sequence.
    eval "$(awk '/^cancel_run "\$(E|EA|X|ED)"/ || /^wait_runs_terminal_off_worker /' \
      "$ROOT/e2e/phases/42-api-outage-readoption.sh")"
    for run in E G EA GA X ED; do
      [ "$(cat "$SEQ_DIR/settle-$run")" -ge 2 ] && [ "$(cat "$SEQ_DIR/off-$run")" = off ] \
        || fail "next case would start with run $run still pending cancellation"
    done
    echo 'every case cancellation observed before the next boundary'
  else
    wait_runs_terminal_off_worker 2 R
    case "$mode" in
      delayed) [ "$(cat "$SEQ_DIR/settle-R")" -eq 2 ] || fail 'did not wait for cancellation' ;;
      delayed-removal) [ "$(cat "$SEQ_DIR/settle-R")" -eq 3 ] || fail 'did not wait for snapshot removal' ;;
    esac
  fi
}
custody_case "readoption waits for delayed cancellation" pass 'run=R status=cancelled' readopt_settlement delayed
custody_case "readoption waits after terminal until off worker" pass 'run=R status=cancelled' readopt_settlement delayed-removal
custody_case "readoption timeout names the non-terminal run" fail 'run=R status=running' readopt_settlement timeout
custody_case "readoption timeout names a terminal run still on worker" fail 'worker_active_runs=1' readopt_settlement snapshot-timeout
custody_case "readoption permits failed terminal fixture off worker" pass 'run=R status=failed' readopt_settlement failed
custody_case "readoption permits completed terminal fixture off worker" pass 'run=R status=completed' readopt_settlement completed
custody_case "readoption unavailable status cannot read green" fail 'status=unknown' readopt_settlement unknown
custody_case "phase 42 waits at every case cancellation boundary" pass 'every case cancellation observed' readopt_settlement phase

rollback_settlement() {
  local ED=ED snapshots=0 terminal_observed=0
  COMPOSE=(rollback_compose)
  eval "$(awk '/^wait_runs_terminal_off_worker\(\) \{/,/^\}/' "$LIB")"
  run_status_quick() { echo cancelled; }
  db_psql() { [ "$snapshots" = 1 ] && echo 0 || echo 1; }
  wait_status() { [ "$1:$2" = ED:cancelled ] || fail 'unexpected terminal wait'; terminal_observed=1; }
  wait_worker_online() { :; }
  rollback_compose() {
    case " $* " in
      *' agent '*)
        [ "$terminal_observed" = 1 ] || fail 'worker recreated before cancellation was observed'
        snapshots=1 ;;
    esac
  }
  # The rollback fallback disables snapshots until the worker registers again.
  # Run the actual restore waits and recreates with that protocol state.
  eval "$(awk '
    /^say "restore:/ { inside=1; next }
    /^pass "api \+ agent recreated/ { inside=0 }
    inside && (/^"\$\{COMPOSE\[@\]\}" up/ || /^wait_status / || /^wait_runs_terminal_off_worker / || /^wait_worker_online$/) { print }
  ' "$ROOT/e2e/phases/42-api-outage-readoption.sh")"
  [ "$terminal_observed:$snapshots" = 1:1 ] || fail 'rollback restoration did not settle in protocol order'
  echo 'rollback cancelled before restart and off worker after renegotiation'
}
custody_case "rollback waits for cancellation before renegotiating snapshots" pass 'rollback cancelled before restart' rollback_settlement

# Accepted cancellation is asynchronous. Execute the real phase's cancellation
# statements and the real wait_status against delayed terminal responses before
# permitting the next leg's credential changes or final token teardown.
autoselect_cancel_boundary() {
  local mode="${1:-delayed}" run
  AS_RUN=A; AS_RUN2=B; AS_RUN3=C
  eval "$(awk '/^wait_status\(\) \{/,/^\}/' "$LIB")"
  for run in "$AS_RUN" "$AS_RUN2" "$AS_RUN3"; do
    echo 0 > "$SEQ_DIR/as-$run-polls"
    echo pending > "$SEQ_DIR/as-$run-state"
  done
  apipost() {
    [ "$2" = '{"kind":"cancel","body":""}' ] || fail 'unexpected autoselect input'
    case "$1" in /api/runs/A/inputs|/api/runs/B/inputs|/api/runs/C/inputs) ;; *) fail 'unexpected autoselect cancel route';; esac
    [ "$mode" != post-failed ] || return 1
    echo '{}'
  }
  apiget() {
    local run="${1##*/}" n status=awaiting_approval
    case "$run" in A|B|C) ;; *) fail 'unexpected autoselect status route';; esac
    n="$(cat "$SEQ_DIR/as-$run-polls")"
    echo $((n + 1)) > "$SEQ_DIR/as-$run-polls"
    if [ "$mode" = failed ]; then status=failed
    elif [ "$n" -ge 1 ]; then status=cancelled; echo cancelled > "$SEQ_DIR/as-$run-state"
    fi
    jq -nc --arg status "$status" '{run:{status:$status,failure_reason:"fixture terminal outcome"}}'
  }
  eval "$(awk '/^apipost "\/api\/runs\/\$AS_RUN[23]?\/inputs"/ || /^wait_status "\$AS_RUN[23]?" cancelled/' "$ROOT/e2e/phases/48-auto-selection.sh")"
  for run in "$AS_RUN" "$AS_RUN2" "$AS_RUN3"; do
    [ "$(cat "$SEQ_DIR/as-$run-state")" = cancelled ] || fail 'token teardown would start with cancellation still pending'
  done
  echo 'all owned cancellations observed before token teardown'
}
custody_case "autoselect observes all three cancellations before credential teardown" pass 'all owned cancellations observed' autoselect_cancel_boundary delayed
custody_case "autoselect cancel POST failure cannot proceed to teardown" fail 'could not cancel auto-selection run' autoselect_cancel_boundary post-failed
custody_case "autoselect unexpected failed outcome cannot proceed to teardown" fail "entered 'failed'" autoselect_cancel_boundary failed

eval "$(awk '/^resolve_fixture_source_hold\(\) \{/,/^\}/' "$LIB")"
fixture_owner_decision() {
  local mode="$1" target=b4dec5cf-e6d4-413b-b3a8-0005a2835770 sibling=b4dec5cf-e6d4-413b-b3a8-0005a2835771
  local other=b4dec5cf-e6d4-413b-b3a8-0005a2835772 status=completed
  echo 0 > "$SEQ_DIR/dispositions"
  jq -nc --arg run "$UUID" --arg id "$target" --arg sibling "$sibling" --arg other "$other" --arg mode "$mode" \
    '{holds:[{id:$id,run_id:$run,generation:1,state:"open",attention:"source_only",has_available_capture:false},
      {id:$sibling,run_id:$run,generation:2,state:"released",attention:"released",has_available_capture:true},
      {id:$other,run_id:$other,generation:1,state:"open",attention:"source_only",has_available_capture:false}]}
      | if $mode=="missing" then .holds|=.[1:]
        elif $mode=="duplicate" then .holds += [ .holds[0] ]
        elif $mode=="wrong-generation" then .holds[0].generation=9
        elif $mode=="released" or $mode=="discarded" then .holds[0].state=$mode
        elif $mode=="capturing" then .holds[0] += {attention:"capturing",capture_state:"uploading"}
        elif $mode=="needs_action" then .holds[0] += {attention:"needs_action",capture_state:"needs_action"}
        elif $mode=="ready" then .holds[0].has_available_capture=true
        elif $mode=="invalid-id" then .holds[0].id="bad"
        else . end' > "$SEQ_DIR/fixture-holds"
  jq -nc --arg sibling "$sibling" --arg target "$target" --arg mode "$mode" \
    '{archives:[{id:"ready-archive",hold_id:$sibling,state:"available",checksum:"stable",byte_size:123}]}
      | if $mode=="target-capture" then .archives += [{id:"partial",hold_id:$target,state:"preparing"}] else . end' > "$SEQ_DIR/fixture-archives"
  [ "$mode" != live ] || status=awaiting_approval
  apiget() {
    case "$1" in
      /api/recovery/holds) cat "$SEQ_DIR/fixture-holds" ;;
      "/api/runs/$UUID/archives") cat "$SEQ_DIR/fixture-archives" ;;
      "/api/runs/$UUID") jq -nc --arg run "$UUID" --arg status "$status" '{run:{id:$run,status:$status}}' ;;
      *) fail 'unexpected fixture read' ;;
    esac
  }
  uzi_cli() {
    [ "$*" = "run discard $UUID --hold $target --yes --json" ] || fail 'unscoped fixture disposition'
    echo $(( $(cat "$SEQ_DIR/dispositions") + 1 )) > "$SEQ_DIR/dispositions"
    [ "$mode" != rpc-failed ] || return 1
    if [ "$mode" != post-state ]; then
      jq --arg id "$target" '(.holds[]|select(.id==$id)).state="discarded"' "$SEQ_DIR/fixture-holds" > "$SEQ_DIR/fixture-next"
      mv "$SEQ_DIR/fixture-next" "$SEQ_DIR/fixture-holds"
    fi
    case "$mode" in
      archive-lost) echo '{"archives":[]}' > "$SEQ_DIR/fixture-archives" ;;
      archive-changed) jq '.archives[0].checksum="changed"' "$SEQ_DIR/fixture-archives" > "$SEQ_DIR/fixture-next"; mv "$SEQ_DIR/fixture-next" "$SEQ_DIR/fixture-archives" ;;
    esac
    [ "$mode" != ack-false ] || { echo '{"discarded":false}'; return; }
    echo '{"discarded":true}'
  }
  resolve_fixture_source_hold "$UUID" 1
  jq -e --arg sibling "$sibling" --arg other "$other" \
    '([.holds[]|select(.id==$sibling)][0].state=="released") and ([.holds[]|select(.id==$other)][0].state=="open")' "$SEQ_DIR/fixture-holds" >/dev/null \
    || fail 'fixture disposition changed a sibling or unrelated hold'
  if [ "$mode" = released ] || [ "$mode" = discarded ]; then
    [ "$(cat "$SEQ_DIR/dispositions")" = 0 ] || fail 'resolved hold was mutated'
  else [ "$(cat "$SEQ_DIR/dispositions")" = 1 ] || fail 'exact owner choice missing'
  fi
  echo 'exact fixture generation resolved, siblings and ready archive preserved'
}
custody_case "exact fixture owner choice preserves sibling and archive" pass 'exact fixture generation resolved' fixture_owner_decision source
for mode in released discarded; do
  custody_case "already $mode fixture hold is a no-op" pass 'exact fixture generation resolved' fixture_owner_decision "$mode"
done
for mode in missing duplicate wrong-generation; do
  custody_case "fixture decision rejects $mode identity" fail 'missing/ambiguous' fixture_owner_decision "$mode"
done
custody_case "fixture decision rejects a live run" fail 'not terminal' fixture_owner_decision live
for mode in capturing needs_action ready; do
  custody_case "fixture decision rejects $mode work" fail 'not a capture-less owner decision' fixture_owner_decision "$mode"
done
custody_case "fixture decision catches capture evidence omitted by hold summary" fail 'has capture evidence' fixture_owner_decision target-capture
custody_case "fixture decision refuses malformed hold id" fail 'invalid fixture hold id' fixture_owner_decision invalid-id
custody_case "fixture decision rejects failed owner RPC" fail 'disposition failed' fixture_owner_decision rpc-failed
custody_case "fixture decision requires positive owner ACK" fail 'not acknowledged' fixture_owner_decision ack-false
custody_case "fixture decision rechecks exact discarded row" fail 'no exact discarded row' fixture_owner_decision post-state
for mode in archive-lost archive-changed; do
  custody_case "fixture decision detects $mode evidence" fail 'changed an available archive' fixture_owner_decision "$mode"
done

fixture_archive_receipt() {
  local mode="$1" fixture_id=b4dec5cf-e6d4-413b-b3a8-0005a2835770
  local ADMIN_EMAIL=fixture@example.com n=0
  # shellcheck disable=SC2034  # read by the extracted real helper on timeout
  local COMPOSE=(fixture_agent_log)
  fixture_agent_log() { echo 'recreated agent log tail'; }
  echo 0 > "$SEQ_DIR/receipt-polls"
  apiget() {
    case "$1" in
      /api/recovery/holds) jq -nc --arg run "$UUID" --arg id "$fixture_id" --arg mode "$mode" \
        '{holds:[{id:$id,run_id:$run,generation:1,state:"open",inventory_guarded:true,attention:"source_only",
          has_available_capture:true,capture_state:(if $mode=="preparing" then "preparing" else "available" end)}]}' ;;
      "/api/runs/$UUID/archives") echo '{"archives":[{"id":"kept","state":"available","checksum":"stable","byte_size":12}]}' ;;
      "/api/runs/$UUID") jq -nc --arg run "$UUID" '{run:{id:$run,status:"cancelled"}}' ;;
      *) fail 'unexpected fixture receipt route' ;;
    esac
  }
  uzi_cli() { fail 'captured hold must never be discarded'; }
  db_psql() {
    [[ "$1" == SELECT* && "$1" != *DELETE* && "$1" != *UPDATE* ]] || fail 'unexpected receipt SQL mutation'
    n="$(cat "$SEQ_DIR/receipt-polls")"
    echo $((n + 1)) > "$SEQ_DIR/receipt-polls"
    jq -nc --arg run "$UUID" --arg id "$fixture_id" --arg mode "$mode" --argjson n "$n" '
      {run_id:$run,run_status:"cancelled",holds:[{id:$id,run_id:$run,generation:1,inventory_guarded:true,
        state:"open",final_disposition:null,release_evidence:null,captures:[{state:"available"}],
        worker:{last_heartbeat_at:"fixture-heartbeat",updated_at:"fixture-register"}}]}
      | if $n>0 and $mode!="timeout" then .holds[0] += {state:"released",final_disposition:"archive",release_evidence:"archive",archive_matches:true} else . end
      | if $n>0 and $mode=="discarded" then .holds[0].state="discarded"
        elif $n>0 and $mode=="no-proof" then .holds[0].archive_matches=false
        elif $n>0 and $mode=="wrong-id" then .holds[0].id="changed"
        else . end'
  }
  if [ "$mode" = timeout ]; then
    local output
    if output="$(resolve_fixture_source_hold "$UUID" 1 2 '2026-10-08T09:08:20Z' 2>&1)"; then
      fail 'captured fixture unexpectedly settled'
    fi
    [[ "$output" == *'restore_at=2026-10-08T09:08:20Z'* && "$output" == *'fixture-heartbeat'* \
      && "$output" == *'fixture-register'* && "$output" == *'"captures"'* \
      && "$output" == *'recreated agent log tail'* && "$output" == *'receipt timeout'* ]] \
      || fail "timeout diagnostic omitted evidence: $output"
    printf '%s\n' "$output"
    return 1
  fi
  resolve_fixture_source_hold "$UUID" 1 2 '2026-10-08T09:08:20Z'
  [ "$(cat "$SEQ_DIR/receipt-polls")" -ge 2 ] || fail 'receipt returned before archive release'
}
custody_case "captured fixture waits for exact archive FINAL without discard" pass 'fixture archive custody receipt:' fixture_archive_receipt released
custody_case "captured fixture refuses mid-wait discard" fail 'unexpected disposition' fixture_archive_receipt discarded
custody_case "captured fixture requires matching archive proof" fail 'lacks archive proof' fixture_archive_receipt no-proof
custody_case "captured fixture preserves exact hold identity" fail 'changed identity' fixture_archive_receipt wrong-id
custody_case "captured fixture timeout retains row worker and recreation log" fail 'recreated agent log tail' fixture_archive_receipt timeout
custody_case "preparing fixture fails immediately" fail 'not a capture-less owner decision' fixture_archive_receipt preparing

# Execute the actual owning phase boundaries against the real owner helper. The
# nine-hold fixture includes the completed outage case, which must remain open.
fixture_phase_boundary() {
  local phase="$1" before after run gen n=0 entry
  local X=b4dec5cf-e6d4-413b-b3a8-0005a2835701 ED=b4dec5cf-e6d4-413b-b3a8-0005a2835702
  local RUN_KA=b4dec5cf-e6d4-413b-b3a8-0005a2835703 RUN_KB=b4dec5cf-e6d4-413b-b3a8-0005a2835704
  local RUN_F1=b4dec5cf-e6d4-413b-b3a8-0005a2835705 RUN_F2=b4dec5cf-e6d4-413b-b3a8-0005a2835706
  # shellcheck disable=SC2034  # read by the extracted owning phase statements
  local GEN_X0=1 GEN_X1=2 GEN_ED_ORIGINAL=1 GEN_KA_ORIGINAL=1 GEN_KB_ORIGINAL=1 GEN_F1=1 GEN_F2=1 GEN_F2_OBSERVED=3
  # shellcheck disable=SC2034  # read by the extracted phase-42 boundary
  local RA_RESTORE_AT=2026-10-08T09:08:20Z
  echo '{"holds":[]}' > "$SEQ_DIR/boundary-holds"
  for entry in "$X:1" "$X:2" "$ED:1" "$RUN_KA:1" "$RUN_KB:1" "$RUN_F1:1" "$RUN_F2:1" "$RUN_F2:3" "$UUID:1"; do
    run="${entry%:*}"; gen="${entry##*:}"; n=$((n + 1))
    jq --arg run "$run" --argjson gen "$gen" --arg id "$(printf 'b4dec5cf-e6d4-413b-b3a8-%012d' "$n")" \
      '.holds += [{id:$id,run_id:$run,generation:$gen,state:"open",attention:"source_only",has_available_capture:false}]' \
      "$SEQ_DIR/boundary-holds" > "$SEQ_DIR/boundary-next"
    mv "$SEQ_DIR/boundary-next" "$SEQ_DIR/boundary-holds"
  done
  apiget() {
    case "$1" in
      /api/recovery/holds) cat "$SEQ_DIR/boundary-holds" ;;
      /api/runs/*/archives) echo '{"archives":[]}' ;;
      /api/runs/*) jq -nc --arg run "${1##*/}" '{run:{id:$run,status:"cancelled"}}' ;;
      *) fail 'unexpected boundary read' ;;
    esac
  }
  uzi_cli() {
    [ "$1 $2 $4 $6 $7" = 'run discard --hold --yes --json' ] || fail 'unexpected fixture command'
    [ "$3" != "$UUID" ] || fail 'completed outage case was discarded'
    jq -e --arg run "$3" --arg id "$5" '[.holds[]|select(.id==$id and .run_id==$run and .state=="open")]|length==1' "$SEQ_DIR/boundary-holds" >/dev/null \
      || fail 'fixture command targeted a foreign hold'
    jq --arg id "$5" '(.holds[]|select(.id==$id)).state="discarded"' "$SEQ_DIR/boundary-holds" > "$SEQ_DIR/boundary-next"
    mv "$SEQ_DIR/boundary-next" "$SEQ_DIR/boundary-holds"
    echo '{"discarded":true}'
  }
  before="$(jq '[.holds[]|select(.state=="open")]|length' "$SEQ_DIR/boundary-holds")"
  eval "$(awk '/^[[:space:]]*resolve_fixture_source_hold /' "$ROOT/e2e/phases/$phase.sh")"
  after="$(jq '[.holds[]|select(.state=="open")]|length' "$SEQ_DIR/boundary-holds")"
  [ "$before" = 9 ] && [ "$after" -lt 8 ] || fail 'next sweep still blocked by deliberate fixture holds'
  jq -e --arg run "$UUID" '[.holds[]|select(.run_id==$run)][0].state=="open"' "$SEQ_DIR/boundary-holds" >/dev/null \
    || fail 'completed outage case lost its evidence'
  echo 'own phase relieved capacity while completed outage evidence stayed open'
}
for phase in 42-api-outage-readoption 45-bounded-concurrency 52-api-outage-outbox; do
  custody_case "$phase resolves only its intentional fixture obligations" pass 'own phase relieved capacity' fixture_phase_boundary "$phase"
done

# The previous cleanup failed with a capture FK and silently removed source-only
# evidence when no capture existed. Pin all three phase seams to read-only admission.
for phase in 42-api-outage-readoption 46-run-health 52-api-outage-outbox; do
  cases=$((cases + 1))
  if grep -qF 'wait_custody_headroom ' "$ROOT/e2e/phases/$phase.sh" && ! grep -qF 'DELETE FROM recovery_custody_holds' "$ROOT/e2e/phases/$phase.sh"; then
    passed=$((passed + 1)); echo "PASS: $phase preserves cross-phase custody"
  else echo "FAIL: $phase still clears custody or lacks headroom wait"; fi
done

# #1368: drive the real phase through the unblock wait. The source override is a
# test-only seam for scratch mutations; production always runs the shipped phase.
CUSTODY_PHASE_TEST_SOURCE="${CUSTODY_PHASE_TEST_SOURCE:-$ROOT/e2e/phases/72-custody-lifecycle.sh}"
eval "$(awk '/^assert_custody_worker_idle\(\) \{/,/^\}/' "$CUSTODY_PHASE_TEST_SOURCE")"
idle_snapshot_case() {
  local fixture="$1"
  echo 0 > "$SEQ_DIR/idle-reads"
  apiget() {
    [ "$1" = /api/workers ] || fail 'wrong worker route'
    echo $(( $(cat "$SEQ_DIR/idle-reads") + 1 )) > "$SEQ_DIR/idle-reads"
    [ "$fixture" != api-error ] || return 22
    printf '%s' "$fixture"
  }
  assert_custody_worker_idle 'snapshot test'
  echo 'worker idle accepted'
}
custody_case "custody worker accepts online and Boolean idle" pass 'worker idle accepted' idle_snapshot_case '{"workers":[{"status":"online","busy":false}]}'
custody_case "custody worker accepts two online idle workers" pass 'worker idle accepted' idle_snapshot_case '{"workers":[{"status":"online","busy":false},{"status":"online","busy":false}]}'
custody_case "custody worker accepts an offline idle second worker" pass 'worker idle accepted' idle_snapshot_case '{"workers":[{"status":"online","busy":false},{"status":"offline","busy":false}]}'
custody_case "custody worker accepts an offline idle first worker" pass 'worker idle accepted' idle_snapshot_case '{"workers":[{"status":"offline","busy":false},{"status":"online","busy":false}]}'
for fixture in \
  '{"workers":[{"status":"online","busy":true}]}' \
  '{"workers":[{"status":"offline","busy":false}]}' \
  '{"workers":[]}' \
  '{"workers":[{"status":"online"}]}' \
  '{"workers":[{"status":"online","busy":"false"}]}' \
  '{"workers":[{"status":"online","busy":0}]}' \
  '{"workers":[{"status":"online","busy":null}]}' \
  '{"workers":[{"status":"online","busy":true},{"status":"online","busy":false}]}' \
  '{"workers":[{"status":"online","busy":false},{"status":"online","busy":true}]}' \
  '{"workers":[{"status":"online","busy":false},{"status":"offline"}]}' \
  '{"workers":[{"status":"offline","busy":false},{"status":"online","busy":true}]}' \
  'invalid JSON' '' 'api-error'; do
  custody_case "custody worker refuses [$fixture]" fail 'snapshot test:' idle_snapshot_case "$fixture"
  cases=$((cases + 1))
  if [ "$(cat "$SEQ_DIR/idle-reads")" = 1 ]; then
    passed=$((passed + 1)); echo 'PASS: rejected snapshot used exactly one GET'
  else echo 'FAIL: rejected snapshot repeated GET'; fi
done

custody_idle_phase() {
  local mode="$1"
  # These variables are consumed by the actual phase statements.
  # shellcheck disable=SC2034
  local ADMIN_EMAIL=fixture@example.com REPO_ID=repo RUNROOT="$SEQ_DIR"
  echo entry > "$SEQ_DIR/idle-stage"
  echo 0 > "$SEQ_DIR/idle-worker-reads"
  echo 0 > "$SEQ_DIR/idle-polls"
  echo 0 > "$SEQ_DIR/idle-seeded"
  : > "$SEQ_DIR/idle-events"
  idle_busy_now() {
    local stage
    stage="$(cat "$SEQ_DIR/idle-stage")"
    [ "$mode" = "$stage" ] || [ "$stage" = claimed ]
  }
  idle_boundary() {
    if idle_busy_now; then
      echo "forbidden boundary: $1" >> "$SEQ_DIR/idle-events"
      fail "busy reached forbidden boundary: $1"
    fi
    echo "$1" >> "$SEQ_DIR/idle-events"
  }
  say() { :; }
  pass() { :; }
  inventory_diagnostic_snapshot() { :; }
  worker_status() { echo online; } # permits an online-only mutation to run
  wait_custody_headroom() { :; }
  db_psql() {
    case "$1" in
      'SELECT id FROM users'*) idle_boundary entry; echo owner ;;
      'SELECT count('*original_worker_identity*) # seed count by identity, not the owner-wide count
        if [ "$(cat "$SEQ_DIR/idle-seeded")" = 1 ]; then echo 8; else echo 0; fi ;;
      'SELECT count('*)
        if [ "$(cat "$SEQ_DIR/idle-seeded")" = 1 ]; then echo 8; else echo 0; fi ;;
      INSERT*) echo 1 > "$SEQ_DIR/idle-seeded" ;;
      UPDATE*) : ;;
      DELETE*) : ;; # failure-path seed cleanup
      *) fail 'unhandled idle phase SQL' ;;
    esac
  }
  apiget() {
    case "$1" in
      /api/workers)
        echo $(( $(cat "$SEQ_DIR/idle-worker-reads") + 1 )) > "$SEQ_DIR/idle-worker-reads"
        if idle_busy_now; then echo '{"workers":[{"status":"online","busy":true}]}'
        else echo '{"workers":[{"status":"online","busy":false}]}'; fi ;;
      /api/recovery/holds)
        echo '{"aggregate":{"open_holds":8,"admission_counted_holds":8,"custody_hold_limit":8,"blocked_runs":1}}' ;;
      /api/runs/wedge) echo '{"run":{"status":"queued"}}' ;;
      *) fail 'unhandled idle phase API read' ;;
    esac
  }
  apipost() { echo '{"card":{"iid":1}}'; }
  create_run() { echo window > "$SEQ_DIR/idle-stage"; echo wedge; }
  assert_earlier_held_custody_receipt() {
    [ "$1:$2" = "$UUID:1" ] || fail 'wrong earlier receipt boundary identity'
    [ "$(cat "$SEQ_DIR/idle-stage")" = entry ] || fail 'earlier receipt checked after seeding'
    echo verified > "$SEQ_DIR/older-verified"
  }
  db_psql_rows() {
    if [[ "$1" == 'SELECT h.run_id,h.generation'* ]]; then
      [[ "$1" == *"h.user_id='owner'"* && "$1" == *"h.state='released'"* &&
         "$1" == *"h.final_disposition='held_publication'"* &&
         "$1" == *"h.generation>0 AND h.generation<r.claim_generation"* ]] || fail 'wrong earlier receipt observation scope'
      printf '%s|1\n' "$UUID"
      return
    fi
    # Hold-id reads are not window polls; only the json_build_object read is.
    if [[ "$1" == 'SELECT id FROM recovery_custody_holds'* ]]; then
      echo "$UUID"
      return
    fi
    [[ "$1" == 'SELECT json_build_object'* ]] || fail 'unhandled idle phase row SQL'
    [ "$(cat "$SEQ_DIR/older-verified")" = verified ] || fail 'older held receipt missed before fixture'
    idle_boundary window
    echo $(( $(cat "$SEQ_DIR/idle-polls") + 1 )) > "$SEQ_DIR/idle-polls"
  }
  uzi_cli() {
    case "$1 $2" in
      'run recovery')
        echo discard > "$SEQ_DIR/idle-stage"
        printf '[{"id":"%s","state":"open"}]\n' "$UUID" ;;
      'run discard')
        idle_boundary discard
        [ "$*" = "run discard wedge --hold $UUID --yes --json" ] || fail 'wrong discard target'
        echo claimed > "$SEQ_DIR/idle-stage"
        echo '{"discarded":true}' ;;
      *) fail 'unhandled idle phase CLI' ;;
    esac
  }
  wait_status() {
    [ "$*" = 'wedge awaiting_approval 60' ] || fail 'changed unblock wait'
    [ "$(cat "$SEQ_DIR/idle-stage")" = claimed ] || fail 'unblock wait before discard'
    echo unblock >> "$SEQ_DIR/idle-events"
  }
  # Include actual setup, guards, polling, CLI and wait statements, stopping before
  # post-claim custody accounting (covered by the live full-stack phase).
  # shellcheck disable=SC1090  # extracted real phase under test, optionally replaced for mutation checks
  source <(awk '{print} /^wait_status "\$WEDGE_RUN" awaiting_approval 60$/ {exit}' "$CUSTODY_PHASE_TEST_SOURCE")
}
for mode in entry window discard idle; do
  cases=$((cases + 1))
  if output="$(unset SECONDS; SECONDS=0; custody_idle_phase "$mode" 2>&1)"; then got=pass; else got=fail; fi
  events="$(cat "$SEQ_DIR/idle-events")"
  reads="$(cat "$SEQ_DIR/idle-worker-reads")"
  polls="$(cat "$SEQ_DIR/idle-polls")"
  valid=false
  case "$mode" in
    entry) [ "$got" = fail ] && [ "$reads" = 1 ] && [ -z "$events" ] \
      && [[ "$output" == *'custody phase entry:'* && "$output" == *'"busy":true'* ]] && valid=true ;;
    window) [ "$got" = fail ] && [ "$reads" = 2 ] && [ "$events" = entry ] \
      && [[ "$output" == *'custody negative window:'* && "$output" == *'"busy":true'* ]] && valid=true ;;
    discard) [ "$got" = fail ] && [ "$reads" = 3 ] && [ "$polls" = 12 ] \
      && [[ "$events" != *discard* && "$events" != *unblock* && "$events" != *forbidden* ]] \
      && [[ "$output" == *'custody pre-discard:'* && "$output" == *'"busy":true'* ]] && valid=true ;;
    idle) [ "$got" = pass ] && [ "$reads" = 3 ] && [ "$polls" = 12 ] \
      && [[ "$events" == *$'discard\nunblock' && "$events" != *forbidden* ]] && valid=true ;;
  esac
  if [ "$valid" = true ]; then
    passed=$((passed + 1)); echo "PASS: real custody phase idle boundary $mode"
  else echo "FAIL: real custody phase idle boundary $mode: result=$got reads=$reads polls=$polls events=[$events] output=[$output]"; fi
done

# Stateful gate model driving the REAL phase end to end (no cutoff). The model is a set of
# files (mocks run inside command substitutions): baseline holds B (all admission-counted),
# the seeds the phase inserted, how many were attached/discarded. The fake claim loop admits
# the wedge run iff counted = B + undiscarded seeds < 8, and wait_status FAILS unless the
# model says the run was claimed, so a phase that discards too few holds cannot pass.
# Args: PHASE_FILE BASELINE SETTLE(never|poll1|discard1) CLAIM(gate|always)
#       [AGG(normal|seed-short|post-nodiff|post-high)]
custody_model_phase() {
  local phase="$1" baseline="$2" settle="$3" claim="$4" agg="${5:-normal}"
  local M="$SEQ_DIR/model"
  rm -rf "$M"; mkdir -p "$M"
  echo "$baseline" > "$M/b"; echo 0 > "$M/seeds"; echo 0 > "$M/attached"
  echo 0 > "$M/discarded"; echo 0 > "$M/claimed"; echo 0 > "$M/polls"; echo 0 > "$M/cleanup"
  : > "$M/discards"
  # shellcheck disable=SC2034  # consumed by the actual phase statements
  local ADMIN_EMAIL=fixture@example.com REPO_ID=repo RUNROOT="$SEQ_DIR"
  local U="b4dec5cf-e6d4-413b-b3a8-0005a283"
  : > "$SEQ_DIR/custody-window.jsonl"
  model_counted() { echo $(( $(cat "$M/b") + $(cat "$M/seeds") - $(cat "$M/discarded") )); }
  model_claimed() {
    if [ "$(cat "$M/claimed")" = 1 ]; then return 0; fi
    if [ "$claim" = always ] || [ "$(model_counted)" -lt 8 ]; then echo 1 > "$M/claimed"; return 0; fi
    return 1
  }
  say() { :; }
  pass() { :; }
  record_margin() { :; }
  sleep() { SECONDS=$((SECONDS + 1)); } # virtual clock for the negative window
  inventory_diagnostic_snapshot() { :; }
  wait_custody_headroom() { :; }
  assert_earlier_held_custody_receipt() { :; }
  db_psql() {
    case "$1" in
      'SELECT id FROM users'*) echo owner ;;
      'SELECT count('*original_worker_identity*) echo "$(cat "$M/seeds")" ;;
      'SELECT count('*) model_counted ;;
      INSERT*) [[ "$1" =~ generate_series\(1,\ ([0-9]+)\) ]] && echo "${BASH_REMATCH[1]}" > "$M/seeds" ;;
      UPDATE*) [[ "$1" =~ LIMIT\ ([0-9]+) ]] && echo "${BASH_REMATCH[1]}" > "$M/attached" ;;
      DELETE*) echo 1 > "$M/cleanup" ;;
      *) fail "unhandled model SQL: $1" ;;
    esac
  }
  db_psql_rows() {
    local i
    case "$1" in
      'SELECT h.run_id,h.generation'*) : ;;
      'SELECT json_build_object'*)
        echo $(( $(cat "$M/polls") + 1 )) > "$M/polls"
        [ "$settle" != poll1 ] || echo 0 > "$M/b" ;;
      'SELECT id FROM recovery_custody_holds'*DISTINCT*)
        model_claimed && echo "${U}ffff" ;;
      'SELECT id FROM recovery_custody_holds'*)
        for ((i = 1; i <= $(cat "$M/attached"); i++)); do echo "${U}000$i"; done ;;
      *) fail "unhandled model row SQL: $1" ;;
    esac
  }
  apiget() {
    local counted open
    case "$1" in
      /api/workers) echo '{"workers":[{"status":"online","busy":false}]}' ;;
      /api/recovery/holds)
        counted="$(model_counted)"; open="$counted"
        # A claimed run's fresh hold is open but excluded from admission counting.
        if [ "$claim" != always ] && model_claimed; then open=$((counted + 1)); fi
        # Aggregate-serving faults: seed-short serves counted=LIMIT-1 on the first read after
        # seeding; post-nodiff serves open == counted after the claim; post-high serves
        # counted >= LIMIT after the claim.
        if [ "$agg" = seed-short ] && [ "$(cat "$M/seeds")" != 0 ] && [ ! -e "$M/short-served" ]; then
          : > "$M/short-served"; counted=7; open=7
        elif [ "$agg" = post-nodiff ] && [ "$(cat "$M/claimed")" = 1 ]; then open="$counted"
        elif [ "$agg" = post-high ] && [ "$(cat "$M/claimed")" = 1 ]; then counted=8; open=9
        fi
        jq -nc --argjson c "$counted" --argjson o "$open" --argjson b "$([ "$(cat "$M/claimed")" = 1 ] && echo 0 || echo 1)" \
          '{aggregate:{open_holds:$o,admission_counted_holds:$c,custody_hold_limit:8,blocked_runs:$b}}' ;;
      /api/runs/wedge) if model_claimed; then echo '{"run":{"status":"running"}}'; else echo '{"run":{"status":"queued"}}'; fi ;;
      *) fail "unhandled model API read: $1" ;;
    esac
  }
  apipost() { echo '{"card":{"iid":1}}'; }
  create_run() { echo wedge; }
  uzi_cli() {
    local i id out
    case "$1 $2" in
      'run recovery')
        out='[]'
        for ((i = 1; i <= $(cat "$M/attached"); i++)); do
          id="${U}000$i"
          if grep -qx "$id" "$M/discards"; then st=discarded; else st=open; fi
          out="$(jq -c --arg id "$id" --arg st "$st" '. + [{id:$id,state:$st}]' <<< "$out")"
        done
        if model_claimed && [ "$(cat "$M/discarded")" -ge 1 ]; then
          out="$(jq -c --arg id "${U}ffff" '. + [{id:$id,state:"open"}]' <<< "$out")"
        fi
        echo "$out" ;;
      'run discard')
        [ "$3" = wedge ] && [ "$4" = --hold ] || fail "wrong discard target: $*"
        grep -qx "$5" "$M/discards" && fail "hold discarded twice: $5"
        echo "$5" >> "$M/discards"
        echo $(( $(cat "$M/discarded") + 1 )) > "$M/discarded"
        [ "$settle" != discard1 ] || echo 0 > "$M/b"
        echo '{"discarded":true}' ;;
      *) fail "unhandled model CLI: $*" ;;
    esac
  }
  wait_status() {
    case "$2" in
      awaiting_approval) model_claimed || fail "model: run $1 never claimed (counted=$(model_counted)); unblock wait would time out" ;;
      cancelled) : ;;
      *) fail "unexpected wait_status $*" ;;
    esac
  }
  # shellcheck disable=SC1090  # the real phase under test (or a scratch mutant)
  source "$phase"
  echo "discards=$(cat "$M/discarded") cleanup=$(cat "$M/cleanup")"
}
# mutant: K forced to 1 in a scratch copy of the phase (the pre-fix single-discard unblock).
sed 's/^K=\$((UNB_COUNTED - LIMIT + 1))$/K=1/' "$CUSTODY_PHASE_TEST_SOURCE" > "$SEQ_DIR/phase72-k1.sh"
# mutant: seed only LIMIT - C0 holds (the baseline-dependent seeding), seed-count assertion adjusted.
sed -e 's/^NEED=\$LIMIT$/NEED=$((LIMIT - C0))/' \
    -e 's/^\[ "\$SEEDED" = "\$LIMIT" \] || fail/[ "$SEEDED" = "$NEED" ] || fail/' \
    "$CUSTODY_PHASE_TEST_SOURCE" > "$SEQ_DIR/phase72-base.sh"
model_case() {
  local name="$1" want="$2" needle="$3"; shift 3
  cases=$((cases + 1))
  if output="$(unset SECONDS; SECONDS=0; custody_model_phase "$@" 2>&1)"; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ] && [[ "$output" == *"$needle"* ]]; then
    passed=$((passed + 1)); echo "PASS: custody gate model: $name"
  else echo "FAIL: custody gate model: $name: result=$got want=$want output=[$output]"; fi
}
model_case "baseline of 1 settles on the first window poll" pass 'discards=1' "$CUSTODY_PHASE_TEST_SOURCE" 1 poll1 gate
model_case "persistent baseline of 1 needs two discards" pass 'discards=2' "$CUSTODY_PHASE_TEST_SOURCE" 1 never gate
model_case "baseline settling between the two discards" pass 'discards=2' "$CUSTODY_PHASE_TEST_SOURCE" 1 discard1 gate
model_case "empty baseline needs one discard" pass 'discards=1' "$CUSTODY_PHASE_TEST_SOURCE" 0 never gate
model_case "mutant with a single discard cannot unblock a baseline of 1" fail 'never claimed' "$SEQ_DIR/phase72-k1.sh" 1 never gate
model_case "product control: claim while counted >= limit fails the negative window" fail "wedge run left 'queued'" "$CUSTODY_PHASE_TEST_SOURCE" 1 never always
model_case "mutant seeding LIMIT - baseline wedges nothing when the baseline settles" fail "wedge run left 'queued'" "$SEQ_DIR/phase72-base.sh" 1 poll1 gate
model_case "post-claim aggregate with open == counted fails" fail 'open_holds - admission_counted_holds = 0' "$CUSTODY_PHASE_TEST_SOURCE" 0 never gate post-nodiff
model_case "post-claim aggregate with counted >= limit fails" fail 'still >= 8 after the unblock' "$CUSTODY_PHASE_TEST_SOURCE" 0 never gate post-high
model_case "post-seed aggregate below the limit fails" fail 'after seeding 8 holds' "$CUSTODY_PHASE_TEST_SOURCE" 0 never gate seed-short

# Exercise the actual handoff startup/trap, stopping before transport and dispatch.
# The compose double records its exported environment, not source-code spelling.
handoff_timing_scope() {
  HANDOFF_TEST_MODE="$1"
  # shellcheck disable=SC2034  # consumed by the actual sourced phase prefix
  EXECUTOR="$2"
  case "$3" in
    unset) unset UZI_E2E_RECOVERY_FAST_REDRIVE ;;
    empty) export UZI_E2E_RECOVERY_FAST_REDRIVE= ;;
    *) export UZI_E2E_RECOVERY_FAST_REDRIVE="$3" ;;
  esac
  # shellcheck disable=SC2034  # consumed by the actual sourced phase prefix
  COMPOSE=(handoff_compose -p handoff-timing-fixture)
  HANDOFF_TEST_COMPOSE_CALLS=0
  HANDOFF_TEST_REGISTRATIONS=0
  say() { :; }
  fail() { echo "$*" >&2; exit 1; }
  handoff_compose() {
    HANDOFF_TEST_COMPOSE_CALLS=$((HANDOFF_TEST_COMPOSE_CALLS + 1))
    printf 'compose:%s:%s:%s\n' "${UZI_E2E_RECOVERY_FAST_REDRIVE+x}" "$(printenv UZI_E2E_RECOVERY_FAST_REDRIVE || :)" "$*" >> "$SEQ_DIR/handoff-timing"
    [ "$HANDOFF_TEST_MODE" != activation ] || [ "$HANDOFF_TEST_COMPOSE_CALLS" -ne 1 ] || return 1
    case "$HANDOFF_TEST_MODE:$HANDOFF_TEST_COMPOSE_CALLS" in restore:2|receipt-restore:2) return 1;; esac
  }
  wait_worker_online() {
    HANDOFF_TEST_REGISTRATIONS=$((HANDOFF_TEST_REGISTRATIONS + 1))
    [ "$HANDOFF_TEST_MODE" != registration ] || [ "$HANDOFF_TEST_REGISTRATIONS" -ne 1 ] || return 1
  }
  # shellcheck disable=SC1090  # real phase prefix, no transport or dispatch
  source <(awk '/^WC=/ {exit} {print}' "$ROOT/e2e/phases/70-handoff.sh")
  printf 'body:%s:%s\n' "${UZI_E2E_RECOVERY_FAST_REDRIVE+x}" "$(printenv UZI_E2E_RECOVERY_FAST_REDRIVE || :)" >> "$SEQ_DIR/handoff-timing"
  case "$HANDOFF_TEST_MODE" in receipt|receipt-restore) exit 7;; esac
  exit 0
}
for spec in 'success:stub:unset' 'success:stub:0' 'success:stub:empty' 'success:sdk:unset' 'success:sdk:0' \
    'activation:stub:unset' 'registration:stub:0' 'receipt:stub:unset' 'restore:stub:0' 'receipt-restore:stub:unset'; do
  cases=$((cases + 1))
  IFS=: read -r mode executor prior <<< "$spec"
  : > "$SEQ_DIR/handoff-timing"
  if output="$(handoff_timing_scope "$mode" "$executor" "$prior" 2>&1)"; then got=pass; else got=fail; fi
  events="$(cat "$SEQ_DIR/handoff-timing")"
  if [ "$prior" = unset ]; then restored=':'; elif [ "$prior" = empty ]; then restored='x:'; else restored="x:$prior"; fi
  valid=false
  if [ "$executor" = sdk ]; then
    [ "$got" = pass ] && [ "$events" = "body:${restored}" ] && valid=true
  else
    first="$(head -1 "$SEQ_DIR/handoff-timing")"
    last="$(tail -1 "$SEQ_DIR/handoff-timing")"
    [ "$mode" = success ] && want=pass || want=fail
    if [ "$got" = "$want" ] && [[ "$first" == 'compose:x:1:-p handoff-timing-fixture '* ]] \
        && [[ "$last" == "compose:${restored}:-p handoff-timing-fixture "* ]]; then
      valid=true
      if [ "$mode" = activation ] || [ "$mode" = registration ]; then
        [[ "$events" != *body:* ]] || valid=false
      else
        [[ "$events" == *body:x:1* ]] || valid=false
      fi
      case "$mode" in restore|receipt-restore) [[ "$output" == *'handoff timing restore failed'* ]] || valid=false;; esac
    fi
  fi
  if [ "$valid" = true ]; then
    passed=$((passed + 1)); echo "PASS: handoff timing scope $spec"
  else echo "FAIL: handoff timing scope $spec: result=$got events=[$events] output=[$output]"; fi
done

# Drive the shipped phase-18 continuation/EXIT scope with mocked API/CLI
# transports. These cases assert restoration and intent preservation on failures,
# not just the metadata predicate in isolation.
manual_hold_case() {
  bash -euo pipefail -s -- "$ROOT" "$SEQ_DIR" "$1" <<'BASH'
ROOT="$1" SEQ_DIR="$2" MODE="$3"
RUN_CLI=run-owned COMPLETE_TIMEOUT_DEFAULT=5
CLI_GATE_JSON='{"worker_id":"worker-owned"}'
printf '%s' '{"id":"worker-owned","ephemeral":false,"manual_cordoned_at":null,"manual_cordoned_by":null,"manual_cordoned_by_label":null,"manual_cordon_reason":null}' > "$SEQ_DIR/manual-worker"
set_hold() {
  jq --arg reason "$1" '.manual_cordoned_at="2026-10-24T00:00:00Z" | .manual_cordoned_by="owner-owned"
    | .manual_cordoned_by_label="Owner" | .manual_cordon_reason=$reason | .effective_cordoned=true' \
    "$SEQ_DIR/manual-worker" > "$SEQ_DIR/manual-next"
  mv "$SEQ_DIR/manual-next" "$SEQ_DIR/manual-worker"
}
[ "$MODE" != prior ] || set_hold prior-intent
db_psql() {
  case "$1" in
    *gen_random_uuid*) echo fixture-owned ;;
    *'SELECT user_id'*) echo owner-owned ;;
    *'SELECT display_name'*) echo Owner ;;
    *'SELECT count('* ) echo 1 ;;
    *) return 1 ;;
  esac
}
apiget() {
  case "$1" in
    /api/workers) jq '{workers:[.]}' "$SEQ_DIR/manual-worker" ;;
    /api/runs/run-owned) echo '{"run":{"id":"run-owned","status":"completed","worker_id":"worker-owned"}}' ;;
    *) return 1 ;;
  esac
}
uzi_cli() {
  printf '%s\n' "$1 $2" >> "$SEQ_DIR/manual-calls"
  case "$1 $2" in
    'worker cordon')
      set_hold "$CLI_HOLD_REASON"
      [ "$MODE" != ambiguous ] || return 7
      cat "$SEQ_DIR/manual-worker" ;;
    'run approve')
      [ "$MODE" != replacement ] || set_hold replacement-intent
      [ "$MODE" != approval ] || return 7 ;;
    'worker uncordon')
      [ "$MODE" != restore ] || return 7
      jq '.manual_cordoned_at=null | .manual_cordoned_by=null | .manual_cordoned_by_label=null
        | .manual_cordon_reason=null' "$SEQ_DIR/manual-worker" > "$SEQ_DIR/manual-next"
      mv "$SEQ_DIR/manual-next" "$SEQ_DIR/manual-worker"
      cat "$SEQ_DIR/manual-worker" ;;
    *) return 1 ;;
  esac
}
wait_status() { [ "$MODE" != completion ]; }
fail() { echo "$*" >&2; exit 1; }
pass() { :; }
# shellcheck disable=SC1090  # shipped phase suffix, mocked transports
source <(awk '/^# This run is already held/ {emit=1} emit' "$ROOT/e2e/phases/18-cli-smoke.sh")
BASH
}
for mode in success prior approval completion ambiguous replacement restore; do
  cases=$((cases + 1))
  : > "$SEQ_DIR/manual-calls"
  if output="$(manual_hold_case "$mode" 2>&1)"; then got=pass; else got=fail; fi
  calls="$(cat "$SEQ_DIR/manual-calls")"
  reason="$(jq -r '.manual_cordon_reason' "$SEQ_DIR/manual-worker")"
  valid=false
  case "$mode" in
    success) [ "$got" = pass ] && [ "$reason" = null ] && [[ "$calls" == *'worker uncordon'* ]] && valid=true ;;
    prior) [ "$got" = fail ] && [ "$reason" = prior-intent ] && [ -z "$calls" ] && valid=true ;;
    replacement) [ "$got" = fail ] && [ "$reason" = replacement-intent ] && [[ "$calls" != *'worker uncordon'* ]] && valid=true ;;
    restore) [ "$got" = fail ] && [[ "$reason" == e2e-cli-assigned-* ]] && [[ "$output" == *cleanup* ]] && valid=true ;;
    *) [ "$got" = fail ] && [ "$reason" = null ] && [[ "$calls" == *'worker uncordon'* ]] && valid=true ;;
  esac
  if [ "$valid" = true ]; then
    passed=$((passed + 1)); echo "PASS: assigned CLI manual hold $mode"
  else echo "FAIL: assigned CLI manual hold $mode result=$got reason=$reason calls=[$calls] output=[$output]"; fi
done

# The isolated-owner cleanup must terminalize its exact runs before cascading
# resource deletion. Exercise the real local EXIT trap, including failure stops.
manual_owner_cleanup_case() {
  bash -euo pipefail -s -- "$ROOT" "$SEQ_DIR" "$1" <<'BASH'
ROOT="$1" SEQ_DIR="$2" MODE="$3"
M5_OWNER=11111111-1111-4111-8111-111111111111
M5_REPO=22222222-2222-4222-8222-222222222222
M5_EMAIL=fixture-owned@uzi.e2e
M5_RUNS=(33333333-3333-4333-8333-333333333333)
case "$MODE" in
  uncreated) M5_OWNER="" ;;
  invalid-owner) M5_OWNER=not-a-uuid ;;
  sql-owner) M5_OWNER="invalid'"; M5_OWNER+=" OR '1'='1" ;;
  invalid-fixture|invalid-email|invalid-label)
    M5_OWNER="" M5_REG_ARMED=1
    M5_FIXTURE=44444444-4444-4444-8444-444444444444
    M5_EMAIL="manual-$M5_FIXTURE@uzi.e2e"
    M5_LABEL="Manual owner $M5_FIXTURE"
    case "$MODE" in
      invalid-fixture) M5_FIXTURE="invalid'" ;;
      invalid-email) M5_EMAIL="invalid'" ;;
      invalid-label) M5_LABEL="invalid'" ;;
    esac ;;
esac
db_psql() {
  printf '%s\n' "$1" >> "$SEQ_DIR/manual-cleanup"
  case "$1" in
    'UPDATE runs'*)
      [[ "$1" == *"id='33333333-3333-4333-8333-333333333333' AND user_id='11111111-1111-4111-8111-111111111111' AND repo_id='22222222-2222-4222-8222-222222222222'"* ]] || return 1
      [ "$MODE" != terminal-failure ] ;;
    'SELECT count(*) FROM runs'*)
      [ "$MODE" != still-active ] && echo 0 || echo 1 ;;
    'DELETE FROM users'*)
      [[ "$1" == *"id='11111111-1111-4111-8111-111111111111' AND email='fixture-owned@uzi.e2e'"* ]] || return 1
      echo 11111111-1111-4111-8111-111111111111 ;;
    'SELECT (SELECT count(*) FROM workers'*) echo 0 ;;
    *) return 1 ;;
  esac
}
# shellcheck disable=SC1090  # actual phase-local cleanup and EXIT handler
source <(awk '/^  m5_uuid\(\)/ {emit=1} emit {print} /^  trap m5_exit EXIT/ {exit}' "$ROOT/e2e/phases/50-worker-token-binding.sh")
case "$MODE" in
  success) m5_cleanup; trap - EXIT ;;
  # A successful body must still fail when the EXIT handler cannot safely clean.
  invalid-owner|sql-owner|invalid-fixture|invalid-email|invalid-label) exit 0 ;;
  *) exit 7 ;;
esac
BASH
}
for mode in success failure uncreated terminal-failure still-active invalid-owner sql-owner invalid-fixture invalid-email invalid-label; do
  cases=$((cases + 1))
  : > "$SEQ_DIR/manual-cleanup"
  if output="$(manual_owner_cleanup_case "$mode" 2>&1)"; then got=pass; else got=fail; fi
  calls="$(cat "$SEQ_DIR/manual-cleanup")"
  valid=false
  case "$mode" in
    uncreated) [ "$got" = fail ] && [ -z "$calls" ] && valid=true ;;
    invalid-owner|sql-owner|invalid-fixture|invalid-email|invalid-label)
      [ "$got" = fail ] && [ -z "$calls" ] && [[ "$output" == *'owned fixture cleanup failed'* ]] && valid=true ;;
    terminal-failure|still-active)
      [ "$got" = fail ] && [[ "$calls" != *'DELETE FROM users'* ]] && [[ "$output" == *cleanup* ]] && valid=true ;;
    *)
      [ "$mode" = success ] && want=pass || want=fail
      [ "$got" = "$want" ] && [[ "$calls" == 'UPDATE runs'*'DELETE FROM users'* ]] && valid=true ;;
  esac
  if [ "$valid" = true ]; then
    passed=$((passed + 1)); echo "PASS: manual owner cleanup $mode"
  else echo "FAIL: manual owner cleanup $mode result=$got calls=[$calls] output=[$output]"; fi
done

# Drive registration and EXIT cleanup together, so a lost curl response cannot
# hide an identity committed before owner assignment. All transports are offline.
manual_registration_case() {
  bash -euo pipefail -s -- "$ROOT" "$SEQ_DIR" "$1" <<'BASH'
ROOT="$1" SEQ_DIR="$2" MODE="$3"
BASE=http://fixture.invalid RUNROOT="$SEQ_DIR"
fixture=44444444-4444-4444-8444-444444444444
owner=11111111-1111-4111-8111-111111111111
email="manual-$fixture@uzi.e2e"
label="Manual owner $fixture"
[ "$MODE" != preexisting ] || : > "$SEQ_DIR/registration-account"
fail() { echo "$*" >&2; exit 1; }
db_psql() {
  printf '%s\n' "$1" >> "$SEQ_DIR/registration-sql"
  case "$1" in
    'SELECT gen_random_uuid()') echo "$fixture" ;;
    'SELECT count(*) FROM users WHERE email='*)
      [ "$1" = "SELECT count(*) FROM users WHERE email='$email'" ] || return 1
      [ ! -f "$SEQ_DIR/registration-account" ] && echo 0 || echo 1 ;;
    'SELECT id FROM users WHERE email='*)
      [ "$1" = "SELECT id FROM users WHERE email='$email' AND display_name='$label' AND NOT is_admin" ] || return 1
      [ "$MODE" != lookup-error ] || return 7
      if [ -f "$SEQ_DIR/registration-account" ]; then
        [ "$MODE" != invalid-result ] && echo "$owner" || echo not-a-uuid
      fi ;;
    'SELECT count(*) FROM users WHERE id='*)
      [ "$1" = "SELECT count(*) FROM users WHERE id='$owner' AND email='$email' AND NOT is_admin" ] || return 1
      echo 1 ;;
    'SELECT count(*) FROM runs'*) echo 0 ;;
    'DELETE FROM users'*)
      [ "$1" = "DELETE FROM users WHERE id='$owner' AND email='$email' RETURNING id" ] || return 1
      rm "$SEQ_DIR/registration-account"
      echo "$owner" ;;
    'SELECT (SELECT count(*) FROM workers'*) echo 0 ;;
    *) return 1 ;;
  esac
}
curl() {
  printf 'register\n' >> "$SEQ_DIR/registration-http"
  [[ " $* " == *" $BASE/api/auth/register "* ]] || return 1
  [ "$MODE" = not-committed ] || : > "$SEQ_DIR/registration-account"
  case "$MODE" in
    confirmed) printf '{"user":{"id":"%s"}}\n' "$owner" ;;
    empty-response) return 0 ;;
    *) return 7 ;;
  esac
}
# shellcheck disable=SC1090  # actual setup through owner confirmation and EXIT trap
source <(awk '
  /^  M5_FIXTURE=/ {emit=1; print "("}
  /\[ -n "\$\(csrf\)"/ && emit {print ")"; exit}
  emit {print}
' "$ROOT/e2e/phases/50-worker-token-binding.sh")
BASH
}
for mode in lost-response empty-response not-committed preexisting lookup-error invalid-result confirmed; do
  cases=$((cases + 1))
  rm -f "$SEQ_DIR/registration-account"
  : > "$SEQ_DIR/registration-sql"
  : > "$SEQ_DIR/registration-http"
  if output="$(manual_registration_case "$mode" 2>&1)"; then got=pass; else got=fail; fi
  calls="$(cat "$SEQ_DIR/registration-sql")"
  http="$(cat "$SEQ_DIR/registration-http")"
  valid=false
  case "$mode" in
    lost-response|empty-response)
      [ "$got" = fail ] && [ "$http" = register ] && [ ! -f "$SEQ_DIR/registration-account" ] \
        && [[ "$calls" == *'SELECT id FROM users'*'DELETE FROM users'* ]] \
        && [[ "$output" != *'owned fixture cleanup failed'* ]] && valid=true ;;
    confirmed)
      [ "$got" = pass ] && [ "$http" = register ] && [ ! -f "$SEQ_DIR/registration-account" ] \
        && [[ "$calls" != *'SELECT id FROM users'* ]] && [[ "$calls" == *'DELETE FROM users'* ]] && valid=true ;;
    not-committed)
      [ "$got" = fail ] && [ "$http" = register ] && [ ! -f "$SEQ_DIR/registration-account" ] \
        && [[ "$calls" == *'SELECT id FROM users'* ]] && [[ "$calls" != *'DELETE FROM users'* ]] \
        && [[ "$output" != *'owned fixture cleanup failed'* ]] && valid=true ;;
    preexisting)
      [ "$got" = fail ] && [ -z "$http" ] && [ -f "$SEQ_DIR/registration-account" ] \
        && [[ "$calls" == *'SELECT count(*) FROM users WHERE email='* ]] \
        && [[ "$calls" != *'SELECT id FROM users'* ]] && [[ "$calls" != *'DELETE FROM users'* ]] && valid=true ;;
    lookup-error|invalid-result)
      [ "$got" = fail ] && [ "$http" = register ] && [ -f "$SEQ_DIR/registration-account" ] \
        && [[ "$calls" != *'DELETE FROM users'* ]] \
        && [[ "$output" == *'owned fixture cleanup failed'* ]] && valid=true ;;
  esac
  if [ "$valid" = true ]; then
    passed=$((passed + 1)); echo "PASS: manual registration cleanup $mode"
  else echo "FAIL: manual registration cleanup $mode result=$got sql=[$calls] http=[$http] output=[$output]"; fi
done

bash "$ROOT/e2e/vault-modes.test.sh"
node --test "$ROOT/e2e/settlement-diagnostic.test.mjs"
node --test "$ROOT/e2e/recovery-record-projection.test.mjs"
echo "cases=$cases passed=$passed"
# Tally guard (the driver.test.sh idiom): a real run has at least 203 cases green; a zero-case or
# partially-red run must exit nonzero.
[ "$cases" -ge 203 ] && [ "$cases" -eq "$passed" ]
