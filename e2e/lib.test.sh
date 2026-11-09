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

# The real capture_agent_logs (#4060), defined up front: phase code extracted by later cases now contains
# capture lines. Later cases provide RUNROOT/COMPOSE; the handoff fixture relies on the exported
# RUNROOT=$SEQ_DIR to find its snapshots. Without them the helper only reports to stderr and returns 0.
eval "$(awk '/^capture_agent_logs\(\) \{/,/^\}/' "$LIB")"
CAL_DEFINED=yes
type capture_agent_logs >/dev/null 2>&1 || CAL_DEFINED=no

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

# Run the real phase prefixes in fresh errexit-enabled shells. Force an early
# assertion failure after mutation, as happened before the first binding claim.
# The fake API persists state outside command-substitution subshells.
cat > "$SEQ_DIR/harness-state.sh" <<'HARNESS'
#!/usr/bin/env bash
set -euo pipefail
ROOT="$1" STATE="$2" SCENARIO="$3" ORIGINAL="$4"
BASE=http://fixture JAR=fixture DUMMY_ANTHROPIC=different
COMPOSE=(true)
say() { :; }; pass() { :; }; login() { :; }; wait_custody_headroom() { :; }; capture_agent_logs() { :; }
fail() { echo "$*" >&2; exit 17; }; csrf() { echo fixture; }
eval "$(awk '/^synthetic_issue_worker_capabilities\(\) \{/,/^\}/; /^set_owner_autopilot\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
apiget() {
  case "$1" in
    /api/auth/me) jq -cn --argjson value "$ORIGINAL" '{user:{autopilot_enabled:$value}}' ;;
    /api/forge/connections) echo '{"connections":[{"id":"connection","human_username":"owner-alice"}]}' ;;
    /api/forge/connections/connection/projects) echo '{"repos":[{"id":"repo2","path_with_namespace":"group/repo2"}]}' ;;
    /api/me/secrets)
      if [ -f "$STATE/token" ]; then
        echo '{"secrets":[{"id":"11111111-1111-4111-8111-111111111111","kind":"anthropic_token","label":"default","is_default":true},{"id":"22222222-2222-4222-8222-222222222222","kind":"anthropic_token","label":"console-key","is_default":false}]}'
      else echo '{"secrets":[{"kind":"anthropic_token","label":"default","is_default":true}]}'; fi ;;
    *) return 1 ;;
  esac
}
apiput() {
  case "$1" in
    /api/forge/connections/connection) echo '{"warning":"fixture"}' ;;
    /api/me/autopilot)
      value="$(printf '%s' "$2" | jq -r '.enabled')"
      if [ "$SCENARIO" = autopilot-cleanup-fail ] && [ "$value" = false ]; then return 1; fi
      printf '%s\n' "$value" > "$STATE/autopilot"
      jq -cn --argjson value "$value" '{user:{autopilot_enabled:$value}}' ;;
    /api/repos/repo2) echo 'repo enabled' >> "$STATE/events"; echo '{"repo":{"enabled":true}}' ;;
    *) return 1 ;;
  esac
}
apipost() {
  case "$1" in
    /api/me/secrets/anthropic_token) touch "$STATE/token"; echo '{}' ;;
    /api/workers) echo '{"worker":{"id":"worker"},"token":"fixture"}' ;;
    *) return 1 ;;
  esac
}
curl() {
  local payload='' arg
  while [ "$#" -gt 0 ]; do
    arg="$1"; shift
    case "$arg" in
      -d) payload="$1"; shift ;;
      */api/worker/register) printf '%s\n' "$payload" > "$STATE/register" ;;
      */api/me/secrets/anthropic_token/22222222-2222-4222-8222-222222222222)
        [ "$SCENARIO" != token-cleanup-fail ] || return 1
        rm "$STATE/token"; echo deleted >> "$STATE/events" ;;
    esac
  done
  # -d follows the URL in these calls, so record it after parsing all arguments.
  if [ -n "$payload" ]; then printf '%s\n' "$payload" > "$STATE/register"; fi
}
case "$SCENARIO" in
  autopilot*)
    eval "$(awk '/^# --- autopilot #1:/ {exit} {print}' "$ROOT/e2e/phases/29-autopilot-lifecycle.sh")" ;;
  guard)
    EXECUTOR=stub
    # Stop at the first compose call: consent must already be false when repo2
    # is enabled, even if phase 29 was skipped and owner opt-in was true.
    apiput() {
      case "$1" in
        /api/me/autopilot) printf '%s\n' "$2" > "$STATE/guard-consent"; echo '{"user":{"autopilot_enabled":false}}' ;;
        /api/repos/repo2) jq -e '.enabled == false' "$STATE/guard-consent" >/dev/null || return 1; echo guarded > "$STATE/events"; exit 17 ;;
        *) return 1 ;;
      esac
    }
    # The prefix sits inside the phase's if/else; close it for this fixture.
    eval "$(awk '/^  # Recreate the one worker/ {exit} {print}' "$ROOT/e2e/phases/45-bounded-concurrency.sh")
fi" ;;
  token*)
    eval "$(awk '/^# claim_token DESC/ {exit} {print}' "$ROOT/e2e/phases/50-worker-token-binding.sh")" ;;
  autostop)
    WTOK=fixture
    eval "$(awk '/^curl .*api\/worker\/register/ {printing=1} printing {print} printing && /could not register|synthetic worker register failed/ {exit}' "$ROOT/e2e/phases/51-auto-stop-poison.sh")" ;;
  manual)
    M5_FIXTURE=fixture M5_JOIN=fixture
    eval "$(awk '/^  curl .*api\/worker\/register/ {printing=1} printing && /^  m5_heartbeat\(\)/ {exit} printing {print}' "$ROOT/e2e/phases/50-worker-token-binding.sh")" ;;
esac
exit 17
HARNESS
harness_state_case() {
  local scenario="$1" original="$2" expected_rc="$3" expected_state="$4" rc=0 state="$SEQ_DIR/harness-$1-$2"
  cases=$((cases + 1))
  mkdir -p "$state"
  # No if/! wrapper around the child shell: its own errexit remains active.
  bash "$SEQ_DIR/harness-state.sh" "$ROOT" "$state" "$scenario" "$original" > "$state/output" 2>&1 || rc=$?
  if [ "$rc" = "$expected_rc" ] && [ "$(cat "$state/autopilot" 2>/dev/null || true)" = "$expected_state" ]; then
    passed=$((passed + 1)); echo "PASS: $scenario restores original=$original with rc=$rc"
  else echo "FAIL: $scenario original=$original rc=$rc"; cat "$state/output"; fi
}
harness_state_case autopilot false 17 false
harness_state_case autopilot true 17 true
harness_state_case autopilot-cleanup-fail false 1 true
harness_state_case guard true 17 ''
cases=$((cases + 1))
if [ "$(cat "$SEQ_DIR/harness-guard-true/events" 2>/dev/null || true)" = guarded ]; then
  passed=$((passed + 1)); echo 'PASS: phase 45 disables autopilot before exposing repo2'
else echo 'FAIL: repo2 exposed before autopilot disabled'; fi
for scenario in token token-cleanup-fail autostop manual; do
  state="$SEQ_DIR/harness-$scenario"
  mkdir -p "$state"
  rc=0
  bash "$SEQ_DIR/harness-state.sh" "$ROOT" "$state" "$scenario" false > "$state/output" 2>&1 || rc=$?
  cases=$((cases + 1))
  expected_rc=17; [ "$scenario" != token-cleanup-fail ] || expected_rc=1
  if [ "$rc" = "$expected_rc" ] && jq -e '
      .protocol_capabilities | sort == (["completion_interlock_v1","repo_agent_source_v1","agent_source_request_v1"] | sort)
    ' "$state/register" >/dev/null 2>&1; then
    passed=$((passed + 1)); echo "PASS: $scenario registers the shared claim-only capabilities"
  else echo "FAIL: $scenario capabilities/exit rc=$rc"; cat "$state/output"; fi
  if [[ "$scenario" == token* ]]; then
    cases=$((cases + 1))
    if { [ "$scenario" = token ] && [ ! -e "$state/token" ] && [ "$(cat "$state/events")" = deleted ]; } \
      || { [ "$scenario" = token-cleanup-fail ] && [ -e "$state/token" ] && grep -F 'console-key cleanup failed' "$state/output" >/dev/null; }; then
      passed=$((passed + 1)); echo "PASS: $scenario cleans up early failure or reports cleanup failure"
    else echo "FAIL: $scenario token cleanup"; cat "$state/output"; fi
  fi
done

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
    if [[ "$step" == cancelled:* ]]; then status=cancelled; step="${step#cancelled:}"; fi
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
park_fixture cancelled:open cancelled:released
custody_case "guarded cancellation waits for its final receipt" pass "" wait_forge_park_release R 1 5 cancelled
park_fixture cancelled:wrong
custody_case "cancellation refuses wrong final evidence" fail "wrong release evidence" wait_forge_park_release R 1 2 cancelled
park_fixture cancelled:early
custody_case "cancel release predating terminal outcome is refused" fail "wrong release evidence" wait_forge_park_release R 1 2 cancelled
park_fixture promoted
custody_case "cancellation refuses status change" fail "changed run/generation" wait_forge_park_release R 1 2 cancelled

# Finite forge allowance: use distinct claim/session/hold identities so folds
# cannot accidentally compare equal. Exercise the actual assertions with no clock wait.
eval "$(awk '/^assert_forge_owner_hold\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^forge_attempt_ready\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^forge_owner_hold_ready\(\) \{/,/^\}/' "$LIB")"
FORGE_BEFORE='{"status":"running","worker_id":"worker-B","session_id":"session-B","generation":3,"claim_released_at":null,"count":2,"holds":[{"id":"hold-B","guarded":true,"state":"open","disposition":null,"evidence":null}]}'
FORGE_HELD="$(printf '%s' "$FORGE_BEFORE" | jq -c '.+{status:"recovery_wait",cause:"forge_unreachable",retry:null,fail_origin:null,finished_at:null}')"
custody_case "forge finite allowance holds the same claim" pass '' assert_forge_owner_hold "$FORGE_BEFORE" "$FORGE_HELD"
FORGE_SETTLED="$(printf '%s' "$FORGE_HELD" | jq -c '.holds[0]+={state:"released",disposition:"settled",evidence:"forge_no_output",after_ack:true}')"
custody_case "forge held guarded empty receipt after ACK is allowed" pass '' assert_forge_owner_hold "$FORGE_BEFORE" "$FORGE_SETTLED"
for fold in '.count=3' '.retry="tomorrow"' '.status="failed"' '.cause=null' '.fail_origin="forge_unreachable"' '.finished_at="now"' '.worker_id="worker-A"' '.session_id="session-A"' '.generation=2' '.claim_released_at="now"' '.holds[0].id="hold-A"' '.holds=[]' '.holds += .holds' '.holds[0].guarded=false' '.holds[0].after_ack=false' '.holds[0].evidence="no_adopted_source"'; do
  mutant="$(printf '%s' "$FORGE_SETTLED" | jq -c "$fold")"
  custody_case "forge hold refuses $fold" fail 'changed claim/custody or terminalized' assert_forge_owner_hold "$FORGE_BEFORE" "$mutant"
done
legacy_before="$(printf '%s' "$FORGE_BEFORE" | jq -c '.holds[0].guarded=false')"
legacy_held="$(printf '%s' "$FORGE_HELD" | jq -c '.holds[0].guarded=false')"
custody_case "forge owner hold preserves legacy open custody" pass '' assert_forge_owner_hold "$legacy_before" "$legacy_held"
legacy_released="$(printf '%s' "$FORGE_SETTLED" | jq -c '.holds[0].guarded=false')"
custody_case "forge owner hold refuses legacy release" fail 'changed claim/custody' assert_forge_owner_hold "$legacy_before" "$legacy_released"
forge_park_posture() { printf '%s' "$FORGE_PROBE"; }
FORGE_PROBE="$FORGE_HELD"
custody_case "NULL retry owner hold is ready" pass true forge_owner_hold_ready R
FORGE_PROBE="$(printf '%s' "$FORGE_HELD" | jq -c '.retry="later"')"
custody_case "timed park is not an owner hold" pass false forge_owner_hold_ready R
FORGE_PROBE="$FORGE_BEFORE"
custody_case "Resume new ordinary generation is observable" pass true forge_attempt_ready R 2
custody_case "Resume cannot reuse old generation" pass false forge_attempt_ready R 3
FORGE_PROBE="$(printf '%s' "$FORGE_BEFORE" | jq -c '.generation=4')"
custody_case "Resume cannot skip an extra generation" pass false forge_attempt_ready R 2
FORGE_PROBE="$(printf '%s' "$FORGE_HELD" | jq -c '.status="queued"')"
custody_case "cooldown or custody queued Resume is not execution" pass false forge_attempt_ready R 2
FORGE_PROBE="$(printf '%s' "$FORGE_HELD" | jq -c '.status="failed"')"
custody_case "terminal outcome aborts owner hold polling" fail 'unexpectedly terminal' forge_owner_hold_ready R
custody_case "terminal outcome aborts resumed attempt polling" fail 'unexpectedly terminal' forge_attempt_ready R 2

