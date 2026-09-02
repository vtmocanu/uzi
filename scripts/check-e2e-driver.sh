#!/usr/bin/env bash
# Run the hermetic e2e driver test and GUARD its tally (PRD #966 M2).
#
# usage: scripts/check-e2e-driver.sh
#
# 🔴 WHY THIS EXISTS -- THE PASS=0 FAMILY. e2e/driver.test.sh is the hermetic
# behavioural test for the phase-registry driver (e2e/driver.sh): fake
# docker/curl/psql shims, no stack, no network. It prints per-case PASS:/FAIL:
# lines and a final `cases=<c> passed=<p>` tally. But a test that crashed before
# its first case, or that was gutted to zero cases, would print no tally (or a
# `cases=0 passed=0`) and STILL exit 0 -- reading green while asserting nothing.
# So a bare `./e2e/driver.test.sh` in the gate is not enough: this wrapper runs
# it, echoes its output, extracts the LAST `cases=<c> passed=<p>` line, and fails
# unless that line EXISTS and c == p AND c >= the floor below.
#
# 🔴 THE FLOOR IS 8. PRD #966 M2 specifies eight named driver behaviours, each
# watched red under a named mutation (SC2, "N >= 8"): (1) errexit-safe subshell,
# (2) provides round-trip, (3) requires-miss names both phases, (4) env: token
# validation, (5) critical-stop + suspect_cascade, (6) quarantine LEAK + DB
# fallback, (7) ONLY/SKIP honour critical, (8) fault injection + artifact shapes.
# The test emits more than 8 cases (several behaviours split into a/b sub-cases,
# and later milestones add more), so it passes with headroom; the floor catches a
# driver test that regressed BELOW its designed coverage, not one that grew.
#
# EXIT CODES:
#     2 = the instrument is broken (driver.test.sh missing, or NO tally line)
#     1 = the test failed (c != p) OR the tally is below the floor (c < 8)
#     0 = clean: driver.test.sh green and c == p >= 8
# `task`'s own rc is 201 for all of them.
set -euo pipefail

FLOOR=8   # the 8 PRD #966 M2 driver cases (see the header). Never lower this.

# Run from the repo root whatever the caller's directory, like the sibling checks.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-e2e-driver: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-e2e-driver: cannot cd to repo root: $ROOT" >&2
  exit 2
}

TEST="./e2e/driver.test.sh"
if [ ! -x "$TEST" ] && [ ! -f "$TEST" ]; then
  echo "check-e2e-driver: driver test not found at $ROOT/$TEST" >&2
  exit 2
fi

# Run the hermetic test, capture its full output, echo it (so the gate log shows
# the per-case lines), and keep its exit status without tripping errexit.
set +e
out="$(bash "$TEST" 2>&1)"
test_rc=$?
set -e
printf '%s\n' "$out"

# Extract the LAST `cases=<c> passed=<p>` line (there is one; taking the last is
# defensive against a case body ever echoing the token).
tally="$(printf '%s\n' "$out" | awk '/^cases=[0-9]+ passed=[0-9]+$/{last=$0} END{print last}')"
if [ -z "$tally" ]; then
  echo "check-e2e-driver: ================================================================" >&2
  echo "check-e2e-driver: INSTRUMENT BROKEN -- driver.test.sh printed no 'cases=N passed=N'" >&2
  echo "check-e2e-driver: tally line (test exit $test_rc). A crashed or gutted test would" >&2
  echo "check-e2e-driver: exit 0 with no tally and read green; that is the PASS=0 hole this" >&2
  echo "check-e2e-driver: check closes. Fix driver.test.sh so it prints its tally." >&2
  echo "check-e2e-driver: ================================================================" >&2
  exit 2
fi

cases="${tally#cases=}"; cases="${cases%% *}"
passed="${tally##*passed=}"

if [ "$test_rc" -ne 0 ] || [ "$cases" -ne "$passed" ]; then
  echo "check-e2e-driver: driver.test.sh FAILED -- $tally (test exit $test_rc)." >&2
  echo "  Some driver case did not pass; read the FAIL: lines above." >&2
  exit 1
fi

if [ "$cases" -lt "$FLOOR" ]; then
  echo "check-e2e-driver: TOO FEW CASES -- $tally, but the floor is $FLOOR." >&2
  echo "  PRD #966 M2 specifies 8 driver behaviours (SC2, N >= 8). A tally below the" >&2
  echo "  floor means driver.test.sh regressed below its designed coverage. Restore the" >&2
  echo "  missing case(s), never lower the floor." >&2
  exit 1
fi

echo "check-e2e-driver: clean -- $tally (>= floor $FLOOR); the driver's 8 M2 behaviours are covered."
exit 0
