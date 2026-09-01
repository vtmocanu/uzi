# shellcheck shell=bash
# phase:    plan-revision-loop
# title:    PRD #41: plan-revision loop (revise_plan -> re-plan -> re-park -> approve -> MR)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #41 — plan-revision loop: a run parked at the approval gate can be sent
# BACK with reviewer feedback (revise_plan) before approval, instead of the
# binary approve/reject. The worker records the feedback on the feed
# (plan_feedback), opens a revision round (plan_revising), re-plans, and RE-PARKS
# at the gate with a NEW plan (the stub mirrors sdk-executor.ts's revise loop and
# appends a `(revision N: applied feedback)` marker) — then a normal approve
# drives it through implement → push → MR, exactly like the happy path. Proves
# the full plan → revise → approve → MR flow end to end over the real HTTP
# steering surface, with a LIVE worker so the revise is poller-consumed. Runs at
# the DEFAULT cap-1 worker, before the PRD #42 phase below reconfigures it.
#
# DELIBERATELY NOT asserted here: the stale-approve-discarded race (an approve
# that arrives interleaved with an in-flight revise being dropped). Over HTTP
# from bash the interleaving can't be made deterministic, so forcing it would
# only flake; it is covered by the steering unit tests + the store integration
# test.
say "PRD #41: plan-revision loop (revise_plan → re-plan → re-park → approve → MR)"
IID_RV="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E revise","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_RV="$(create_run "$REPO_ID" "$IID_RV")" || fail "revise-path run-create failed (non-transient; see stderr)"
{ [ -n "$RUN_RV" ] && [ "$RUN_RV" != null ]; } || fail "revise-path run was not created"

# v1 gate: the run parks at awaiting_approval carrying a plan.
wait_status "$RUN_RV" awaiting_approval
PLAN_V1="$(apiget "/api/runs/$RUN_RV" | jq -r '.run.plan_md // empty')"
[ -n "$PLAN_V1" ] || fail "revise-path run reached the v1 gate with no plan_md"
pass "run $RUN_RV reached the v1 plan gate (awaiting_approval) with a plan"

# Send the plan back with feedback. A LIVE worker is online, so like the PRD #33
# reject this is poller-consumed (server_side=false), not applied server-side.
SS_RV="$(apipost "/api/runs/$RUN_RV/inputs" \
  '{"kind":"revise_plan","body":"drop step 3 and reuse the existing endpoint"}' | jq -r '.server_side')"
[ "$SS_RV" = false ] || fail "a revise against a LIVE worker must be poller-consumed, not server-side (got server_side=$SS_RV)"

# The worker records the feedback then opens a revision round on the feed, in that
# order (executor emits plan_feedback BEFORE plan_revising, both before the
# re-gate flushes the new plan).
wait_msg_kind "$RUN_RV" plan_feedback
wait_msg_kind "$RUN_RV" plan_revising
pass "revise consumed (poller-side): worker emitted plan_feedback + plan_revising"

# v2 re-park: poll until the REVISED plan actually lands. plan_md only carries the
# `(revision N: applied feedback)` marker once the re-gate re-reports
# awaiting_approval with the revised plan, so waiting for the marker IS waiting
# for the v2 re-park — robust against reading the still-v1 plan_md in the window
# between plan_revising and the re-report.
rv_deadline=$((SECONDS + 60)); PLAN_V2=""
while [ $SECONDS -lt $rv_deadline ]; do
  PLAN_V2="$(apiget "/api/runs/$RUN_RV" | jq -r '.run.plan_md // empty')"
  printf '%s' "$PLAN_V2" | grep -q '(revision 1: applied feedback)' && break
  sleep 0.3
done
wait_status "$RUN_RV" awaiting_approval
printf '%s' "$PLAN_V2" | grep -q '(revision 1: applied feedback)' \
  || fail "v2 plan_md never picked up the stub revision marker (got: ${PLAN_V2:-none})"
[ "$PLAN_V2" != "$PLAN_V1" ] || fail "v2 plan_md is identical to v1 (the revision did not take)"
[ "$(apiget "/api/runs/$RUN_RV/messages" | jq '[.messages[] | select(.kind=="plan")] | length')" -ge 2 ] \
  || fail "expected >=2 'plan' messages after the revision round (v1 + v2)"
pass "run re-parked at the gate with a revised plan (v2 != v1, revision marker present, >=2 plan messages)"

# Approve the revised plan → implement → push → MR, mirroring the happy-path tail.
apipost "/api/runs/$RUN_RV/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_RV" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
RV_FINAL="$(apiget "/api/runs/$RUN_RV")"
[ "$(echo "$RV_FINAL" | jq -r '.run.status')" = completed ] || fail "revise-path final status is not completed"
[ "$(echo "$RV_FINAL" | jq -r '.run.branch')" = "agent/issue-$IID_RV" ] \
  || fail "revise-path run.branch is not agent/issue-$IID_RV"
RV_MR="$(echo "$RV_FINAL" | jq -r '.run.mr_iid')"
{ [ "$RV_MR" != null ] && [ "$RV_MR" -gt 0 ]; } || fail "revise-path run.mr_iid not set (got $RV_MR)"
git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_RV" \
  || fail "branch agent/issue-$IID_RV was not pushed to the remote after approving the revised plan"
[ "$(fake_state | jq -r --arg b "agent/issue-$IID_RV" '[.mrs[] | select(.source_branch==$b)] | length')" -ge 1 ] \
  || fail "fake GitLab recorded no MR from agent/issue-$IID_RV"
pass "revised plan approved → completed: branch=agent/issue-$IID_RV pushed, mr_iid=$RV_MR recorded on the fake GitLab"