# Replay the shipped Resume response assertion, with the public DTO seam.
forge_resume_response_contract() {
  # shellcheck disable=SC2034  # consumed by the extracted phase response assertion
  local RUN_B=B RESUMED_B
  apipost() { printf '%s' "$FORGE_RESUME_REPLY"; }
  eval "$(awk '/^RESUMED_B=/ {active=1} active {print} active && index($0, "Resume did not queue one ordinary attempt") {exit}' "$ROOT/e2e/phases/73-forge-unreachable-park.sh")"
}
FORGE_RESUME_REPLY='{"run":{"status":"queued","forge_park_count":2,"recovery_wait_cause":null,"recovery_retry_not_before":null,"fail_origin":null,"finished_at":null}}'
custody_case "owner Resume queues an ordinary attempt at count=2" pass '' forge_resume_response_contract
for status in claimed running; do
  FORGE_RESUME_REPLY="$(printf '%s' '{"run":{"forge_park_count":2}}' | jq -c --arg status "$status" '.run.status=$status')"
  custody_case "Resume response may already show $status after cooldown expiry" pass '' forge_resume_response_contract
done
for fold in '.run.status="failed"' '.run.forge_park_count=0' '.run.recovery_wait_cause="forge_unreachable"' '.run.recovery_retry_not_before="later"' '.run.fail_origin="forge_unreachable"' '.run.finished_at="now"'; do
  FORGE_RESUME_REPLY="$(printf '%s' '{"run":{"status":"queued","forge_park_count":2}}' | jq -c "$fold")"
  custody_case "Resume response refuses $fold" fail 'Resume did not queue one ordinary attempt' forge_resume_response_contract
done

# Issue #1324 publication-park helpers. custody_case runs each helper in a command
# substitution, so the stubs below live and die inside that subshell.
eval "$(awk '/^publication_park_ready\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^publication_hold_ready\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^run_executor_runs\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^fake_fault_create_mr\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^fake_clear_create_mr_fault\(\) \{/' "$LIB")"
pub_wrap() {
  apiget() { printf '%s' "$PUB_VIEW"; }
  fake_post() { printf '%s %s\n' "$1" "$2" >> "$SEQ_DIR/fake-post"; }
  "$@"
}
pub_fault_wrap() { : > "$SEQ_DIR/fake-post"; pub_wrap "$@"; cat "$SEQ_DIR/fake-post"; }
PUB_PARK='{"run":{"status":"recovery_wait","recovery_wait_cause":"forge_unreachable","recovery_wait_phase":"publication","recovery_retry_not_before":"2026-01-01T00:00:00Z"}}'
PUB_HOLD='{"run":{"status":"recovery_wait","recovery_wait_cause":"forge_unreachable","recovery_wait_phase":"publication","recovery_retry_not_before":null,"publication_hold_reason":"publication_retry_exhausted"}}'
PUB_VIEW="$PUB_PARK"
custody_case "publication park is ready" pass true pub_wrap publication_park_ready R
custody_case "publication park is not an owner hold" pass false pub_wrap publication_hold_ready R
PUB_VIEW="$PUB_HOLD"
custody_case "publication hold is ready" pass true pub_wrap publication_hold_ready R
custody_case "publication hold is not a timed park" pass false pub_wrap publication_park_ready R
for fold in '.run.recovery_wait_phase=null' '.run.recovery_wait_cause="provider_outage"'; do
  PUB_VIEW="$(printf '%s' "$PUB_PARK" | jq -c "$fold")"
  custody_case "park refuses $fold" pass false pub_wrap publication_park_ready R
done
PUB_VIEW="$(printf '%s' "$PUB_HOLD" | jq -c '.run.publication_hold_reason="publication_custodian_unavailable"')"
custody_case "hold refuses another hold reason" pass false pub_wrap publication_hold_ready R
PUB_VIEW="$(printf '%s' "$PUB_PARK" | jq -c '.run.recovery_wait_phase=null')"
custody_case "pre-clone park is not a publication park" pass false pub_wrap publication_park_ready R
PUB_VIEW='{"run":{"status":"failed"}}'
custody_case "terminal run aborts park polling" fail 'unexpectedly terminal' pub_wrap publication_park_ready R
custody_case "terminal run aborts hold polling" fail 'unexpectedly terminal' pub_wrap publication_hold_ready R
PUB_VIEW='{"messages":[{"kind":"text","payload":{"text":"stub work committed locally"}},{"kind":"text","payload":{"text":"other"}},{"kind":"status","payload":{"text":"stub work committed locally"}}]}'
custody_case "executor runs counts only stub text messages" pass 1 pub_wrap run_executor_runs R
PUB_VIEW='{"messages":[]}'
custody_case "no messages means no executor run" pass 0 pub_wrap run_executor_runs R
custody_case "fault helper arms create-mr with status and count" pass '/_e2e/faults {"surface":"create-mr","status":503,"count":8}' pub_fault_wrap fake_fault_create_mr 503 8
custody_case "fault clear helper sends count 0" pass '/_e2e/faults {"surface":"create-mr","count":0}' pub_fault_wrap fake_clear_create_mr_fault

# Drive the real sequential outbox creation seam, not just the admission helper.
# Each fresh claim must observe a saturated-then-free owner before create_run.
eval "$(awk '/^make_outbox_run\(\) \{/,/^\}/' "$ROOT/e2e/phases/52-api-outage-outbox.sh")"
headroom_fixture 8 7 8 7
echo 0 > "$SEQ_DIR/claims"
export REPO_ID=repo
apipost() { printf '%s\n' '{"card":{"iid":1}}'; }
wait_status() { :; }
# Registration is exercised in outage-cut.test.sh; this fixture isolates custody admission.
wait_outbox_feature() { :; }
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
    'SELECT claim_generation FROM runs'*)
      [[ "$1" != *\\* ]] || fail 'generation SQL contains literal backslash'
      echo 1;;
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
  park_calls=$((park_calls + 1))
}
park_phase_call() {
  local park_calls=0
  local -x RUN_A=R RUN_B=B RUN_C=C FB='{"generation":1}' FA='{"run":{"status":"recovery_wait"}}'
  eval "$(awk '/^PARK_GENERATION(_[BC])?=/ || /^[[:space:]]*wait_forge_park_release /' "$ROOT/e2e/phases/73-forge-unreachable-park.sh")"
  [ "$park_calls" = 3 ] || fail 'phase did not wait for timed, owner-hold and cancellation receipts'
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
custody_case "guarded typed G+1 same-claim retries retain one distinct capture" pass 'exact generation 2 captured' wait_same_claim_capture_resume R 2 0
capture_fixture legacy
custody_case "legacy typed G+1 same-claim capture still accepted" pass 'exact generation 2 captured' wait_same_claim_capture_resume R 2 0
capture_fixture single-upload
custody_case "same-claim capture precedes fresh reseed" pass 'exact generation 2 captured' wait_same_claim_capture_resume R 2 0
for variant in wrong-generation wrong-order two-captures missing-id extra-claim wrong-seed-generation; do
  capture_fixture "$variant"
  custody_case "capture decoder refuses $variant" fail 'lacks the exact G+1' wait_same_claim_capture_resume R 2 0
done

# #3644 independent restore: the production log sequence, in production order (agent/src/runner.ts
# "run claimed", agent/src/git.ts "runner clone: seeding" with seeded_from restore_pin, runner.ts
# "independent restore seeded", then the admission line), and no bundle upload.
eval "$(awk '/^wait_independent_restore_resume\(\) \{/,/^\}/' "$LIB")"
RESTORE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
RESTORE_CID="$(printf 'b%.0s' $(seq 64))"
RESTORE_PATH=/data/runner/repo/issue-1.attempt-20260101T000000Z-g2-0123456789abcdef
restore_fixture() {
  jq -nc --arg variant "$1" --arg sha "$RESTORE_SHA" --arg cid "$RESTORE_CID" --arg path "$RESTORE_PATH" '
    {run_id:"R",msg:"run claimed"} as $claim |
    {msg:"runner clone: seeding",branch:"agent/issue-1",seeded_from:"restore_pin",prior_commits:1,path:$path} as $seed |
    {run_id:"R",msg:"independent restore seeded",slot:{kind:"compose",hostname:$cid[0:12],containerId:$cid},
      crossCheck:"ok",verdict:{predecessorIncarnation:"p",boot:"same_boot"},binding:"on_default",pinGeneration:1,
      sourceSha:$sha,captureId:"cap",recordState:"pending",attemptPath:$path,seededHead:$sha,claim_generation:2} as $restored |
    {run_id:"R",msg:"recovery: prior open custody holds inventoried after clone",claim_generation:2} as $ready |
    {run_id:"R",msg:"recovery: bundle uploaded (durable capture)",server_capture_id:"capture",server_state:"available"} as $capture |
    [$claim,$claim,$seed,$restored,$ready] |
    if $variant=="owed" then .[3].binding="owed"
    elif $variant=="pin-ref" then .[3].binding="recovery_pin_ref"
    elif $variant=="wrong-pin-generation" then .[3].pinGeneration=2
    elif $variant=="extra-capture" then [.[0],.[1],$capture,.[2],.[3],.[4]]
    elif $variant=="extra-claim" then . + [$claim]
    elif $variant=="wrong-order" then [.[0],.[1],.[3],.[2],.[4]]
    elif $variant=="admission-before-restore" then [.[0],.[1],.[2],.[4],.[3]]
    elif $variant=="wrong-generation" then .[2].path="/data/runner/repo/issue-1.attempt-20260101T000000Z-g3-0123456789abcdef"
    elif $variant=="mismatched-attempt-path" then .[3].attemptPath="/data/runner/repo/issue-1.attempt-20260101T000000Z-g2-fedcba9876543210"
    elif $variant=="disallowed-binding" then .[3].binding="recovery_pin_ref"
    elif $variant=="seeded-head-mismatch" then .[3].seededHead="cccccccccccccccccccccccccccccccccccccccc"
    elif $variant=="short-source-sha" then .[3].sourceSha="aaaa" | .[3].seededHead="aaaa"
    elif $variant=="slot-not-compose" then .[3].slot={kind:"hosted",podUid:"u",container:"worker"}
    elif $variant=="cross-check-failed" then .[3].crossCheck="failed"
    elif $variant=="empty-verdict" then .[3].verdict={}
    elif $variant=="admission-wrong-generation" then .[4].claim_generation=3
    elif $variant=="not-restore-pin-seed" then .[2].seeded_from="tracking"
    elif $variant=="two-restore-lines" then [.[0],.[1],.[2],.[3],.[3],.[4]]
    else . end | .[]' > "$SEQ_DIR/events"
}
restore_globals_case() {
  wait_independent_restore_resume R 2 agent/issue-1 owed,on_default 0 >/dev/null
  [ "$RESTORE_BINDING:$RESTORE_SOURCE_SHA:$RESTORE_SEEDED_HEAD:$RESTORE_ATTEMPT_PATH" = "$1:$RESTORE_SHA:$RESTORE_SHA:$RESTORE_PATH" ] \
    || fail "restore fields not exported: $RESTORE_BINDING $RESTORE_SOURCE_SHA $RESTORE_SEEDED_HEAD $RESTORE_ATTEMPT_PATH"
  [ "$RESTORE_CAPTURE_ID:$RESTORE_SLOT_CONTAINER:$RESTORE_SLOT_HOSTNAME" = "cap:$RESTORE_CID:${RESTORE_CID:0:12}" ] \
    || fail "restore slot fields not exported"
  [ "$RESTORE_PRED_INCARNATION" = p ] || fail "restore predecessor incarnation not exported: [$RESTORE_PRED_INCARNATION]"
  echo 'restore fields exported'
}
restore_fixture valid
custody_case "independent restore accepts on_default" pass 'restored independently' wait_independent_restore_resume R 2 agent/issue-1 on_default 0
custody_case "independent restore exports the seeded fields" pass 'restore fields exported' restore_globals_case on_default
restore_fixture owed
custody_case "independent restore accepts owed" pass 'restored independently' wait_independent_restore_resume R 2 agent/issue-1 owed,on_default 0
custody_case "independent restore exports the owed binding" pass 'restore fields exported' restore_globals_case owed
restore_fixture pin-ref
custody_case "independent restore accepts recovery_pin_ref when allowed" pass 'binding=recovery_pin_ref' wait_independent_restore_resume R 2 agent/issue-1 owed,on_default,recovery_pin_ref 0
custody_case "independent restore refuses recovery_pin_ref when it is not allowed" fail 'independent-restore G+1 sequence' wait_independent_restore_resume R 2 agent/issue-1 owed,on_default 0
restore_fixture owed
custody_case "independent restore refuses owed when only on_default is allowed" fail 'independent-restore G+1 sequence' wait_independent_restore_resume R 2 agent/issue-1 on_default 0
for variant in wrong-pin-generation extra-capture extra-claim wrong-order admission-before-restore wrong-generation mismatched-attempt-path \
  disallowed-binding seeded-head-mismatch short-source-sha slot-not-compose cross-check-failed empty-verdict admission-wrong-generation \
  not-restore-pin-seed two-restore-lines; do
  restore_fixture "$variant"
  custody_case "independent restore refuses $variant" fail 'independent-restore G+1 sequence' wait_independent_restore_resume R 2 agent/issue-1 owed,on_default 0
done
restore_fixture valid
custody_case "independent restore refuses another branch" fail 'independent-restore G+1 sequence' wait_independent_restore_resume R 2 agent/issue-2 on_default 0
custody_case "independent restore refuses another generation" fail 'independent-restore G+1 sequence' wait_independent_restore_resume R 3 agent/issue-1 on_default 0
custody_case "independent restore needs the branch" fail 'needs the run' wait_independent_restore_resume R 2 "" on_default 0

# Phase 52's wrapper: the run's branch comes from the database, the allowed bindings are owed,on_default,
# and the seed witness runs only after the log sequence is proved.
eval "$(awk '/^f42_run_branch\(\) \{/,/^\}/' "$ROOT/e2e/phases/52-api-outage-outbox.sh")"
f42_case() {
  # runs.branch is NULL for a live issue run: only issue_iid is readable, as in the database.
  rb_run_field() { case "$2" in issue_iid) echo "${F42_IID-1}";; branch) echo "";; *) echo "unexpected column $2" >&2; return 1;; esac; }
  restore_assert_seed_witness() { echo "witnessed $1 $2 $3"; }
  f42_assert_capture_resume R 2 0
}
f42_branch_case() {
  rb_run_field() { echo "$F42_IID"; }
  f42_run_branch R
}
restore_fixture owed
custody_case "phase 52 accepts an owed pin and runs the seed witness" pass 'witnessed R agent/issue-1 2' f42_case
F42_IID=7 custody_case "phase 52 branch derivation names the issue" pass 'agent/issue-7' f42_branch_case
for bad in "" 0 007 x1; do
  F42_IID="$bad" custody_case "phase 52 branch derivation refuses issue_iid [$bad]" fail "" f42_branch_case
