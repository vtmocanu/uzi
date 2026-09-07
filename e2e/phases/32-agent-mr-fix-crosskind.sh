# shellcheck shell=bash
# phase:    agent-mr-fix-crosskind
# title:    PRD #6: agent-MR same-branch fix + cross-kind race
# critical: no
# lane:     gitlab
# executor: any
# requires: env:E2E_FORGE_POLL_INTERVAL=2s
# provides: -
# handoff:  -
# mutates:  admin setting ci_autofix_enabled -> false (this phase drives the MANUAL
#           ci-fix path, so the autofix poller must not race it — see the note below)
# restores: admin setting ci_autofix_enabled -> true (the shipped default)
# 6) Agent-MR fix + cross-kind race. An issue run leaves an open MR on
#    agent/issue-N; a red pipeline on that MR gets a ci_fix run whose commits land
#    on the SAME branch (the existing MR updates, no second MR) and whose
#    verification stamps on it. While that ci_fix is active, an issue run on the
#    same issue is refused (they would share the worktree).
say "PRD #6: agent-MR same-branch fix + cross-kind race"

# This phase drives the MANUAL ci-fix path (POST /ci-fix-runs below) plus the
# cross-kind race, and needs to be the ONLY creator of a ci_fix on the agent branch.
# Since #1109 the admin setting ci_autofix_enabled defaults ON, so the CIAutoFix
# poller auto-opens a ci_fix on any agent/issue-N branch carrying a red pipeline --
# exactly the red pipeline this phase posts below. That auto-run races the phase's
# own manual POST and one loses with a 409 (one ci_fix per ref), flaking the phase
# intermittently (the "no fail message; last output: curl 409" nightly failures,
# issue #1155). Disable the poller for the duration; the trap restores it even if an
# assertion below fails and exits this phase's subshell early. Other ci-fix phases
# post on ref=main, which the agent/issue-N-only detector ignores, so only this one
# needs it.
apiput /api/admin/settings '{"settings":{"ci_autofix_enabled":"false"}}' >/dev/null
trap 'apiput /api/admin/settings '\''{"settings":{"ci_autofix_enabled":"true"}}'\'' >/dev/null 2>&1 || true' EXIT

AIID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E agent-MR fix","description":"implements prds/6-ci-status-integration.md"}' | jq -r '.card.iid')"
ARUN="$(create_run "$REPO_ID" "$AIID")" || fail "agent-MR-fix run-create failed (non-transient; see stderr)"
wait_status "$ARUN" awaiting_approval
apipost "/api/runs/$ARUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$ARUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
AGENTBRANCH="agent/issue-$AIID"
AMR="$(apiget "/api/runs/$ARUN" | jq -r '.run.mr_iid')"
{ [ "$AMR" != null ] && [ "$AMR" -gt 0 ]; } || fail "issue run left no MR on $AGENTBRANCH (got $AMR)"
pass "issue run left an open MR !$AMR on $AGENTBRANCH"

# Red pipeline on the agent branch's MR (LatestMRPipeline resolves MR->source_branch).
fake_post "/_e2e/pipelines" "{\"ref\":\"$AGENTBRANCH\",\"status\":\"failed\",\"jobs\":[{\"name\":\"unit\",\"stage\":\"test\",\"status\":\"failed\",\"trace\":\"--- FAIL: TestBar\\n\"}]}" >/dev/null
wait_card_pipeline "$AIID" failed 20
pass "red pipeline on $AGENTBRANCH is visible on card #$AIID"

AFIX="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" "{\"ref\":\"$AGENTBRANCH\"}" | jq -r '.run.id')"
wait_status "$AFIX" awaiting_approval

# Cross-kind race: an issue run on the SAME issue must 409 while the ci_fix holds
# the agent branch/worktree.
RACE="$(apipost_code "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$AIID}")"
[ "$RACE" = 409 ] || fail "issue run on #$AIID while a ci_fix holds $AGENTBRANCH must 409, got $RACE"
pass "cross-kind race: an issue run on #$AIID is refused (409) while a ci_fix holds $AGENTBRANCH"

MRS_BEFORE="$(fake_state | jq --arg b "$AGENTBRANCH" '[.mrs[] | select(.source_branch==$b)] | length')"
apipost "/api/runs/$AFIX/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$AFIX" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
[ "$(apiget "/api/runs/$AFIX" | jq -r '.run.branch')" = "$AGENTBRANCH" ] \
  || fail "agent-branch ci_fix did not land on $AGENTBRANCH"
[ "$(apiget "/api/runs/$AFIX" | jq -r '.run.mr_iid')" = "$AMR" ] \
  || fail "agent-branch ci_fix must reuse the existing MR !$AMR, no second MR"
MRS_AFTER="$(fake_state | jq --arg b "$AGENTBRANCH" '[.mrs[] | select(.source_branch==$b)] | length')"
[ "$MRS_AFTER" = "$MRS_BEFORE" ] || fail "agent-branch ci_fix opened a SECOND MR ($MRS_BEFORE -> $MRS_AFTER)"
pass "agent-branch ci_fix landed on $AGENTBRANCH, reused MR !$AMR, opened no second MR"

# Verification stamps on the agent branch too (the fix pipeline outranks the failure).
fake_post "/_e2e/pipelines" "{\"ref\":\"$AGENTBRANCH\",\"status\":\"success\"}" >/dev/null
wait_verdict "$AFIX" verified 20
pass "agent-branch fix pipeline passed -> run $AFIX stamped verified"

# Restore the poller and drop the fail-safe trap now that the explicit restore ran.
apiput /api/admin/settings '{"settings":{"ci_autofix_enabled":"true"}}' >/dev/null
trap - EXIT

