# shellcheck shell=bash
# phase:    schedules-prompt
# title:    PRD #966 M4: scheduled prompt run (issue-less repo->MR run via run-now)
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID UZI_BIN UZI_TOKEN_VAL
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #966 M4 — the `prompt` run kind had zero wire coverage. A prompt schedule is an
# issue-LESS target: it fires an ad-hoc repo->MR run from prompt text, not from a forge
# issue. This phase creates a fire-once (`--at`) prompt schedule, fires it immediately
# with run-now, and asserts the resulting run is kind=prompt with no issue_iid and
# reaches completed through the stub with an MR opened.
#
# --at (fire-once) is required because `schedule create` demands exactly one timing
# (--at | --cron); run-now fires the schedule immediately regardless of the timing, so
# the +1h instant never actually fires on its own during the suite. --auto-approve
# defaults to true, so the fired run skips the plan gate and the kind-agnostic stub
# executor drives it to completed with an MR (no runner.ts/executor.ts change needed).
say "PRD #966 M4: scheduled prompt run (issue-less repo->MR run via run-now)"

# Compute an RFC3339 instant one hour out. GNU date first, BSD date as fallback, so the
# phase is portable across the harness's possible hosts.
AT="$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)"
[ -n "$AT" ] || fail "could not compute an RFC3339 --at instant"

SID="$(uzi_cli schedule create --repo "$REPO_ID" --prompt 'e2e prompt run' --at "$AT" --json | jq -r '.id')"
{ [ -n "$SID" ] && [ "$SID" != null ]; } || fail "schedule create --prompt returned no id"
pass "created a fire-once prompt schedule $SID (--at $AT)"

RN="$(uzi_cli schedule run-now "$SID" --json)" || fail "schedule run-now (prompt) failed (exit $?)"
echo "$RN" | jq -e '(.started | length) == 1' >/dev/null \
  || fail "prompt run-now should start exactly one run: $RN"
RUN_P="$(echo "$RN" | jq -r '.started[0].run_id')"
{ [ -n "$RUN_P" ] && [ "$RUN_P" != null ]; } || fail "prompt run-now returned no run_id: $RN"

# The kind is asserted from the wire, through the CLI's own run get --field (a scalar).
KIND="$(uzi_cli run get "$RUN_P" --field kind)" || fail "uzi run get --field kind failed (exit $?)"
[ "$KIND" = prompt ] || fail "scheduled prompt run kind should be 'prompt', got '$KIND'"

# A prompt run is issue-less: issue_iid is null (no forge issue backs it).
RUN_P_JSON="$(apiget "/api/runs/$RUN_P")"
[ "$(echo "$RUN_P_JSON" | jq -r '.run.issue_iid')" = null ] \
  || fail "a prompt run must have a null issue_iid: $(echo "$RUN_P_JSON" | jq -c '.run.issue_iid')"
pass "prompt run $RUN_P is kind=prompt with a null issue_iid"

wait_status "$RUN_P" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
MR_P="$(apiget "/api/runs/$RUN_P" | jq -r '.run.mr_iid')"
{ [ "$MR_P" != null ] && [ "$MR_P" -gt 0 ]; } || fail "prompt run opened no MR (got $MR_P)"
pass "prompt run $RUN_P completed through the stub and opened MR !$MR_P"

uzi_cli schedule delete "$SID" >/dev/null || fail "schedule delete failed (exit $?)"
pass "deleted prompt schedule $SID; run terminal (quarantine-clean)"