done
restore_fixture owed
F42_IID='' custody_case "phase 52 refuses a run without an issue_iid" fail 'cannot derive the branch' f42_case
restore_fixture disallowed-binding
custody_case "phase 52 refuses a recovery_pin_ref binding" fail 'independent-restore G+1 sequence' f42_case
restore_fixture extra-capture
custody_case "phase 52 refuses a bundle capture on the restore route" fail 'independent-restore G+1 sequence' f42_case

# The seed-time witness: the bare's restore basis must agree with the log, and while S is live P must be
# unchanged. The witness container is replaced by canned outputs (the real one needs the compose stack).
eval "$(awk '/^restore_assert_seed_witness\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^_restore_p_slots\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^_restore_journal_value\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^_restore_journal_bare\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^restore_assert_agent_identity\(\) \{/,/^\}/' "$LIB")"
witness_case() {
  local variant="$1" pclone=/data/runner/repo/issue-1.attempt-20260101T000000Z-g1-1111111111111111
  # Names here must not collide with the locals of restore_assert_seed_witness: the fakes below are dynamically scoped.
  local pjournal restore run_state=running digest='digest=d1 head=ref: refs/heads/agent/issue-1 tip=t1' pin
  pjournal="$(jq -nc --arg c "$pclone" '{runId:"R",clonePath:$c,attemptId:"20260101T000000Z-g1-1111111111111111"}')"
  restore="$(jq -nc --arg c "$pclone" --arg sha "$RESTORE_SHA" '{restore:{version:1,predecessor:{clonePath:$c,generation:1},
    slotOwner:{worker:"p",epoch:"e1"},pin:{captureId:"cap",generation:1,sourceSha:$sha,finalization:false},restoreTip:$sha},restoreStage:"ready",runId:"R"}')"
  pin="$(jq -nc --arg sha "$RESTORE_SHA" '{runId:"R",branch:"agent/issue-1",generation:1,sourceSha:$sha,captureId:"cap",mac:"x"}')"
  # Slot lines are "name<TAB>digest<TAB>owner<TAB>epoch": k1 is P's clone slot (owner p, epoch e1, the restore basis's
  # slotOwner), k3 is another slot of the same process (p, epoch e2), k2 belongs to another worker (q).
  local slots_before slots_after holds_before='h1 open 1' holds_after='h1 open 1' bound=on_default hmode=descendant sh="$RESTORE_SHA" anc=yes
  slots_before="$(printf './k1.json\taaa\tp\te1\n./k2.json\tbbb\tq\te9\n./k3.json\teee\tp\te2\n')"; slots_after="$slots_before"
  case "$variant" in
    p-slot-changed) slots_after="$(printf './k1.json\tccc\tp\te1\n./k2.json\tbbb\tq\te9\n./k3.json\teee\tp\te2\n')";;
    p-slot-removed) slots_after="$(printf './k2.json\tbbb\tq\te9\n./k3.json\teee\tp\te2\n')";;
    same-owner-other-epoch-changed) slots_after="$(printf './k1.json\taaa\tp\te1\n./k2.json\tbbb\tq\te9\n./k3.json\tfff\tp\te2\n')";;
    same-owner-other-epoch-removed) slots_after="$(printf './k1.json\taaa\tp\te1\n./k2.json\tbbb\tq\te9\n')";;
    other-slot-changed) slots_after="$(printf './k1.json\taaa\tp\te1\n./k2.json\tccc\tq\te9\n./k3.json\teee\tp\te2\n')";;
    other-slot-removed) slots_after="$(printf './k1.json\taaa\tp\te1\n./k3.json\teee\tp\te2\n')";;
    no-p-slot) slots_before="$(printf './k2.json\tbbb\tq\te9\n./k3.json\teee\tp\te2\n')"; slots_after="$slots_before";;
    two-p-slots) slots_before="$(printf './k1.json\taaa\tp\te1\n./k2.json\tbbb\tq\te9\n./k4.json\tggg\tp\te1\n')"; slots_after="$slots_before";;
    slot-added) slots_after="$(printf './k1.json\taaa\tp\te1\n./k2.json\tbbb\tq\te9\n./k3.json\teee\tp\te2\n./k5.json\tddd\tp\te1\n')";;
    hold-changed) holds_after='h1 released 1';;
    hold-removed) holds_after='';;
    binding-unevidenced|terminal-binding-unevidenced) bound=owed;;
    head-exact-mismatch) hmode=exact; sh=cccccccccccccccccccccccccccccccccccccccc;;
    head-exact-ok) hmode=exact;;
    head-descendant) sh=cccccccccccccccccccccccccccccccccccccccc;;
    head-not-descendant) sh=cccccccccccccccccccccccccccccccccccccccc; anc=no;;
  esac
  jq -nc --arg j "$pjournal" --arg c "$pclone" --arg d "$digest" --arg s "$slots_before" --arg h "$holds_before" \
    '{journal:$j,clonePath:$c,digest:$d,slots:$s,holds:$h}' > "$SEQ_DIR/.restore-p-R.json"
  printf '%s' "$slots_after" > "$SEQ_DIR/w-slots"; printf '%s' "$holds_after" > "$SEQ_DIR/w-holds"
  printf '%s' "$bound" > "$SEQ_DIR/w-bound"; printf '%s' "$sh" > "$SEQ_DIR/w-shead"; printf '%s' "$anc" > "$SEQ_DIR/w-anc"
  case "$variant" in
    basis-tip) restore="$(jq -c --arg s cccccccccccccccccccccccccccccccccccccccc '.restore.restoreTip=$s' <<<"$restore")";;
    basis-generation) restore="$(jq -c '.restore.pin.generation=2' <<<"$restore")";;
    basis-capture) restore="$(jq -c '.restore.pin.captureId="other"' <<<"$restore")";;
    basis-slot-owner-worker) restore="$(jq -c '.restore.slotOwner.worker="q"' <<<"$restore")";;
    basis-no-slot-owner) restore="$(jq -c 'del(.restore.slotOwner)' <<<"$restore")";;
    basis-predecessor) restore="$(jq -c '.restore.predecessor.clonePath="/elsewhere"' <<<"$restore")";;
    no-restore) restore="";;
    terminal) run_state=completed; digest='digest=changed head=x tip=y';;
    terminal-no-restore) run_state=completed; restore="";;
    terminal-pin-absent) run_state=completed; pin="";;
    terminal-binding-unevidenced) run_state=completed;;
    p-journal-changed) pjournal="${pjournal%\}},\"x\":1}";;
    p-clone-changed) digest='digest=d2 head=ref: refs/heads/agent/issue-1 tip=t1';;
    pin-sha) pin="$(jq -c '.sourceSha="dddddddddddddddddddddddddddddddddddddddd"' <<<"$pin")";;
    pin-absent) pin="";;
  esac
  local other=''
  [ "$variant" != other-run ] || other="$(jq -c '.runId="Z"' <<<"$restore")"
  { printf 'bare\tuzi-recovery.agent/issue-1.clone\t%s\n' "$pjournal"
    [ -z "$restore" ] || printf 'bare\tuzi-restore.agent/issue-1.journal\t%s\n' "$restore"
    [ -z "$other" ] || printf 'bare\tuzi-restore.agent/issue-1.journal\t%s\n' "$other"
  } > "$SEQ_DIR/w-journals"
  printf '%s' "$digest" > "$SEQ_DIR/w-digest"
  printf '%s' "$pin" > "$SEQ_DIR/w-pin"
  db_psql() { echo "$run_state"; }
  db_psql_rows() { cat "$SEQ_DIR/w-holds"; echo; }
  restore_agent_identity() { echo "$RESTORE_CID ${RESTORE_CID:0:12}"; }
  restore_witness_exec() {
    case "$1" in
      journals) cat "$SEQ_DIR/w-journals";;
      digest) cat "$SEQ_DIR/w-digest";;
      pin) [ -s "$SEQ_DIR/w-pin" ] && cat "$SEQ_DIR/w-pin";;
      shead) cat "$SEQ_DIR/w-shead";;
      slots) cat "$SEQ_DIR/w-slots";;
      isanc) cat "$SEQ_DIR/w-anc";;
      # bind BARE KIND RUN SHA GENERATION: evidenced only for the kind the bare is staged with, in the restore journal's bare.
      bind) [ "$2:$3:$4:$5:$6" = "bare:$(cat "$SEQ_DIR/w-bound"):R:$RESTORE_SHA:1" ] && echo yes || echo no;;
    esac
  }
  RESTORE_SOURCE_SHA="$RESTORE_SHA" RESTORE_SEEDED_HEAD="$RESTORE_SHA" RESTORE_CAPTURE_ID=cap RESTORE_SLOT_CONTAINER="$RESTORE_CID"
  RESTORE_SLOT_HOSTNAME="${RESTORE_CID:0:12}" RESTORE_ATTEMPT_PATH="$RESTORE_PATH" RESTORE_BINDING=on_default RESTORE_PRED_INCARNATION=p
  [ "$variant" != slot-hostname ] || RESTORE_SLOT_HOSTNAME=ffffffffffff
  if [ "$variant" = slot-container ]; then
    RESTORE_SLOT_CONTAINER="$(printf 'e%.0s' $(seq 64))"
    RESTORE_SLOT_HOSTNAME="${RESTORE_SLOT_CONTAINER:0:12}"
  fi
  restore_assert_seed_witness R agent/issue-1 2 "$hmode"
}
custody_case "seed witness accepts a live restore" pass 'basis=pin=seed' witness_case valid
custody_case "seed witness reports the binding it evidenced" pass 'on_default evidenced in the bare' witness_case valid
custody_case "seed witness accepts a terminal run whose predecessor is gone" pass 'terminal run' witness_case terminal
custody_case "seed witness accepts a terminal run whose pin record is gone, with a note" pass 'not readable' witness_case terminal-pin-absent
custody_case "seed witness requires the retained basis of a terminal run" fail 'no restore journal' witness_case terminal-no-restore
custody_case "seed witness requires a readable pin record for a live run" fail 'for live run R is not readable' witness_case pin-absent
custody_case "seed witness refuses a binding the bare does not evidence" fail 'is not evidenced' witness_case binding-unevidenced
custody_case "seed witness refuses an unevidenced binding on a terminal run too" fail 'is not evidenced' witness_case terminal-binding-unevidenced
custody_case "seed witness accepts an authority slot set that only grew" pass 'clone authority slot and holds unchanged' witness_case slot-added
custody_case "seed witness refuses a changed predecessor clone slot" fail 'predecessor-owned authority slot file changed' witness_case p-slot-changed
custody_case "seed witness refuses a vanished predecessor clone slot" fail 'predecessor-owned authority slot file changed' witness_case p-slot-removed
custody_case "seed witness accepts a changed slot of the same owner at another epoch" pass 'clone authority slot and holds unchanged' witness_case same-owner-other-epoch-changed
custody_case "seed witness accepts a vanished slot of the same owner at another epoch" pass 'clone authority slot and holds unchanged' witness_case same-owner-other-epoch-removed
custody_case "seed witness accepts a changed slot of another owner" pass 'clone authority slot and holds unchanged' witness_case other-slot-changed
custody_case "seed witness accepts a vanished slot of another owner" pass 'clone authority slot and holds unchanged' witness_case other-slot-removed
custody_case "seed witness requires a clone slot before the restart" fail 'exactly one authority slot owned by predecessor p at epoch e1 before the restart, found 0' witness_case no-p-slot
custody_case "seed witness refuses two clone slots before the restart" fail 'found 2' witness_case two-p-slots
custody_case "seed witness refuses a slotOwner worker that is not the predecessor incarnation" fail 'is not the predecessor incarnation' witness_case basis-slot-owner-worker
custody_case "seed witness refuses a basis without a slotOwner" fail 'no readable slotOwner' witness_case basis-no-slot-owner
custody_case "seed witness refuses a changed custody hold" fail 'custody holds of run R changed' witness_case hold-changed
custody_case "seed witness refuses a vanished custody hold" fail 'custody holds of run R changed' witness_case hold-removed
custody_case "seed witness accepts S at exactly the seeded head" pass 'exact)' witness_case head-exact-ok
custody_case "seed witness refuses S away from the seeded head in exact mode" fail 'is not the seeded head' witness_case head-exact-mismatch
custody_case "seed witness accepts S descending from the seeded head" pass 'descendant)' witness_case head-descendant
custody_case "seed witness refuses S not descending from the seeded head" fail 'not an ancestor of the successor HEAD' witness_case head-not-descendant
for variant in basis-tip basis-generation basis-capture; do
  custody_case "seed witness refuses a $variant disagreement" fail 'disagrees with the log' witness_case "$variant"
