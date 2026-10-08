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
      | if $step=="open" then .holds[0] += {state:"open",final_disposition:null,release_evidence:null}
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

diagnostic_trap() {
  local phase="$1" status="$2"
  RUNROOT="$SEQ_DIR"
  inventory_diagnostic_snapshot() { echo 'selected inventory diagnostic'; }
  eval "$(awk '/^trap .*inventory_diagnostic_snapshot/' "$ROOT/e2e/phases/$phase.sh")"
  exit "$status"
}
for phase in 60-schedules-sweep 72-custody-lifecycle; do
  custody_case "$phase preserves failure while emitting diagnostics" fail 'selected inventory diagnostic' diagnostic_trap "$phase" 7
  cases=$((cases + 1))
  if output="$(diagnostic_trap "$phase" 0)" && [ -z "$output" ]; then
    passed=$((passed + 1)); echo "PASS: $phase skips the failure snapshot on success"
  else echo "FAIL: $phase invoked diagnostic snapshot on success"; fi
done

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

echo "cases=$cases passed=$passed"
# Tally guard (the driver.test.sh idiom): a real run has all 121 cases green; a zero-case or
# partially-red run must exit nonzero.
[ "$cases" -ge 121 ] && [ "$cases" -eq "$passed" ]
