#!/usr/bin/env bash
# Behavioral test for gate.sh (#3212): every leg outcome the required gate must accept or reject.
# Prints a mandatory `cases=N passed=N` tally and exits non-zero unless every case passes.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$HERE/gate.sh"

cases=0
passed=0

# run_case <name> <expected rc> <KEY=VALUE>...: runs gate.sh with only the given env.
run_case() {
  local name="$1" want="$2" got=0
  shift 2
  cases=$((cases + 1))
  env -i PATH="$PATH" "$@" bash "$GATE" >/dev/null 2>&1 || got=$?
  if [ "$got" -eq "$want" ]; then
    passed=$((passed + 1))
  else
    echo "FAIL: ${name}: rc=${got}, want ${want}" >&2
  fi
}

PR=(CI_EVENT=pull_request CHANGES_RESULT=success)

run_case "unrelated PR: both legs skipped, both not needed" 0 \
  "${PR[@]}" NEED_AMD64=false NEED_ARM64=false AMD64_RESULT=skipped ARM64_RESULT=skipped
run_case "amd64-only PR: proof passed, arm64 not needed" 0 \
  "${PR[@]}" NEED_AMD64=true NEED_ARM64=false AMD64_RESULT=success ARM64_RESULT=skipped
run_case "package PR: both legs passed" 0 \
  "${PR[@]}" NEED_AMD64=true NEED_ARM64=true AMD64_RESULT=success ARM64_RESULT=success
run_case "package PR: arm64 failed" 1 \
  "${PR[@]}" NEED_AMD64=true NEED_ARM64=true AMD64_RESULT=success ARM64_RESULT=failure
run_case "package PR: a template failed (matrix failure)" 1 \
  "${PR[@]}" NEED_AMD64=true NEED_ARM64=true AMD64_RESULT=failure ARM64_RESULT=success
run_case "cancelled leg" 1 \
  "${PR[@]}" NEED_AMD64=true NEED_ARM64=true AMD64_RESULT=cancelled ARM64_RESULT=success
run_case "needed leg unexpectedly skipped" 1 \
  "${PR[@]}" NEED_AMD64=true NEED_ARM64=true AMD64_RESULT=success ARM64_RESULT=skipped
run_case "skipped leg with no filter output" 1 \
  "${PR[@]}" NEED_AMD64=false AMD64_RESULT=skipped ARM64_RESULT=skipped
run_case "failed filter, legs skipped" 1 \
  CI_EVENT=pull_request CHANGES_RESULT=failure NEED_AMD64=false NEED_ARM64=false AMD64_RESULT=skipped ARM64_RESULT=skipped
run_case "cancelled filter, legs skipped" 1 \
  CI_EVENT=pull_request CHANGES_RESULT=cancelled AMD64_RESULT=skipped ARM64_RESULT=skipped
run_case "failed filter, legs passed" 1 \
  CI_EVENT=pull_request CHANGES_RESULT=failure NEED_AMD64=true NEED_ARM64=true AMD64_RESULT=success ARM64_RESULT=success
run_case "cancelled filter, legs passed" 1 \
  CI_EVENT=pull_request CHANGES_RESULT=cancelled AMD64_RESULT=success ARM64_RESULT=success
run_case "schedule: both legs passed" 0 \
  CI_EVENT=schedule CHANGES_RESULT=skipped AMD64_RESULT=success ARM64_RESULT=success
run_case "schedule: a skipped leg is red" 1 \
  CI_EVENT=schedule CHANGES_RESULT=skipped AMD64_RESULT=success ARM64_RESULT=skipped
run_case "dispatch: a failed leg is red" 1 \
  CI_EVENT=workflow_dispatch CHANGES_RESULT=skipped AMD64_RESULT=failure ARM64_RESULT=success
run_case "no results at all is red" 1

echo "cases=$cases passed=$passed"
[ "$cases" -ge 16 ] && [ "$passed" -eq "$cases" ]
