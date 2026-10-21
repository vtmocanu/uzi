#!/usr/bin/env bash
# Verdict of codex-packaged-proof.yml's `codex-packaged-proof-gate`, the stable REQUIRED context
# (#3212). Evaluated per leg, from the job results GitHub passes in as env:
#
#   CI_EVENT        github.event_name
#   CHANGES_RESULT  needs.changes.result
#   NEED_AMD64      needs.changes.outputs.amd64   ("true"/"false"; unset off pull_request)
#   NEED_ARM64      needs.changes.outputs.arm64
#   AMD64_RESULT    needs.packaged-proof.result   (the base+jvm matrix: failure if any template failed)
#   ARM64_RESULT    needs.arm64-package-exec.result
#
# On a pull_request the `changes` job must have succeeded, or the gate is red whatever the legs
# report. A leg passes on `success`, or on `skipped` only for a pull_request whose `changes` job
# succeeded and marked that leg "false". Every other result (failure, cancelled, an unexplained
# skip) is red. Off pull_request (schedule, dispatch) both legs must succeed.
# Exit 0 pass, 1 fail. hermetic test: gate.test.sh.
set -euo pipefail

leg() {
  local name="$1" result="$2" needed="$3"
  if [ "$result" = "success" ]; then
    echo "gate: ${name} passed"
    return 0
  fi
  if [ "$result" = "skipped" ] && [ "${CI_EVENT:-}" = "pull_request" ] &&
    [ "${CHANGES_RESULT:-}" = "success" ] && [ "$needed" = "false" ]; then
    echo "gate: ${name} not needed on this PR"
    return 0
  fi
  echo "gate: ${name} FAILED (result=${result:-unset} event=${CI_EVENT:-unset} changes=${CHANGES_RESULT:-unset} needed=${needed:-unset})"
  return 1
}

rc=0
# A PR is judged only through a successful filter, whatever the legs report.
if [ "${CI_EVENT:-}" = "pull_request" ] && [ "${CHANGES_RESULT:-}" != "success" ]; then
  echo "gate: changes filter FAILED (result=${CHANGES_RESULT:-unset})"
  rc=1
fi
leg "packaged-proof (amd64)" "${AMD64_RESULT:-}" "${NEED_AMD64:-}" || rc=1
leg "arm64-package-exec" "${ARM64_RESULT:-}" "${NEED_ARM64:-}" || rc=1
if [ "$rc" -eq 0 ]; then echo "gate: pass"; else echo "gate: fail"; fi
exit "$rc"