done
custody_case "seed witness refuses a basis naming another predecessor clone" fail 'different predecessor clone' witness_case basis-predecessor
custody_case "seed witness requires a restore journal for a live run" fail 'no restore journal' witness_case no-restore
custody_case "seed witness refuses a pin record that disagrees" fail 'pin record' witness_case pin-sha
custody_case "seed witness refuses a changed predecessor journal" fail 'predecessor journal of run R changed' witness_case p-journal-changed
custody_case "seed witness refuses a changed predecessor clone" fail 'predecessor clone of run R changed' witness_case p-clone-changed
custody_case "seed witness refuses a slot hostname that is not the id prefix" fail 'slot hostname' witness_case slot-hostname
custody_case "seed witness refuses a slot that is not the agent container" fail 'logged slot container' witness_case slot-container
custody_case "seed witness ignores a restore journal of another run" pass 'basis=pin=seed' witness_case other-run

# The witness `slots` mode prints each slot file's owner and epoch through slot_fields, extracted here from the
# script text the witness container runs. The slot lines are built like agent/src/completion-authority.ts seal
# writes them (compact JSON, key order version, key, state, epoch, seq, owner, pending, ...), one with an
# owner-shaped fragment inside a string field, which JSON escapes.
# (the function's body is a heredoc holding shell functions of its own, so the range ends at the first brace line after it)
eval "$(awk '/^_restore_witness_script\(\) \{/ { f = 1 } f { print } /^WITNESS$/ { w = 1 } w && /^\}/ { exit }' "$LIB")"
eval "$(_restore_witness_script | awk '/^slot_fields\(\) \{/,/^\}/')"
slot_fields_case() {
  local key out version
  key="$(printf 'a%.0s' $(seq 64))"
  # The production slot version, read from the store's own constant so the fixture cannot drift from it.
  version="$(sed -n 's/^const SLOT_VERSION = "\([^"]*\)";$/\1/p' "$ROOT/agent/src/completion-authority.ts")"
  [ -n "$version" ] || fail "SLOT_VERSION not found in agent/src/completion-authority.ts"
  line() { jq -nc --arg version "$version" --arg key "$key" --arg epoch "$1" --arg worker "$2" --arg fp "$3" \
    '{version:$version,key:$key,state:"active",epoch:$epoch,seq:3,owner:{worker:$worker,executor:null},
      pending:(if $fp == "" then null else {operation:"op",kind:"destructive",footprint:$fp,status:"running",residue:[]} end),
      retiring:null,publication:null,codexMarked:false,mac:"m"}'; }
  out="$(line 11111111-aaaa 22222222-bbbb '' | slot_fields)"
  [ "$out" = "22222222-bbbb 11111111-aaaa" ] || fail "plain slot: worker/epoch [$out]"
  out="$(line e2 w2 '{"owner":{"worker":"bad"' | slot_fields)"
  [ "$out" = "w2 e2" ] || fail "slot with an owner-shaped fragment in a string field: [$out]"
  line e3 w3 'x' | grep -q 'owner' || fail "fixture is not canonical slot JSON"
  line e2 w2 '{"owner":{"worker":"bad"' | grep -qF '\"owner\":{\"worker\":\"bad\"' || fail "fixture does not carry the escaped fragment"
  out="$(line e4 w4 '' | sed 's/^{"version":"[^"]*"/{"version":1/' | slot_fields)"
  [ -z "$out" ] || fail "a slot with a non-production version must print nothing: [$out]"
  out="$(printf '{"version":"%s","key":"k","owner":{"worker":"w"}}\n' "$version" | slot_fields)"
  [ -z "$out" ] || fail "a record outside the canonical key order must print nothing: [$out]"
  echo 'slot fields extracted'
}
custody_case "slot owner and epoch are read from canonical slot JSON, not from string fields" pass 'slot fields extracted' slot_fields_case

identity_case() {
  local fake_after="$2"
  restore_agent_identity() { echo "$fake_after"; }
  restore_assert_agent_identity "label" "$1"
}
custody_case "agent identity accepts the same container" pass 'same agent container' identity_case "$RESTORE_CID ${RESTORE_CID:0:12}" "$RESTORE_CID ${RESTORE_CID:0:12}"
custody_case "agent identity refuses a recreated container" fail 'changed across the kill/start cycle' \
  identity_case "$(printf 'e%.0s' $(seq 64)) eeeeeeeeeeee" "$RESTORE_CID ${RESTORE_CID:0:12}"
custody_case "agent identity refuses a hostname that is not the id prefix" fail 'not the container id prefix' \
  identity_case "$RESTORE_CID ffffffffffff" "$RESTORE_CID ffffffffffff"

cap2_recovery_contract() {
  local mode="$1" RUN_KA=R RUN_KB=S fixture_generation=2
  # shellcheck disable=SC2034  # used by the old real phase statements in the regression
  local RWC_KA RWC_KB
  [ "$mode" != extra-generation ] || fixture_generation=3
  db_psql() { case "$1" in *claim_generation*) echo "$fixture_generation";; *) echo 0;; esac; }
  wait_independent_restore_resume() {
    [ "$mode" != missing-capture ] || fail 'cap2 recovery missing exact capture'
    [ "$2:$3:$4" = 2:agent/fixture:on_default ] || fail 'cap2 recovery lost generation, branch or binding'
  }
  restore_assert_seed_witness() { [ "$1:$2:$3" = "R:agent/fixture:2" ] || fail 'cap2 witness lost run, branch or generation'; }
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
  AS_RUN=A; AS_RUN_RF=D; AS_RUN2=B; AS_RUN3=C
  eval "$(awk '/^wait_status\(\) \{/,/^\}/' "$LIB")"
  for run in "$AS_RUN" "$AS_RUN_RF" "$AS_RUN2" "$AS_RUN3"; do
    echo 0 > "$SEQ_DIR/as-$run-polls"
    echo pending > "$SEQ_DIR/as-$run-state"
  done
  apipost() {
    [ "$2" = '{"kind":"cancel","body":""}' ] || fail 'unexpected autoselect input'
    case "$1" in /api/runs/A/inputs|/api/runs/B/inputs|/api/runs/C/inputs|/api/runs/D/inputs) ;; *) fail 'unexpected autoselect cancel route';; esac
    [ "$mode" != post-failed ] || return 1
    echo '{}'
  }
  apiget() {
    local run="${1##*/}" n status=awaiting_approval
    case "$run" in A|B|C|D) ;; *) fail 'unexpected autoselect status route';; esac
    n="$(cat "$SEQ_DIR/as-$run-polls")"
    echo $((n + 1)) > "$SEQ_DIR/as-$run-polls"
    if [ "$mode" = failed ]; then status=failed
    elif [ "$n" -ge 1 ]; then status=cancelled; echo cancelled > "$SEQ_DIR/as-$run-state"
    fi
    jq -nc --arg status "$status" '{run:{status:$status,failure_reason:"fixture terminal outcome"}}'
  }
  eval "$(awk '/^apipost "\/api\/runs\/\$AS_RUN(2|3|_RF)?\/inputs"/ || /^wait_status "\$AS_RUN(2|3|_RF)?" cancelled/' "$ROOT/e2e/phases/48-auto-selection.sh")"
  for run in "$AS_RUN" "$AS_RUN_RF" "$AS_RUN2" "$AS_RUN3"; do
    [ "$(cat "$SEQ_DIR/as-$run-state")" = cancelled ] || fail 'token teardown would start with cancellation still pending'
  done
  echo 'all owned cancellations observed before token teardown'
}
custody_case "autoselect observes all four cancellations before credential teardown" pass 'all owned cancellations observed' autoselect_cancel_boundary delayed
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
  local RUN_C=b4dec5cf-e6d4-413b-b3a8-0005a2835707
  # shellcheck disable=SC2034  # read by the extracted owning phase statements
  local GEN_X0=1 GEN_X1=2 GEN_ED_ORIGINAL=1 GEN_KA_ORIGINAL=1 GEN_KB_ORIGINAL=1 GEN_F1=1 GEN_F2=1 GEN_F2_OBSERVED=3 GEN_C_ORIGINAL=1
  # shellcheck disable=SC2034  # read by the extracted phase-42 boundary
  local RA_RESTORE_AT=2026-10-08T09:08:20Z
  echo '{"holds":[]}' > "$SEQ_DIR/boundary-holds"
  for entry in "$X:1" "$X:2" "$ED:1" "$RUN_KA:1" "$RUN_KB:1" "$RUN_F1:1" "$RUN_F2:1" "$RUN_F2:3" "$RUN_C:1" "$RUN_C:2" "$UUID:1"; do
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
  [ "$before" = 11 ] && [ "$after" -lt 10 ] || fail 'next sweep still blocked by deliberate fixture holds'
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
    # capture_agent_logs reads the agent with ps/logs (#4060): answer them without touching the
    # lifecycle numbering, which counts only `up ... --force-recreate agent` calls by argv. ps runs in
    # a command substitution, so a counter bumped there would be lost anyway.
    case "$*" in
      *"ps -a -q agent") echo handoff-container-id; return 0 ;;
      *"logs --no-color agent") echo handoff-outgoing-process; return 0 ;;
    esac
    printf 'compose:%s:%s:%s\n' "${UZI_E2E_RECOVERY_FAST_REDRIVE+x}" "$(printenv UZI_E2E_RECOVERY_FAST_REDRIVE || :)" "$*" >> "$SEQ_DIR/handoff-timing"
    case "$*" in
      *" up "*"--force-recreate agent"*) ;;
      *) return 0 ;;
    esac
    HANDOFF_TEST_COMPOSE_CALLS=$((HANDOFF_TEST_COMPOSE_CALLS + 1))
    # Evidence order: by the time the Nth recreate runs, at least N agent-log snapshots must exist.
    if [ "$(find "$SEQ_DIR/artifacts/agent-logs" -name '*.log' 2>/dev/null | wc -l | tr -d ' ')" -ge "$HANDOFF_TEST_COMPOSE_CALLS" ] \
        && grep -qs handoff-outgoing-process "$SEQ_DIR"/artifacts/agent-logs/*.log; then
      echo yes >> "$SEQ_DIR/handoff-snap"
    else
      echo no >> "$SEQ_DIR/handoff-snap"
    fi
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
  : > "$SEQ_DIR/handoff-snap"
  rm -rf "$SEQ_DIR/artifacts/agent-logs"
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
      # Every recreate (activation and the EXIT-trap restore) saw a fresh snapshot of the outgoing agent.
      recreates="$(grep -c -- ' up .*--force-recreate agent' "$SEQ_DIR/handoff-timing" || true)"
      snaps="$(grep -c '^yes$' "$SEQ_DIR/handoff-snap" || true)"
      [ "$recreates" -ge 1 ] && [ "$snaps" = "$recreates" ] && ! grep -q '^no$' "$SEQ_DIR/handoff-snap" || valid=false
    fi
  fi
  if [ "$valid" = true ]; then
    passed=$((passed + 1)); echo "PASS: handoff timing scope $spec"
  else echo "FAIL: handoff timing scope $spec: result=$got events=[$events] snaps=[$(cat "$SEQ_DIR/handoff-snap")] output=[$output]"; fi
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

# Phase 50's API-created ephemeral peer may legitimately be reaped while unbound.
eval "$(awk '/^fixture_worker_absent\(\) \{/,/^\}/' "$LIB")"
eval "$(awk '/^remove_fixture_ephemeral_worker\(\) \{/,/^\}/' "$LIB")"
ephemeral_cleanup_case() (
  local mode="$1" id=11111111-1111-4111-8111-111111111111 owner=22222222-2222-4222-8222-222222222222
  db_psql() {
    case "$1" in
      DELETE*)
        [[ "$1" == *"id='$id' AND user_id='$owner' AND ephemeral RETURNING id"* ]] || return 90
        case "$mode" in deleted) printf '%s' "$id";; wrong-id) printf '%s' "$owner";; delete-error) return 1;; esac ;;
      SELECT*)
        [[ "$1" == "SELECT count(*) FROM workers WHERE id='$id'" ]] || return 91
        case "$mode" in present) printf 1;; count-error) return 1;; malformed) printf unknown;; *) printf 0;; esac ;;
      *) return 92 ;;
    esac
  }
  remove_fixture_ephemeral_worker "$id" "$owner"
)
for mode in deleted reaped present delete-error count-error malformed wrong-id; do
  cases=$((cases + 1))
  case "$mode" in deleted|reaped) want=pass;; *) want=fail;; esac
  if output="$(ephemeral_cleanup_case "$mode" 2>&1)"; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ]; then
    passed=$((passed + 1)); echo "PASS: phase50 ephemeral cleanup $mode"
  else echo "FAIL: phase50 ephemeral cleanup $mode result=$got expected=$want: $output"; fi
done

# Extract and drive the shipped whole window, including both 409 checks and tuple/audit
# snapshots. This same extraction also executes the old inline block for red proof.
phase50_ephemeral_case() {
  bash -euo pipefail -s -- "$ROOT" "$SEQ_DIR" "$1" <<'BASH'
ROOT="$1" SEQ_DIR="$2" MODE="$3"
BASE=http://fixture.invalid JAR=unused
M5_OWNER=22222222-2222-4222-8222-222222222222 M5_FIXTURE=33333333-3333-4333-8333-333333333333 M5_EPHEMERAL=""
printf 0 > "$SEQ_DIR/phase50-seeds"
printf 0 > "$SEQ_DIR/phase50-present"
: > "$SEQ_DIR/phase50-methods"
fail() { echo "$*" >&2; exit 1; }
m5_uuid() { [[ "$1" =~ ^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$ ]]; }
csrf() { printf dummy-csrf; }
apipost() {
  local n
  n=$(( $(cat "$SEQ_DIR/phase50-seeds") + 1 ))
  printf '%s' "$n" > "$SEQ_DIR/phase50-seeds"
  printf 1 > "$SEQ_DIR/phase50-present"
  printf 0 > "$SEQ_DIR/phase50-snapshots"
  printf '{"worker":{"id":"11111111-1111-4111-8111-%012d"}}' "$n"
}
db_psql() {
  local n snapshots
  n="$(cat "$SEQ_DIR/phase50-seeds")"
  case "$1" in
    UPDATE*)
      [[ "$1" == *"id='$M5_EPHEMERAL' AND user_id='$M5_OWNER'"* && "$1" == *'AND NOT ephemeral RETURNING id'* ]] || return 90
      if [ "$MODE" = seed-reaped ] && [ "$n" = 1 ]; then printf 0 > "$SEQ_DIR/phase50-present"; return; fi
      printf '%s' "$M5_EPHEMERAL" ;;
    'SELECT jsonb_build_object('* )
      snapshots=$(( $(cat "$SEQ_DIR/phase50-snapshots") + 1 ))
      printf '%s' "$snapshots" > "$SEQ_DIR/phase50-snapshots"
      if [ "$MODE" = snapshot-error ]; then return 1; fi
      if [ "$MODE" = snapshot-present ] && [ "$snapshots" = 1 ]; then return; fi
      if [ "$MODE" = all-reaped ] || { [ "$n" = 1 ] && {
          { [ "$MODE" = before-reaped ] && [ "$snapshots" = 1 ]; } ||
          { [ "$MODE" = after-post-reaped ] && [ "$snapshots" = 2 ]; } ||
          { [ "$MODE" = after-delete-reaped ] && [ "$snapshots" = 3 ]; }; }; }; then
        printf 0 > "$SEQ_DIR/phase50-present"; return
      fi
      if [ "$MODE" = tuple-change ] && [ "$snapshots" = 2 ]; then printf '{"tuple":["changed"],"events":[]}'; return; fi
      printf '{"tuple":[null,null,null,null],"events":[]}' ;;
    DELETE*)
      [[ "$1" == *"id='$M5_EPHEMERAL' AND user_id='$M5_OWNER' AND ephemeral RETURNING id"* ]] || return 91
      case "$MODE" in
        cleanup-reaped) printf 0 > "$SEQ_DIR/phase50-present";;
        cleanup-present) ;;
        *) printf 0 > "$SEQ_DIR/phase50-present"; printf '%s' "$M5_EPHEMERAL" ;;
      esac ;;
    'SELECT count(*) FROM workers WHERE id='*)
      [[ "$1" != *user_id* ]] || return 92
      cat "$SEQ_DIR/phase50-present" ;;
    'SELECT count(*) FROM workers WHERE user_id='*) printf 1 ;;
    *) return 93 ;;
  esac
}
curl() {
  local method="" n code=409 message='manual cordon is only supported for persistent workers'
  while [ "$#" -gt 0 ]; do
    case "$1" in -X) method="$2"; shift 2;; *) shift;; esac
  done
  n="$(cat "$SEQ_DIR/phase50-seeds")"
  printf '%s:%s\n' "$n" "$method" >> "$SEQ_DIR/phase50-methods"
  if [ "$MODE" = transport-error ]; then return 7; fi
  if [ "$MODE" = wrong-reason ]; then message=wrong; fi
  if [ "$MODE" = missing-present ]; then code=404; fi
  if [ "$MODE" = error-absent ]; then code=500; printf 0 > "$SEQ_DIR/phase50-present"; fi
  if [ "$n" = 1 ] && { { [ "$MODE" = post-reaped ] && [ "$method" = POST ]; } ||
      { [ "$MODE" = delete-reaped ] && [ "$method" = DELETE ]; }; }; then
    code=404; printf 0 > "$SEQ_DIR/phase50-present"
  fi
  printf '{"error":"%s"}\n%s' "$message" "$code"
}
eval "$(awk '/^fixture_worker_absent\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
eval "$(awk '/^remove_fixture_ephemeral_worker\(\) \{/,/^\}/' "$ROOT/e2e/lib.sh")"
# shellcheck disable=SC1090  # execute only the real ephemeral fixture block, with mocked transports
source <(awk '/^  # Public CreateWorker has no ephemeral flag/ {emit=1} /^  M5_HELD=/ {exit} emit {print}' "$ROOT/e2e/phases/50-worker-token-binding.sh")
BASH
}
for mode in success cleanup-reaped before-reaped post-reaped delete-reaped after-post-reaped after-delete-reaped seed-reaped \
    all-reaped missing-present error-absent wrong-reason tuple-change snapshot-present snapshot-error transport-error cleanup-present; do
  cases=$((cases + 1))
  case "$mode" in
    success|cleanup-reaped) want=pass; seeds=1;;
    before-reaped|post-reaped|delete-reaped|after-post-reaped|after-delete-reaped|seed-reaped) want=pass; seeds=2;;
    all-reaped) want=fail; seeds=3;;
    *) want=fail; seeds=1;;
  esac
  if output="$(phase50_ephemeral_case "$mode" 2>&1)"; then got=pass; else got=fail; fi
  actual_seeds="$(cat "$SEQ_DIR/phase50-seeds")"
  valid=true
  [ "$got" = "$want" ] && [ "$actual_seeds" = "$seeds" ] || valid=false
  if [ "$got" = pass ]; then
    methods="$(tail -2 "$SEQ_DIR/phase50-methods")"
    [ "$methods" = "${seeds}:POST"$'\n'"${seeds}:DELETE" ] || valid=false
  fi
  if [ "$mode" = all-reaped ]; then [[ "$output" == *'ephemeral fixture reaped on every attempt'* ]] || valid=false; fi
  if [ "$valid" = true ]; then
    passed=$((passed + 1)); echo "PASS: phase50 ephemeral window $mode"
  else echo "FAIL: phase50 ephemeral window $mode result=$got seeds=$actual_seeds expected=$want/$seeds: $output"; fi
done

# --- capture_agent_logs (#4060): snapshot agent logs before a lifecycle op discards them ------------
# Real helper extracted from lib.sh; stateful fake compose; every case in a subshell with its own
# RUNROOT/COMPOSE so nothing global changes for later code.
CAL_DIR="$(mktemp -d "$SEQ_DIR/cal-fixture.XXXXXX")" # removed with SEQ_DIR by the file's EXIT trap

# State files under $CAL_STATE: current.log (live log), mode (ok|logs-fail|logs-empty|ps-fail|ps-empty),
# recorded (lifecycle observations), bad-argv (any unexpected invocation).
fake_lifecycle_compose() {
  local mode
  mode="$(cat "$CAL_STATE/mode")"
  case "$*" in
    "ps -a -q agent")
      case "$mode" in ps-fail) return 3 ;; ps-empty) return 0 ;; *) echo "container-id-1" ;; esac ;;
    "logs --no-color agent")
      case "$mode" in logs-fail) return 4 ;; logs-empty) return 0 ;; *) cat "$CAL_STATE/current.log" ;; esac ;;
    *"--force-recreate agent" | "kill agent" | "stop agent" | "restart agent" | "down")
      if grep -qs outgoing-process "$RUNROOT"/artifacts/agent-logs/*.log 2>/dev/null; then
        echo "snapshot-present=yes" >> "$CAL_STATE/recorded"
      else
        echo "snapshot-present=no" >> "$CAL_STATE/recorded"
      fi
      echo replacement > "$CAL_STATE/current.log" ;;
    *) echo "unexpected argv: $*" >> "$CAL_STATE/bad-argv"; return 9 ;;
  esac
}

# cal_setup MODE PAYLOAD — fresh RUNROOT + state; sets CAL_STATE, RUNROOT, COMPOSE in the caller.
cal_setup() {
  CAL_STATE="$(mktemp -d "$CAL_DIR/state.XXXXXX")"
  RUNROOT="$(mktemp -d "$CAL_DIR/run.XXXXXX")"
  echo "$1" > "$CAL_STATE/mode"
  echo "$2" > "$CAL_STATE/current.log"
  : > "$CAL_STATE/bad-argv"
  COMPOSE=(fake_lifecycle_compose)
}

cal_case() { # NAME DETAIL(empty = pass)
  cases=$((cases + 1))
  if [ -z "$2" ]; then passed=$((passed + 1)); echo "PASS: $1"; else echo "FAIL: $1 — $2"; fi
}

cal_run() { # NAME FUNCTION — run a case body in a subshell, capturing its detail output
  local detail
  if [ "$CAL_DEFINED" != yes ]; then cal_case "$1" "capture_agent_logs not defined in lib.sh"; return 0; fi
  detail="$( ( "$2" ) 2>&1 )" || detail="${detail:-body exited nonzero}"
  cal_case "$1" "$detail"
}

cal_files() { ls "$RUNROOT/artifacts/agent-logs"/*."$1" 2>/dev/null || true; }

# cal_bare_errexit — run a bare `capture_agent_logs` under live errexit in a fresh bash process (a
# subshell inside `||`/`$( )` would silently ignore set -e). Prints a detail line on failure.
cal_bare_errexit() {
  local script="$CAL_STATE/bare.sh" out="$CAL_STATE/bare.out" rc=0
  {
    echo '#!/usr/bin/env bash'
    declare -f capture_agent_logs fake_lifecycle_compose
    echo 'set -euo pipefail'
    printf 'RUNROOT=%q\nCAL_STATE=%q\nCOMPOSE=(fake_lifecycle_compose)\n' "$RUNROOT" "$CAL_STATE"
    echo 'capture_agent_logs 42-x lbl2'
    echo 'echo SENTINEL'
  } > "$script"
  bash "$script" > "$out" 2>&1 || rc=$?
  [ "$rc" = 0 ] || echo "bare errexit run exited $rc"
  grep -qx SENTINEL "$out" || echo "errexit aborted caller (no SENTINEL)"
}

cal_before_discard() {
  cal_setup ok outgoing-process
  local rc=0 err
  err="$(capture_agent_logs 42-x before-recreate 2>&1)" || rc=$?
  "${COMPOSE[@]}" up -d --no-deps --force-recreate agent
  [ "$rc" = 0 ] || { echo "rc=$rc"; return 0; }
  grep -qx 'snapshot-present=yes' "$CAL_STATE/recorded" || { echo "missing outgoing evidence: no snapshot at recreate time"; return 0; }
  grep -q outgoing-process "$(cal_files log | head -1)" || { echo "missing outgoing evidence: .log lacks payload"; return 0; }
  [ "$(cat "$CAL_STATE/current.log")" = replacement ] || echo "live log is not only replacement"
  [ ! -s "$CAL_STATE/bad-argv" ] || echo "unexpected argv: $(cat "$CAL_STATE/bad-argv")"
  case "$err" in *PASS* | *'FAIL '*) echo "message contains PASS/FAIL token: $err" ;; esac
}
cal_run "capture_agent_logs snapshots outgoing logs before force-recreate" cal_before_discard

cal_repeat() {
  cal_setup ok payload-one
  capture_agent_logs 42-x same
  echo payload-two > "$CAL_STATE/current.log"
  capture_agent_logs 42-x same
  local files n1 n2
  files="$(cal_files log)"
  [ "$(printf '%s\n' "$files" | wc -l | tr -d ' ')" -eq 2 ] || { echo "expected 2 distinct logs, got: $files"; return 0; }
  n1="$(basename "$(printf '%s\n' "$files" | sed -n 1p)")"; n2="$(basename "$(printf '%s\n' "$files" | sed -n 2p)")"
  [ "$((10#${n1%%-*}))" -lt "$((10#${n2%%-*}))" ] || echo "NNNN not strictly increasing: $n1 $n2"
  grep -q payload-one "$RUNROOT/artifacts/agent-logs/$n1" || echo "first payload lost"
  grep -q payload-two "$RUNROOT/artifacts/agent-logs/$n2" || echo "second payload lost"
}
cal_run "capture_agent_logs repeat capture keeps distinct increasing snapshots" cal_repeat

# cal_missing_body MODE REASON-SUBSTRING — shared body for cases 3-5
cal_missing_body() {
  cal_setup "$1" some-payload
  local rc=0 err
  err="$(capture_agent_logs 42-x lbl 2>&1 >/dev/null)" || rc=$?
  [ "$rc" = 0 ] || { echo "rc=$rc"; return 0; }
  case "$err" in *"MISSING-EVIDENCE: agent logs 42-x/lbl: "*"$2"*) ;; *) echo "stderr lacks MISSING-EVIDENCE/$2: $err" ;; esac
  case "$err" in *PASS* | *'FAIL '*) echo "message contains PASS/FAIL token" ;; esac
  [ -n "$(cal_files missing)" ] || echo "no .missing file"
  [ -z "$(cal_files log)" ] || echo ".log written on failure"
  [ -z "$(cal_files absent)" ] || echo ".absent written on failure"
  [ -z "$(cal_files log.part)" ] || echo ".part left behind"
  cal_bare_errexit
}
cal_logs_fail() { cal_missing_body logs-fail "read failure: compose logs"; }
cal_logs_empty() { cal_missing_body logs-empty "empty log output"; }
cal_ps_fail() { cal_missing_body ps-fail "read failure: compose ps"; }
cal_run "capture_agent_logs logs read failure is reported as missing evidence" cal_logs_fail
cal_run "capture_agent_logs empty logs output is reported as missing evidence" cal_logs_empty
cal_run "capture_agent_logs ps failure is a read failure, not absent" cal_ps_fail

cal_write_fail() {
  cal_setup ok some-payload
  mkdir -p "$RUNROOT/artifacts"
  : > "$RUNROOT/artifacts/agent-logs"   # a regular file where the directory must be
  local rc=0 err
  err="$(capture_agent_logs 42-x lbl 2>&1 >/dev/null)" || rc=$?
  [ "$rc" = 0 ] || { echo "rc=$rc"; return 0; }
  case "$err" in *"MISSING-EVIDENCE: agent logs 42-x/lbl: "*) ;; *) echo "stderr lacks MISSING-EVIDENCE: $err" ;; esac
  case "$err" in *PASS* | *'FAIL '*) echo "message contains PASS/FAIL token" ;; esac
  [ -z "$(find "$RUNROOT" -name '*.log' 2>/dev/null)" ] || echo ".log written despite write failure"
  cal_bare_errexit
}
cal_run "capture_agent_logs write failure is reported and returns 0" cal_write_fail

cal_absent() {
  cal_setup ps-empty x
  local err rc=0
  err="$(capture_agent_logs 05-x cleanup absent-ok 2>&1)" || rc=$?
  [ "$rc" = 0 ] || { echo "rc=$rc"; return 0; }
  [ -n "$(cal_files absent)" ] || echo "absent-ok: no .absent"
  [ -z "$(cal_files log)" ] || echo "absent-ok: .log written"
  [ -z "$(cal_files missing)" ] || echo "absent-ok: .missing written"
  case "$err" in *MISSING-EVIDENCE*) echo "absent-ok printed MISSING-EVIDENCE: $err" ;; esac
  case "$err" in *PASS* | *'FAIL '*) echo "message contains PASS/FAIL token" ;; esac
  err="$(capture_agent_logs 05-x strict 2>&1)" || true
  [ -n "$(cal_files missing)" ] || echo "without absent-ok: no .missing"
  case "$err" in *"MISSING-EVIDENCE: agent logs 05-x/strict: agent container absent"*) ;; *) echo "without absent-ok: stderr lacks MISSING-EVIDENCE: $err" ;; esac
}
cal_run "capture_agent_logs absent agent is expected only with absent-ok" cal_absent

# A failed .absent marker write (after slot reservation) is missing evidence, not expected absence.
cal_absent_write_fail() {
  cal_setup ps-empty x
  mkdir -p "$RUNROOT/artifacts/agent-logs/0001-05-x-cleanup.absent"   # a directory where the marker goes
  local err rc=0
  err="$(capture_agent_logs 05-x cleanup absent-ok 2>&1)" || rc=$?
  [ "$rc" = 0 ] || { echo "rc=$rc"; return 0; }
  case "$err" in *"MISSING-EVIDENCE: agent logs 05-x/cleanup: write failure (absent marker)"*) ;; *) echo "stderr lacks MISSING-EVIDENCE write failure: $err" ;; esac
  case "$err" in *"absent (expected); no snapshot"*) echo "reported expected absence despite write failure: $err" ;; esac
  [ -n "$(cal_files missing)" ] || echo "no .missing written"
  [ -z "$(cal_files log)" ] || echo ".log written"
}
cal_run "capture_agent_logs failed .absent marker write is missing evidence" cal_absent_write_fail

cal_bare_ok() {
  cal_setup ok some-payload
  cal_bare_errexit
  # The child really took the success path: a snapshot exists and nothing was reported missing.
  ls "$RUNROOT"/artifacts/agent-logs/*-42-x-lbl2.log >/dev/null 2>&1 || echo "bare run wrote no 42-x-lbl2 snapshot"
  if grep -q MISSING-EVIDENCE "$CAL_STATE/bare.out"; then echo "bare run reported MISSING-EVIDENCE: $(cat "$CAL_STATE/bare.out")"; fi
}
cal_run "capture_agent_logs success path is safe as a bare statement under errexit" cal_bare_ok

cal_slot_parse() {
  cal_setup ok slot-payload
  local d="$RUNROOT/artifacts/agent-logs" got
  mkdir -p "$d"
  : > "$d/.slot-0009"
  : > "$d/.slot-x"
  capture_agent_logs 42-x lbl
  got="$(basename "$(cal_files log)")"
  [ "$got" = "0010-42-x-lbl.log" ] || echo "expected 0010-42-x-lbl.log, got: $got"
}
cal_run "capture_agent_logs numbers past .slot-0009 in decimal and skips non-numeric slots" cal_slot_parse

# --- call-site guard (#4060 case 8): every agent lifecycle command is preceded by a capture --------
# Static scan of the real phases and lib.sh. awk (not grep) so backslash continuations join into one
# logical command and the "previous command" is the nearest non-blank, non-comment logical line.
# Emits HIT rows (file:line, ok|BAD, context before the capture, command) and ABSENT rows (the
# capture line carrying absent-ok, plus the command that follows it).
cal_scan() {
  awk '
    function emit_absent(next_line) {
      if (pend != "") { print "ABSENT\t" pend "\t" next_line; pend = "" }
    }
    function proc(t, ln,    idx, rest, n, toks, i, verb, hasagent, isCap, marker) {
      marker = "\"${COMPOSE[@]}\""
      if (t ~ /^[[:space:]]*$/ || t ~ /^[[:space:]]*#/) return
      if (t ~ /^capture_agent_logs\(\) \{/) inhelper = 1
      if (inhelper) { if (t ~ /^\}/) inhelper = 0; return }
      emit_absent(t)
      isCap = (t ~ /^[[:space:]]*capture_agent_logs[[:space:]]/)
      if (isCap && t ~ /absent-ok/) pend = FILENAME ":" ln
      idx = index(t, marker)
      if (idx > 0) {
        rest = substr(t, idx + length(marker))
        sub(/[>|&;].*/, "", rest)
        n = split(rest, toks, /[[:space:]]+/)
        verb = ""; hasagent = 0
        for (i = 1; i <= n; i++) {
          if (toks[i] == "") continue
          if (verb == "") verb = toks[i]; else if (toks[i] == "agent") hasagent = 1
        }
        if (verb == "down" || (hasagent && verb ~ /^(kill|stop|restart|start|rm|up)$/)) {
          gsub(/^[[:space:]]+/, "", t)
          print "HIT\t" FILENAME ":" ln "\t" (prev ~ /^[[:space:]]*capture_agent_logs[[:space:]]/ ? "ok" : "BAD") "\t" ctx "\t" t
        }
      }
      prev = t
      if (!isCap) ctx = t
    }
    FNR == 1 { emit_absent(""); prev = ""; ctx = ""; acc = ""; inhelper = 0 }
    {
      line = $0
      if (acc == "") accln = FNR
      if (line ~ /\\$/) { acc = acc substr(line, 1, length(line) - 1) " "; next }
      proc(acc line, accln); acc = ""
    }
    END { emit_absent("") }
  ' "$ROOT"/e2e/phases/*.sh "$LIB"
}
CAL_SCAN_OUT="$(cal_scan)"

