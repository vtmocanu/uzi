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
  local step
  for step in "$@"; do
    jq -nc --arg step "$step" '{state:"open",inventory_guarded:true,generation:1,claim_generation:1,run_status:"recovery_wait",final_disposition:null,release_evidence:null,after_park:null}
      | if $step=="released" then .+{state:"released",final_disposition:"settled",release_evidence:"forge_no_output",after_park:true}
        elif $step=="wrong" then .+{state:"released",final_disposition:"no_adopted_source",release_evidence:"no_adopted_source",after_park:true}
        elif $step=="early" then .+{state:"released",final_disposition:"settled",release_evidence:"forge_no_output",after_park:false}
        elif $step=="reclaimed" then .claim_generation=2
        elif $step=="promoted" then .run_status="queued"
        elif $step=="legacy" then .+{inventory_guarded:false,state:"released",release_evidence:"no_adopted_source"}
        elif $step=="legacy-open" then .inventory_guarded=false
        elif $step=="duplicate" then [., .+{state:"released",final_disposition:"settled",release_evidence:"forge_no_output",after_park:true}]
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

# The previous cleanup failed with a capture FK and silently removed source-only
# evidence when no capture existed. Pin all three phase seams to read-only admission.
for phase in 42-api-outage-readoption 46-run-health 52-api-outage-outbox; do
  cases=$((cases + 1))
  if grep -qF 'wait_custody_headroom ' "$ROOT/e2e/phases/$phase.sh" && ! grep -qF 'DELETE FROM recovery_custody_holds' "$ROOT/e2e/phases/$phase.sh"; then
    passed=$((passed + 1)); echo "PASS: $phase preserves cross-phase custody"
  else echo "FAIL: $phase still clears custody or lacks headroom wait"; fi
done

echo "cases=$cases passed=$passed"
# Tally guard (the driver.test.sh idiom): a real run has all 41 cases green; a zero-case or
# partially-red run must exit nonzero.
[ "$cases" -ge 40 ] && [ "$cases" -eq "$passed" ]
