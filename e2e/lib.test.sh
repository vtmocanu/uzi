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
SEQ_DIR="$(mktemp -d)"
trap 'rm -rf "$SEQ_DIR"' EXIT
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
  if [ "$got" = "$want" ] && [[ "$output" == *"$diagnostic"* ]] && ! [[ "$output" == *"unexpected custody"* ]]; then
    passed=$((passed + 1)); echo "PASS: $name"
  else echo "FAIL: $name: got $got want $want; output [$output]"; fi
}
headroom_fixture() {
  : > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
  local count
  for count in "$@"; do
    jq -nc --argjson count "$count" '{aggregate:{open_holds:$count,custody_hold_limit:8},holds:[
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
printf '%s\n' '{"aggregate":{"open_holds":0},"holds":[]}' > "$SEQ_DIR/seq"; echo 1 > "$SEQ_DIR/n"
custody_case "missing ceiling cannot read green" fail "invalid admission limit" wait_custody_headroom 1 2

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

# The actual capture decoder must recognize guarded and legacy typed logs while
# preserving generation, ordering and unique-capture requirements.
eval "$(awk '/^f42_assert_capture_park\(\) \{/,/^\}/' "$ROOT/e2e/phases/52-api-outage-outbox.sh")"
fake_compose() { cat "$SEQ_DIR/events"; }
# shellcheck disable=SC2034  # read by the extracted production capture decoder
COMPOSE=(fake_compose)
export RUNROOT="$SEQ_DIR"
uzi_cli() { echo '[{"generation":2,"captures":[{"id":"capture","state":"available"}]}]'; }
capture_fixture() {
  jq -nc --arg variant "$1" '
    {run_id:"R",msg:"run claimed"} as $claim |
    {run_id:"R",msg:"run parked for transient recovery",detail:"recovering retained work before reseeding"} as $park |
    {run_id:"R",msg:"recovery: guarded inventory disposition",generation:2,capture_id:"capture",state:"uploaded"} as $capture |
    (if $variant=="legacy" then [$claim,$claim,$park,($capture+{msg:"recovery: park/early-terminal disposition outcome",claim_generation:2}),$claim]
     else [$claim,$claim,$park,$capture,$capture,$claim] end) |
    if $variant=="wrong-generation" then .[3].generation=3 | .[4].generation=3
    elif $variant=="wrong-order" then [.[0],.[1],.[3],.[2],.[4],.[5]]
    elif $variant=="two-captures" then .[4].capture_id="other"
    elif $variant=="missing-id" then .[3].capture_id=null | .[4].capture_id=null
    else . end | .[]' > "$SEQ_DIR/events"
}
capture_fixture guarded
custody_case "guarded G+1 capture retries retain one distinct capture" pass 'exact generation 2 captured' f42_assert_capture_park R 2
capture_fixture legacy
custody_case "legacy G+1 capture contract still accepted" pass 'exact generation 2 captured' f42_assert_capture_park R 2
for variant in wrong-generation wrong-order two-captures missing-id; do
  capture_fixture "$variant"
  custody_case "capture decoder refuses $variant" fail 'lacks the exact G+1' f42_assert_capture_park R 2
done

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

# The previous cleanup failed with a capture FK and silently removed source-only
# evidence when no capture existed. Pin all three phase seams to read-only admission.
for phase in 42-api-outage-readoption 46-run-health 52-api-outage-outbox; do
  cases=$((cases + 1))
  if grep -qF 'wait_custody_headroom ' "$ROOT/e2e/phases/$phase.sh" && ! grep -qF 'DELETE FROM recovery_custody_holds' "$ROOT/e2e/phases/$phase.sh"; then
    passed=$((passed + 1)); echo "PASS: $phase preserves cross-phase custody"
  else echo "FAIL: $phase still clears custody or lacks headroom wait"; fi
done

echo "cases=$cases passed=$passed"
# Tally guard (the driver.test.sh idiom): a real run has all 65 cases green; a zero-case or
# partially-red run must exit nonzero.
[ "$cases" -ge 65 ] && [ "$cases" -eq "$passed" ]