cal_guard_offenders() {
  local bad
  bad="$(printf '%s\n' "$CAL_SCAN_OUT" | awk -F'\t' '$1 == "HIT" && $3 == "BAD" { print "  " $2 ": " $5 }')"
  [ -z "$bad" ] || printf 'agent lifecycle command without a preceding capture_agent_logs:\n%s\n' "$bad"
}
cal_run "every agent kill/stop/restart/start/rm/up/down in phases and lib.sh is preceded by capture_agent_logs" cal_guard_offenders

cal_guard_nonvacuous() {
  local hits
  hits="$(printf '%s\n' "$CAL_SCAN_OUT" | awk -F'\t' '$1 == "HIT"')"
  [ "$(printf '%s\n' "$hits" | grep -c .)" -ge 23 ] || echo "scan found only $(printf '%s\n' "$hits" | grep -c .) lifecycle commands, want >= 23"
  has() { printf '%s\n' "$hits" | awk -F'\t' -v f="$1" -v c="$2" -v t="$3" 'index($2, f) > 0 && $4 ~ c && index($5, t) > 0 { found = 1 } END { exit !found }'; }
  has phases/42-api-outage-readoption.sh '^export UZI_E2E_MAX_CONCURRENT_RUNS=2' '--force-recreate agent' || echo "scan missed phase 42 first recreate"
  has phases/42-api-outage-readoption.sh '^RA_RESTORE_AT=' '--force-recreate agent' || echo "scan missed phase 42 RESTORE recreate"
  has phases/70-handoff.sh '' 'if ! "${COMPOSE[@]}" up' || echo "scan missed phase 70 EXIT-trap recreate"
  has phases/52-api-outage-outbox.sh '' 'stop agent >/dev/null || return 1' || echo "scan missed phase 52 stop_outbox_agent stop"
  has phases/52-api-outage-outbox.sh '' 'agent recreation failed' || echo "scan missed phase 52 recreate_outbox_agent up"
  has e2e/lib.sh '' 'down -v --remove-orphans' || echo "scan missed lib.sh teardown down -v"
}
cal_run "lifecycle call-site scan is non-vacuous (finds the incident, restore, trap, helper and teardown sites)" cal_guard_nonvacuous

