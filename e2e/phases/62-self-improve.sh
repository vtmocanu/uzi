# shellcheck shell=bash
# phase:    self-improve
# title:    PRD #966 M4: scheduled self_improve run (vault-unlocked, tracking issue, MR opened)
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
# prompt/sweep, its real gate is the SCHEDULER, not the executor: fireSelfImprove
# (api/internal/schedsvc/self_improve.go) skips with a typed reason unless the owner's
# vault is UNLOCKED (SkipVaultLocked) and the per-repo open-self-improve-MR cap is under
# its limit (SkipSelfImproveMRCapReached), and it files/reuses a tracking issue on the
# forge (ensureTrackingIssue -> CreateIssue) and reads MR open-state (GetMergeRequest).
# All of those forge routes are served by forge-fake, and the StubExecutor is
# kind-agnostic, so a self_improve run reaches completed with an MR unchanged.
#
# The two skip reasons above are the drivable blockers: this phase UNLOCKS the admin
# vault first (the seam's precondition) and runs against a repo with no prior open
# self-improve MR (cap = 0 < 2). If run-now still returns a skip instead of a start,
# that skip reason IS the blocker to record — this phase asserts a Started, not a Skip.
#
# NOTE (SC9 / stretch): live behaviour is validated in CI; the full suite cannot run in
# the M4 worker (agent image build fails at devbox). No runner.ts/executor.ts change is
# made or needed — the stub drives self_improve to completed as-is.
say "PRD #966 M4: scheduled self_improve run (vault-unlocked, tracking issue, MR opened)"

# --- unlock the admin vault (fireSelfImprove's precondition) ------------------
# Reuse phase 34-vault.sh's unlock call. The seed admin is boot-unlocked, so "as found"
# is unlocked; this is idempotent and makes the phase sound under E2E_ONLY subsetting.
# The uzc_ CLI token is admin-scoped, so the scheduled self_improve run (owned by admin)
# sees the same unlocked DEK cache. We leave the vault unlocked (as found).
apipost /api/vault/unlock "{\"password\":\"$ADMIN_PASS\"}" >/dev/null
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = true ] \
  || fail "admin vault must be unlocked before a self_improve fire (fireSelfImprove skips vault_locked otherwise)"
pass "admin vault unlocked (self_improve precondition satisfied)"

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
