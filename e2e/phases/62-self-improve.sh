# shellcheck shell=bash
# phase:    self-improve
# title:    PRD #966 M4: scheduled self_improve run (tracking issue, MR opened)
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID UZI_BIN UZI_TOKEN_VAL
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #966 M4 (stretch) — the `self_improve` run kind had zero wire coverage. Unlike
# prompt/sweep, its gating lives in the SCHEDULER (fireSelfImprove,
# api/internal/schedsvc/self_improve.go), not the executor: it files/reuses a tracking
# issue on the forge (ensureTrackingIssue -> CreateIssue), reads MR open-state
# (GetMergeRequest) to enforce a per-repo open-self-improve-MR cap (selfImproveMaxOpenMRs
# = 2), and only then creates the run. All those forge routes are served by forge-fake,
# and the StubExecutor is kind-agnostic, so a self_improve run reaches completed with an
# MR unchanged. A fresh repo has 0 prior self-improve MRs (cap 0 < 2), so run-now starts.
#
# On the run-now path the vault does NOT gate: the run-now HTTP handler builds its
# scheduler with a NIL vault (api/internal/handler/handler.go NewHandler: schedsvc.New(…,
# nil, nil, 0, …), "treated as always unlocked"), so fireSelfImprove's
# `if e.vault != nil && !e.vault.Unlocked(...)` vault_locked branch is UNREACHABLE via
# run-now. So this phase does NOT unlock the vault — that would be a vacuous control. The
# vault_locked skip is only reachable through the BACKGROUND (non-nil-vault) scheduler and
# belongs to a unit test, not this wire phase. This phase asserts a Started, not a Skip;
# a skip reason (e.g. self_improve_mr_cap_reached) would be the recordable blocker.
#
# NOTE (SC9 / stretch): live behaviour is validated in CI; the full suite cannot run in
# the M4 worker (agent image build fails at devbox). No runner.ts/executor.ts change is
# made or needed — the stub drives self_improve to completed as-is.
say "PRD #966 M4: scheduled self_improve run (tracking issue, MR opened)"

# --- enable the self-improve catalog default ---------------------------------
SID="$(uzi_cli schedule catalog enable self-improve --repo "$REPO_ID" --json | jq -r '.[0].schedule_id')"
{ [ -n "$SID" ] && [ "$SID" != null ]; } || fail "catalog enable self-improve returned no schedule_id"
pass "catalog enable self-improve created schedule $SID"

# --- fire it: must START, not skip -------------------------------------------
RN="$(uzi_cli schedule run-now "$SID" --json)" || fail "schedule run-now (self_improve) failed (exit $?)"
# A skip here is the recordable blocker; name the reason so a red is self-diagnosing.
if [ "$(echo "$RN" | jq -r '.started | length')" != 1 ]; then
  fail "self_improve run-now did not start a run (blocker); response: $(echo "$RN" | jq -c '{matched,started,skips}')"
fi
RUN_S="$(echo "$RN" | jq -r '.started[0].run_id')"
{ [ -n "$RUN_S" ] && [ "$RUN_S" != null ]; } || fail "self_improve run-now returned no run_id: $RN"
pass "self_improve run-now started run $RUN_S (no vault_locked / mr_cap skip)"

# --- assert the kind from the wire, then drive to completion -----------------
KIND="$(uzi_cli run get "$RUN_S" --field kind)" || fail "uzi run get --field kind failed (exit $?)"
[ "$KIND" = self_improve ] || fail "self_improve run kind should be 'self_improve', got '$KIND'"
pass "run $RUN_S is kind=self_improve"

wait_status "$RUN_S" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
MR_S="$(apiget "/api/runs/$RUN_S" | jq -r '.run.mr_iid')"
{ [ "$MR_S" != null ] && [ "$MR_S" -gt 0 ]; } || fail "self_improve run opened no MR (got $MR_S)"
pass "self_improve run $RUN_S completed through the stub and opened MR !$MR_S"

# --- delete the schedule; leave vault state as found (unlocked) --------------
uzi_cli schedule delete "$SID" >/dev/null || fail "schedule delete failed (exit $?)"
pass "deleted self-improve schedule $SID; run terminal (quarantine-clean)"