cal_guard_absent_ok() {
  local rows n
  rows="$(printf '%s\n' "$CAL_SCAN_OUT" | awk -F'\t' '$1 == "ABSENT"')"
  n="$(printf '%s\n' "$rows" | grep -c . || true)"
  [ "$n" = 4 ] || echo "absent-ok used at $n sites, want exactly 4 (05, 13, 15 post-down up, lib.sh teardown): $rows"
  printf '%s\n' "$rows" | awk -F'\t' '
    $2 ~ /phases\/05-lane-forgejo\.sh:/ && $3 ~ /up -d --wait agent/ { a = 1 }
    $2 ~ /phases\/13-worker-join-online\.sh:/ && $3 ~ /up -d --wait agent/ { b = 1 }
    $2 ~ /phases\/15-happy-path-restart\.sh:/ && $3 ~ /up -d --wait agent/ { c = 1 }
    $2 ~ /e2e\/lib\.sh:/ && $3 ~ /down -v/ { d = 1 }
    END { if (!a) print "absent-ok missing at 05 initial up"
          if (!b) print "absent-ok missing at 13 initial up"
          if (!c) print "absent-ok missing at 15 post-down up"
          if (!d) print "absent-ok missing at lib.sh teardown down" }'
}
cal_run "absent-ok appears only at the initial starts, the post-down restart and teardown" cal_guard_absent_ok

