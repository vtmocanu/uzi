#!/usr/bin/env bash
# shellcheck disable=SC2034,SC2329  # judge() reads these globals; step() is called by judge()
# Docker-free controls for the verdict parser (proofs-3888/verdict.sh): synthetic logs, no tests run.
# Each case states the verdict judge() must return. Run as the first step of every proof leg.
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091
. "$here/verdict.sh"
work=$(mktemp -d "${RUNNER_TEMP:-/tmp}/parser-test.XXXXXX")
step() { :; }
fails=0

# case <title> <want 0|1> <KIND> <MODE> <rc> <NAMES> <PASS_NAMES> <log text>
case_() {
  local title=$1 want=$2
  KIND=$3 MODE=$4 rc=$5 NAMES=$6 PASS_NAMES=$7 MSG_RE='boom' PASS_COUNT=''
  log=$work/log; printf '%s\n' "$8" >"$log"
  judge >/dev/null 2>&1; local got=$?
  if [ "$got" -eq "$want" ]; then echo "ok   $title (verdict $got)"; else echo "FAIL $title: verdict $got, want $want"; fails=$((fails+1)); fi
}

goexp='=== RUN   TestA
=== RUN   TestA/p
    x_test.go:1: boom
--- FAIL: TestA (0.00s)
    --- FAIL: TestA/p (0.00s)
=== RUN   TestB
--- PASS: TestB (0.00s)
FAIL'
case_ "go: expected failure plus passing sibling is credited" 0 go fail 1 'TestA/p' 'TestB' "$goexp"
case_ "go: failure without the expected text is rejected" 1 go fail 1 'TestA/p@@other' '' "$goexp"
case_ "go: a late unindented panic after the named failure is rejected" 1 go fail 1 'TestA/p' 'TestB' "$goexp
panic: send on closed channel

goroutine 1 [running]:"
case_ "go: a fatal error anywhere is rejected" 1 go fail 1 'TestA/p' '' "$goexp
fatal error: all goroutines are asleep"
case_ "go: a test timeout anywhere is rejected" 1 go fail 2 'TestA/p' '' "panic: test timed out after 30s
$goexp"
case_ "go: a failure for the named test that never ran is rejected" 1 go fail 1 'TestZ' '' "$goexp"
case_ "go: a passing unfixed variant (exit 0) is rejected" 1 go fail 0 'TestA/p' '' "$goexp"

nodeok='TAP version 13
ok 1 - sibling
not ok 2 - target
  ---
  failureType: '"'"'testCodeFailure'"'"'
  error: boom
  ...'
case_ "node: expected failure plus passing sibling is credited" 0 node fail 1 'target' 'sibling' "$nodeok"
case_ "node: a SKIP sibling does not count as still passing" 1 node fail 1 'target' 'sibling' "TAP version 13
ok 1 - sibling # SKIP not today
not ok 2 - target
  ---
  failureType: 'testCodeFailure'
  error: boom
  ..."
case_ "node: a TODO sibling does not count as still passing" 1 node fail 1 'target' 'sibling' "TAP version 13
ok 1 - sibling # TODO later
not ok 2 - target
  ---
  failureType: 'testCodeFailure'
  error: boom
  ..."
case_ "node: a cancellation is not an assertion failure" 1 node fail 1 'target' '' "TAP version 13
not ok 1 - target
  ---
  failureType: 'cancelledByParent'
  error: boom
  ..."
case_ "node candidate: all named tests pass" 0 node pass 0 'one;two' '' "TAP version 13
ok 1 - one
ok 2 - two
# skipped 0"
case_ "node candidate: a SKIP among the named tests is rejected" 1 node pass 0 'one;two' '' "TAP version 13
ok 1 - one
ok 2 - two # SKIP flaky
# skipped 1"
case_ "node candidate: a SKIP directive is rejected even without the summary line" 1 node pass 0 'one;two' '' "TAP version 13
ok 1 - one
ok 2 - two # SKIP flaky"
case_ "go candidate: a passing log is credited" 0 go pass 0 'TestB' '' "--- PASS: TestB (0.00s)
PASS"

rm -rf "$work"
[ "$fails" -eq 0 ] && echo "all parser controls ok" || echo "$fails parser control(s) failed"
exit "$fails"