# Phase 42 (#4060 case 9): run the REAL block from the cap-2 export through its first force-recreate
# against the lifecycle fake. The fake records whether a snapshot holding the outgoing log existed
# when the recreate ran, so deleting the wired capture line turns this red on behaviour.
cal_phase42_first_recreate() {
  cal_setup ok outgoing-process
  local block
  block="$(awk '/^export UZI_E2E_MAX_CONCURRENT_RUNS=2/ {a=1} a {print} a && /--force-recreate agent/ {exit}' "$ROOT/e2e/phases/42-api-outage-readoption.sh")"
  [ -n "$block" ] || { echo "could not extract the phase 42 first-recreate block"; return 0; }
  case "$block" in *'--force-recreate agent'*) ;; *) echo "extracted block lacks the recreate"; return 0 ;; esac
  eval "$block"
  grep -qx 'snapshot-present=yes' "$CAL_STATE/recorded" 2>/dev/null || { echo "phase 42 recreated the agent with no outgoing-log snapshot on disk"; return 0; }
  grep -q outgoing-process "$(cal_files log | head -1)" 2>/dev/null || echo "phase 42 snapshot lacks outgoing-process"
  [ ! -s "$CAL_STATE/bad-argv" ] || echo "unexpected argv: $(cat "$CAL_STATE/bad-argv")"
}
cal_run "phase 42 snapshots the outgoing agent log before its first force-recreate" cal_phase42_first_recreate

# --- #3644 Docker wiring posture lifecycle ---------------------------------------------------------
# The agent resolves its Docker wiring ONCE, at (re)create, from a socket-reachability probe; the env line
# only marks the sidecar as expected (a longer ready budget). The harness below models exactly that: a
# snapshot of "is the socket reachable" taken ONLY when the agent container is created, with dind stopped
# two ways (clean: the socket is removed; stale: the socket file lingers but the daemon is gone, so a
# connect fails). The corrected-order rows run the SHIPPED docker_wire_prepare / docker_wire_assert /
# docker_restore_posture extracted from lib.sh against the stubs; only the old-sequence negative row is
# modelled inline. The lib.sh path can be overridden (DOCKER_WIRE_TEST_LIB) so a scratch copy with the old
# order proves these rows go red; production always runs the shipped file.
DW_LIB="${DOCKER_WIRE_TEST_LIB:-$LIB}"
cat > "$SEQ_DIR/docker-wire-harness.sh" <<'DWHARNESS'
#!/usr/bin/env bash
set -euo pipefail
LIB="$1" S="$2" SCENARIO="$3"
mkdir -p "$S"
ENVFILE="$S/e2e.env"
RUNROOT="$S"
LINE='UZI_DIND_SOCKET=/run/dind/docker.sock'
SOCK_MODE="${SOCK_MODE:-clean}"
COMPOSE=(fake_compose)
DOCKER_WIRE_TIMEOUT=1

reset_state() {
  find "$S" -mindepth 1 -delete
  : > "$S/calls"; : > "$S/rawcalls"; : > "$S/forbidden"; : > "$S/recreates"
  printf 'FOO=1\n' > "$ENVFILE"
}
sleep() { command sleep 0.05; }
say() { :; }
pass() { echo "pass: $*"; }
fail() { echo "fail: $*" >&2; exit 17; }
wait_worker_online() { echo wait_worker_online >> "$S/calls"; }
capture_agent_logs() { echo "capture $*" >> "$S/calls"; }
docker() { [ "$1" = logs ] || return 1; cat "$S/log.$2"; }
apiget() {
  case "$1" in
    /api/workers) jq -nc --argjson caps "$(cat "$S/api.caps" 2>/dev/null || echo '[]')" '{workers: [{id: "w-1", status: "online", capabilities: $caps}]}' ;;
    *) return 1 ;;
  esac
}
# The resolver model: reachability is read ONCE, here, when the container is created.
agent_create() {
  local n id reach=0 caps='[]' wired=false
  n=$(( $(cat "$S/agent.n" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$S/agent.n"
  id="$(printf 'a%011d' "$n")"
  echo "$id" > "$S/agent.id"
  if { [ -e "$S/sock" ] && [ -e "$S/daemon" ]; } || [ -n "${STUCK_WIRED:-}" ]; then reach=1; wired=true; caps='["docker"]'; fi
  printf 'recreate %s wired=%s envline=%s dind=%s\n' "$id" "$reach" \
    "$(if grep -qxF "$LINE" "$ENVFILE"; then echo present; else echo absent; fi)" \
    "$([ -f "$S/running.dind" ] && echo up || echo down)" >> "$S/recreates"
  { echo "{\"msg\":\"docker wiring resolved\",\"docker_wired\":$wired}"
    echo "{\"msg\":\"registered\",\"worker_id\":\"w-1\",\"capabilities\":$caps}"; } > "$S/log.$id"
  [ -n "${STALE_API:-}" ] || echo "$caps" > "$S/api.caps"
}
fake_compose() {
  local t svc
  printf '%s\n' "$*" >> "$S/rawcalls"
  while [ "${1:-}" = --profile ]; do shift 2; done
  printf '%s\n' "$*" >> "$S/calls"
  for t in "$@"; do case "$t" in -v | down | volume) echo "$*" >> "$S/forbidden" ;; esac; done
  case "$1" in
    ps)
      [ -z "${PS_FAIL:-}" ] || return 1
      svc="${!#}"
      case "$svc" in
        agent) [ ! -f "$S/agent.id" ] || cat "$S/agent.id" ;;
        dind | dind-init) [ ! -f "$S/running.$svc" ] || echo "id-$svc" ;;
      esac ;;
    up)
      case "${!#}" in
        dind) [ -z "${DIND_UP_FAIL:-}" ] || { echo "${DIND_UP_MSG:-}"; return 1; }
          : > "$S/running.dind"; : > "$S/running.dind-init"; : > "$S/daemon"; : > "$S/sock" ;;
        agent) agent_create ;;
        *) echo "unexpected up: $*" >> "$S/forbidden" ;;
      esac ;;
    stop)
      shift
      for svc in "$@"; do
        rm -f "$S/running.$svc"
        if [ "$svc" = dind ]; then rm -f "$S/daemon"; [ "$SOCK_MODE" != clean ] || rm -f "$S/sock"; fi
      done ;;
    rm)
      shift
      for svc in "$@"; do
        [ "$svc" = -f ] && continue
        [ ! -f "$S/running.$svc" ] || return 1
        echo "removed $svc" >> "$S/removed"
      done ;;
    logs) cat "$S/agentlog.txt" 2>/dev/null || true ;;
    *) echo "unexpected: $*" >> "$S/forbidden" ;;
  esac
}
fn() { eval "$(awk -v n="$1" '$0 ~ ("^" n "\\(\\) \\{") { p = 1 } p { print } p && /^\}/ { exit }' "$LIB")"; }
eval "$(awk '/^DOCKER_WIRE_LINE=/ { p = 1 } p { print } p && /^docker_restore_posture\(\) \{/ { q = 1 } q && /^\}/ { exit }' "$LIB")"
for f in restore_agent_container hold_open_count assert_hold_count_le log_hold_census log_run_hold_census assert_no_open_run_holds \
  _restore_journal_value restore_check_p_shape restore_assert_canonical_refusal; do fn "$f"; done
for f in docker_wire_prepare docker_wire_assert docker_restore_posture docker_wire_exit_restore restore_agent_container; do
  type "$f" >/dev/null 2>&1 || { echo "fail: $f not extracted from $LIB" >&2; exit 2; }
done

line_of() { grep -n -- "$1" "$S/calls" | "$2" -n 1 | cut -d: -f1; }
expect_failure() {
  local want="$1" out; shift
  if out="$( ( "$@" ) 2>&1 )"; then fail "expected a failure containing [$want], got success: $out"; fi
  case "$out" in *"$want"*) ;; *) fail "failure lacks [$want]: $out" ;; esac
}
start_wired_entry() {
  : > "$S/running.dind"; : > "$S/running.dind-init"; : > "$S/daemon"; : > "$S/sock"
  printf '%s\n' "$LINE" >> "$ENVFILE"
  fake_compose up -d --no-deps --force-recreate agent
  : > "$S/calls"; : > "$S/rawcalls"
}

reset_state
case "$SCENARIO" in
  shipped)
    # Entry unwired. prepare wires; the phase's own recreate picks it up; restore returns the posture.
    docker_wire_prepare
    [ "$DOCKER_WIRE_OWNED" = 1 ] || fail "prepare did not record ownership"
    grep -qxF "$LINE" "$ENVFILE" || fail "prepare did not append the env line"
    fake_compose up -d --no-deps --force-recreate agent
    docker_wire_assert 2
    docker_restore_posture
    last="$(tail -n 1 "$S/recreates")"
    case "$last" in *"wired=0 envline=absent dind=down") ;; *) fail "final recreate was not an unwired one with the line gone and dind down: $last" ;; esac
    s="$(line_of '^stop dind dind-init$' head)"; r="$(line_of '^rm -f dind dind-init$' head)"; u="$(line_of 'force-recreate agent' tail)"
    { [ -n "$s" ] && [ -n "$r" ] && [ -n "$u" ] && [ "$s" -lt "$r" ] && [ "$r" -lt "$u" ]; } || fail "call order is not stop < rm < agent recreate (stop=$s rm=$r recreate=$u)"
    [ "$(grep -c 'force-recreate agent' "$S/calls")" = 2 ] || fail "the agent must be recreated exactly once by the restore"
    ! grep -qxF "$LINE" "$ENVFILE" || fail "the phase-owned env line was not removed"
    grep -qx 'FOO=1' "$ENVFILE" || fail "unrelated env lines must survive"
    [ ! -s "$S/forbidden" ] || fail "forbidden call: $(cat "$S/forbidden")"
    [ "$DOCKER_WIRE_OWNED" = 0 ] || fail "ownership not cleared after the restore"
    echo "OK ends UNWIRED ($SOCK_MODE socket removal): stop<rm<recreate, line gone, dind down at the recreate" ;;
  already-wired)
    start_wired_entry
    before="$(cat "$S/agent.id")"
    docker_wire_prepare
    [ "${DOCKER_WIRE_OWNED:-}" = 0 ] || fail "an already wired entry must not be owned"
    docker_restore_posture
    ! grep -E '^(stop|rm|up)' "$S/calls" || fail "an already wired entry must not stop, remove or start anything"
    [ "$(cat "$S/agent.id")" = "$before" ] || fail "an already wired entry must not recreate the agent"
    grep -qxF "$LINE" "$ENVFILE" && [ -f "$S/running.dind" ] || fail "an already wired entry must keep its line and sidecar"
    echo "OK entry posture already wired: nothing stopped, removed or recreated" ;;
  old-sequence)
    # Inline model of the OLD sequence only: remove the env line, recreate while dind still runs, stop dind,
    # recreate only if the socket lingered.
    docker_wire_prepare
    fake_compose up -d --no-deps --force-recreate agent
    { grep -vxF "$LINE" "$ENVFILE" > "$ENVFILE.t" || [ "$?" = 1 ]; }; cat "$ENVFILE.t" > "$ENVFILE"
    fake_compose up -d --no-deps --force-recreate agent
    fake_compose stop dind dind-init
    if [ -e "$S/sock" ]; then fake_compose up -d --no-deps --force-recreate agent; fi
    fake_compose rm -f dind dind-init
    last="$(tail -n 1 "$S/recreates")"
    if [ "$SOCK_MODE" = clean ]; then
      case "$last" in *"wired=1 "*) echo "OK old sequence + clean socket removal ends WIRED (it never recreated): the lifecycle test distinguishes the sequences" ;; *) fail "old sequence unexpectedly unwired on clean removal: $last" ;; esac
    else
      case "$last" in *"wired=0 "*) echo "OK old sequence + stale socket ends UNWIRED only because the lingering file triggered the recreate" ;; *) fail "old sequence unexpectedly wired on a stale socket: $last" ;; esac
    fi ;;
  prepare-idempotent)
    docker_wire_prepare
    docker_wire_prepare
    [ "$DOCKER_WIRE_OWNED" = 1 ] && [ "$DOCKER_WIRE_STARTED" = "dind-init dind" ] || fail "second prepare clobbered the recorded ownership ($DOCKER_WIRE_OWNED / $DOCKER_WIRE_STARTED)"
    [ "$(grep -c '^up -d --wait dind$' "$S/calls")" = 1 ] || fail "the sidecar must be started once"
    [ "$(grep -cxF "$LINE" "$ENVFILE")" = 1 ] || fail "the env line must appear once"
    echo "OK second prepare is a no-op; ownership and single start/line kept" ;;
  restore-idempotent)
    docker_wire_prepare
    fake_compose up -d --no-deps --force-recreate agent
    docker_restore_posture
    : > "$S/calls"
    before="$(cat "$S/agent.id")"
    docker_restore_posture
    [ ! -s "$S/calls" ] || fail "a second restore after a successful one must issue no compose call: $(cat "$S/calls")"
    [ "$(cat "$S/agent.id")" = "$before" ] || fail "a second restore must not recreate the agent"
    docker_wire_exit_restore 0
    [ ! -s "$S/calls" ] || fail "the exit restore after a successful restore must be a no-op: $(cat "$S/calls")"
    echo "OK restore is idempotent: a second restore and the exit restore after it do nothing" ;;
  exit-restore)
    # A phase that fails between prepare and its restore: the exit hook restores the posture and keeps the status.
    docker_wire_prepare
    fake_compose up -d --no-deps --force-recreate agent
    rc=0; docker_wire_exit_restore 3 2>/dev/null || rc=$?
    [ "$rc" = 3 ] || fail "the exit restore must preserve the phase status 3, got $rc"
    last="$(tail -n 1 "$S/recreates")"
    case "$last" in *"wired=0 envline=absent dind=down") ;; *) fail "the exit restore did not leave an unwired agent: $last" ;; esac
    ! grep -qxF "$LINE" "$ENVFILE" || fail "the exit restore left the env line"
    # A restore that itself fails is reported loudly and still preserves the original status.
    DOCKER_WIRE_OWNED=0; reset_state; docker_wire_prepare; fake_compose up -d --no-deps --force-recreate agent; STUCK_WIRED=1
    rc=0; out="$(docker_wire_exit_restore 5 2>&1)" || rc=$?
    [ "$rc" = 5 ] || fail "a failing exit restore must preserve the phase status 5, got $rc"
    case "$out" in *LOUD*) ;; *) fail "a failing exit restore must be loud: $out" ;; esac
    echo "OK exit restore: unwires on a failed phase, loud on its own failure, original status preserved" ;;
  prepare-failures)
    ( reset_state; DIND_UP_FAIL=1; expect_failure 'the DinD sidecar did not become healthy' docker_wire_prepare )
    ( reset_state; DIND_UP_FAIL=1; DIND_UP_MSG='boom-from-compose'; expect_failure 'boom-from-compose' docker_wire_prepare )
    ( reset_state; PS_FAIL=1; expect_failure 'cannot read the dind service state at entry' docker_wire_prepare )
    ( reset_state; : > "$S/running.dind"; expect_failure 'inconsistent entry posture' docker_wire_prepare )
    ( reset_state; printf '%s\n' "$LINE" >> "$ENVFILE"; expect_failure 'inconsistent entry posture' docker_wire_prepare )
    echo "OK prepare failures name their cause (unhealthy sidecar, unreadable state, half-wired entry)" ;;
  assert-failures)
    ( reset_state; fake_compose up -d --no-deps --force-recreate agent
      expect_failure 'lacks "docker_wired":true' docker_wire_assert 1 )
    ( reset_state; : > "$S/daemon"; : > "$S/sock"; echo '[]' > "$S/api.caps"; STALE_API=1; fake_compose up -d --no-deps --force-recreate agent
      expect_failure 'API worker row w-1 is [nodocker], want [docker]' docker_wire_assert 1 )
    ( reset_state; : > "$S/daemon"; : > "$S/sock"; fake_compose up -d --no-deps --force-recreate agent
      docker_wire_assert 1 )
    echo "OK assert names its cause: no docker_wired:true in the new container, or no docker capability on the worker row" ;;
  restore-failures)
    ( reset_state; docker_wire_prepare; fake_compose up -d --no-deps --force-recreate agent; STUCK_WIRED=1
      expect_failure 'logged "docker_wired":true although the sidecar must be unwired' docker_restore_posture )
    ( reset_state; docker_wire_prepare; fake_compose up -d --no-deps --force-recreate agent; agent_create() { :; }
      expect_failure 'was not recreated by the unwire step' docker_restore_posture )
    ( reset_state; docker_wire_prepare; fake_compose up -d --no-deps --force-recreate agent; STALE_API=1
      expect_failure 'API worker row w-1 is [docker], want [nodocker]' docker_restore_posture )
    echo "OK restore verifies the FINAL container: still-wired log, no recreate, stale capability row" ;;
  census)
    apiget() { case "$1" in /api/recovery/holds) cat "$S/holds.json" ;; *) return 1 ;; esac; }
    echo '{"aggregate":{"open_holds":3},"holds":[{"run_id":"R1","generation":1,"state":"open","attention":"source_only"},{"run_id":"R1","generation":2,"state":"released","attention":"none"},{"run_id":"R2","generation":1,"state":"discarded","attention":"none"}]}' > "$S/holds.json"
    [ "$(hold_open_count)" = 3 ] || fail "hold_open_count"
    assert_hold_count_le x 3 >/dev/null
    expect_failure 'exceed the entry baseline 2' assert_hold_count_le x 2
    expect_failure 'invalid baseline' assert_hold_count_le x abc
    out="$(log_run_hold_census lbl R1 R3)"
    case "$out" in *'run R1: (generation=1 state=open attention=source_only) (generation=2 state=released attention=none)'*'run R3: none'*) ;; *) fail "run attribution: $out" ;; esac
    assert_no_open_run_holds x R2 R3 >/dev/null
    expect_failure 'run R1 still has open custody holds beyond the declared discards: generation=1' assert_no_open_run_holds x R1 R2
    echo '{"aggregate":{},"holds":[]}' > "$S/holds.json"
    if hold_open_count >/dev/null 2>&1; then fail "a missing aggregate must not read as a count"; fi
    echo 'garbage' > "$S/holds.json"
    out="$(log_run_hold_census lbl R1)"; case "$out" in *unreadable*) ;; *) fail "unreadable holds must be reported: $out" ;; esac
    echo "OK census helpers: count, baseline gate, run attribution, open-hold gate, unreadable inputs" ;;
  p-shape)
    ROWS=''
    restore_witness_exec() { printf '%s\n' "$ROWS"; }
    row() { ROWS="$(printf '/data/repos/x\tuzi-recovery.agent/issue-1.clone\t%s' "$1")"; }
    shape() { restore_check_p_shape R agent/issue-1 "$1" 2>&1; }
    row '{"runId":"R","clonePath":"/d/issue-1.attempt-abc","attemptId":"abc"}'
    shape attempt >/dev/null || fail "an attempt-shaped journal must satisfy attempt"
    out="$(shape canonical)" && fail "an attempt-shaped journal must not satisfy canonical"
    case "$out" in *'not the canonical {runId, clonePath} shape'*) ;; *) fail "canonical refusal message: $out" ;; esac
    row '{"runId":"R","clonePath":"/d/issue-1"}'
    shape canonical >/dev/null || fail "{runId, clonePath} must satisfy canonical"
    out="$(shape attempt)" && fail "a canonical journal must not satisfy attempt"
    case "$out" in *'is not attempt-shaped'*) ;; *) fail "attempt refusal message: $out" ;; esac
    row '{"runId":"R","clonePath":"/d/issue-1","attemptId":"abc"}'
    shape canonical >/dev/null && fail "an extra journal key must not satisfy canonical"
    row '{"runId":"R","clonePath":"/d/issue-1.attempt-abc"}'
    shape canonical >/dev/null && fail "a .attempt- path must not satisfy canonical"
    shape attempt >/dev/null && fail "an attemptId-less journal must not satisfy attempt"
    row '{"runId":"OTHER","clonePath":"/d/issue-1"}'
    out="$(shape canonical)" && fail "another run's journal must not count"
    case "$out" in *'expected exactly one predecessor journal'*) ;; *) fail "missing journal message: $out" ;; esac
    out="$(shape bogus)" && fail "an unknown mode must fail"
    echo "OK predecessor shape: attempt vs exactly {runId, clonePath} at a canonical path" ;;
  refusal)
    db_psql() { cat "$S/dbrow"; }
    apiget() { case "$1" in /api/runs/R/messages) cat "$S/feed.json" ;; *) return 1 ;; esac; }
    good_log() {
      echo '{"msg":"run claimed","run_id":"R"}'; echo '{"msg":"run claimed","run_id":"R"}'
      echo 'noise before json {"msg":"independent restore refused; predecessor retained","unsupported":"canonical","run_id":"R"}'
      echo '{"msg":"restore unsupported predecessor held","run_id":"R"}'
    }
    try() { # LOG_CMD DBROW FEED — fresh evidence, one bounded observation
      "$1" > "$S/agentlog.txt"; printf '%s\n' "$2" > "$S/dbrow"; printf '%s\n' "$3" > "$S/feed.json"
      restore_assert_canonical_refusal R 2 1
    }
    GOOD_ROW='recovery_wait|retained_recovery_adoption_failed||2'
    GOOD_FEED='{"messages":[{"kind":"status","payload":{"text":"this release does not support this predecessor; it is held"}}]}'
    ( try good_log "$GOOD_ROW" "$GOOD_FEED" >/dev/null )
    unconfirmed_log() { good_log | sed 's/restore unsupported predecessor held/restore unsupported predecessor hold unconfirmed/'; }
    ( try unconfirmed_log "$GOOD_ROW" "$GOOD_FEED" >/dev/null )
    seeded_log() { good_log; echo '{"msg":"independent restore seeded","run_id":"R"}'; }
    ( expect_failure 'independently restored' try seeded_log "$GOOD_ROW" "$GOOD_FEED" )
    third_claim_log() { good_log; echo '{"msg":"run claimed","run_id":"R"}'; }
    ( expect_failure 'claimed 3 times' try third_claim_log "$GOOD_ROW" "$GOOD_FEED" )
    attempt_log() { good_log | sed 's/"canonical"/"attempt"/'; }
    ( expect_failure 'missing: refusal-log' try attempt_log "$GOOD_ROW" "$GOOD_FEED" )
    no_held_log() { good_log | grep -v 'predecessor held'; }
    ( expect_failure 'missing: held-log' try no_held_log "$GOOD_ROW" "$GOOD_FEED" )
    ( expect_failure 'feed-text' try good_log "$GOOD_ROW" '{"messages":[]}' )
    ( expect_failure 'run-state([recovery_wait|worker_requeue_exhausted||2])' try good_log 'recovery_wait|worker_requeue_exhausted||2' "$GOOD_FEED" )
    ( expect_failure 'run-state([recovery_wait|retained_recovery_adoption_failed|2026-01-01 00:00:00+00|2])' try good_log 'recovery_wait|retained_recovery_adoption_failed|2026-01-01 00:00:00+00|2' "$GOOD_FEED" )
    ( expect_failure 'run-state([recovery_wait|retained_recovery_adoption_failed||3])' try good_log 'recovery_wait|retained_recovery_adoption_failed||3' "$GOOD_FEED" )
    echo "OK canonical refusal: exactly one successor claim, no seed, frozen log + feed text, recovery_wait/adoption-failed with no retry stamp" ;;
  *) echo "fail: unknown scenario $SCENARIO" >&2; exit 2 ;;
esac
DWHARNESS

# dw_case NAME SCENARIO [VAR=VALUE...] — one harness scenario in a fresh bash process and state directory.
dw_case() {
  cases=$((cases + 1))
  local name="$1" scenario="$2" out; shift 2
  if out="$(env "$@" bash "$SEQ_DIR/docker-wire-harness.sh" "$DW_LIB" "$SEQ_DIR/dw-$cases" "$scenario" 2>&1)" && [[ "$out" == *$'\nOK '* || "$out" == OK\ * ]]; then
    passed=$((passed + 1)); echo "PASS: $name"
  else echo "FAIL: $name: $out"; fi
}
dw_case "docker wiring: corrected order (SHIPPED docker_restore_posture) ends UNWIRED with a clean socket removal" shipped SOCK_MODE=clean
dw_case "docker wiring: corrected order (SHIPPED docker_restore_posture) ends UNWIRED with a stale socket file" shipped SOCK_MODE=stale
dw_case "docker wiring: NEGATIVE old order (remove line, recreate, stop dind, recreate only if the socket lingered) ends WIRED on clean removal" old-sequence SOCK_MODE=clean
dw_case "docker wiring: old order is only unwired when a stale socket file happens to linger" old-sequence SOCK_MODE=stale
dw_case "docker wiring: an already wired entry posture issues no stop, rm or recreate" already-wired
dw_case "docker wiring: prepare is idempotent and keeps its recorded ownership" prepare-idempotent
dw_case "docker wiring: a second restore (and the exit restore after it) is a no-op" restore-idempotent
dw_case "docker wiring: the exit restore unwires a failed phase, is loud on failure and keeps the status" exit-restore
dw_case "docker wiring: prepare failures name their cause" prepare-failures
dw_case "docker wiring: assert failures name their cause" assert-failures
dw_case "docker wiring: restore verifies the FINAL container is unwired" restore-failures
dw_case "hold census helpers: baseline, run attribution and open-hold gates" census
dw_case "predecessor shape: attempt-shaped vs exactly {runId, clonePath}" p-shape
dw_case "canonical refusal: one successor claim, no seed, frozen log and feed, recovery_wait/adoption-failed" refusal

node --test "$ROOT/e2e/settlement-diagnostic.test.mjs"
node --test "$ROOT/e2e/recovery-record-projection.test.mjs"
echo "cases=$cases passed=$passed"
# Tally guard (the driver.test.sh idiom): a real run has at least 217 cases green (including the
# capture_agent_logs cal_* cases); a zero-case or partially-red run must exit nonzero.
[ "$cases" -ge 217 ] && [ "$cases" -eq "$passed" ]
