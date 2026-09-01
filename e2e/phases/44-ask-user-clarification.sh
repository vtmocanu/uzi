# shellcheck shell=bash
# phase:    ask-user-clarification
# title:    PRD #88: ask-user clarification (park -> answer -> resume -> approve -> MR)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# PRD #88 — ask-user clarification: a run PARKS on an agent-initiated question,
# the human answers over the real HTTP steering surface, and the SAME session
# resumes. This is the milestone's own acceptance line ("provable end-to-end with
# the stub executor"), and it is the only place the whole chain runs together:
# signal -> question run-message -> awaiting_input state report -> answer input ->
# SetRunRunning's identity guard -> resumed session -> plan gate -> MR.
#
# The stub asks BEFORE it plans (STUB_ASK_SENTINEL sits ahead of the plan gate in
# executor.ts), so this also exercises M4's pre-run park — the path that used to
# hard-fail on REASON_NO_PLAN, since a planning turn that asks ends with questions
# and NO plan.
#
# Runs at the DEFAULT cap-1 worker, before the PRD #42 phase below reconfigures it.
say "PRD #88: ask-user clarification (park -> answer -> resume -> approve -> MR)"
IID_AU="$(apipost "/api/repos/$REPO_ID/issues" \
  "$(jq -cn '{title:"E2E clarify", description:"implements prds/4-agent-runtime-workers.md — but first: UZI_STUB_ASK"}')" | jq -r '.card.iid')"
RUN_AU="$(create_run "$REPO_ID" "$IID_AU")" || fail "ask-user run-create failed (non-transient; see stderr)"
{ [ -n "$RUN_AU" ] && [ "$RUN_AU" != null ]; } || fail "ask-user run was not created"

# The park. awaiting_input is a DISTINCT status from awaiting_approval — the whole
# reason it exists is that an answer can never satisfy the plan gate's resume guard.
wait_status "$RUN_AU" awaiting_input
wait_msg_kind "$RUN_AU" question
pass "run $RUN_AU parked at awaiting_input with a question on the feed"

# The open question is derived from the FEED, not from a run field (D-L): web, CLI
# and Slack all share this one derivation rule. Newest `question` message wins.
QMSG="$(apiget "/api/runs/$RUN_AU/messages" | jq -c '[.messages[] | select(.kind=="question")] | last')"
QID="$(echo "$QMSG" | jq -r '.payload.question_id // empty')"
[ -n "$QID" ] || fail "the question message carries no question_id (payload: $QMSG)"
[ "$(echo "$QMSG" | jq -r '.payload.questions | length')" -ge 1 ] \
  || fail "the question message carries no questions (payload: $QMSG)"
pass "question payload carries a question_id ($QID) and at least one question"

# An answer naming a DIFFERENT question must be rejected. This is the stale-answer
# guard — the case that actually happens is a Slack reply to question N landing
# after the agent has moved on to N+1 — and asserting it here proves the identity
# keying end to end over HTTP, not just in the store test.
STALE_CODE="$(curl -sS -b "$JAR" -o /dev/null -w '%{http_code}' -X POST "$BASE/api/runs/$RUN_AU/inputs" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" \
  -d "$(jq -cn '{kind:"answer", body:"{\"question_id\":\"not-the-open-question\",\"answers\":[\"nope\"]}"}')")"
[ "$STALE_CODE" = 409 ] \
  || fail "an answer naming a stale question must be rejected with 409, got $STALE_CODE"
# ...and the run must still be parked: a rejected answer must not disturb the park.
[ "$(apiget "/api/runs/$RUN_AU" | jq -r '.run.status')" = awaiting_input ] \
  || fail "a rejected stale answer un-parked the run"
pass "an answer naming a stale question is rejected (409) and leaves the run parked"

# The real answer, naming the open question.
SS_AU="$(apipost "/api/runs/$RUN_AU/inputs" \
  "$(jq -cn --arg q "$QID" '{kind:"answer", body:({question_id:$q, answers:["proceed"]} | tojson)}')" | jq -r '.server_side')"
[ "$SS_AU" = false ] || fail "an answer against a LIVE worker must be poller-consumed, not server-side (got server_side=$SS_AU)"

# The worker echoes the answer to the feed, then the run leaves the park. Both are
# asserted: the echo is what makes the round trip auditable, and the status change
# is what proves SetRunRunning's identity guard was actually satisfied.
wait_msg_kind "$RUN_AU" answer
pass "answer accepted and echoed to the feed"

# The run resumes and reaches the ordinary plan gate — the pre-run park does NOT
# replace the gate, it precedes it.
wait_status "$RUN_AU" awaiting_approval
[ -n "$(apiget "/api/runs/$RUN_AU" | jq -r '.run.plan_md // empty')" ] \
  || fail "the resumed run reached the plan gate with no plan_md"
pass "run resumed from the park and reached the plan gate with a plan"

# An answer submitted when nothing is being asked is rejected (D-F): SubmitInput
# otherwise accepts any non-terminal run, so a pre-seeded answer would resolve the
# next question the instant it opened, with the user never seeing it.
NOTPARKED_CODE="$(curl -sS -b "$JAR" -o /dev/null -w '%{http_code}' -X POST "$BASE/api/runs/$RUN_AU/inputs" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" \
  -d "$(jq -cn --arg q "$QID" '{kind:"answer", body:({question_id:$q, answers:["too late"]} | tojson)}')")"
[ "$NOTPARKED_CODE" = 409 ] \
  || fail "an answer to a run that is not parked must be rejected with 409, got $NOTPARKED_CODE"
pass "an answer to a run that is not asking anything is rejected (409)"

# Approve → implement → push → MR, mirroring the happy-path tail. Proves the park
# left the run in a genuinely normal state rather than a special one.
apipost "/api/runs/$RUN_AU/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_AU" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
AU_FINAL="$(apiget "/api/runs/$RUN_AU")"
AU_MR="$(echo "$AU_FINAL" | jq -r '.run.mr_iid')"
{ [ "$AU_MR" != null ] && [ "$AU_MR" -gt 0 ]; } || fail "ask-user run.mr_iid not set (got $AU_MR)"
git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_AU" \
  || fail "branch agent/issue-$IID_AU was not pushed after the clarification round trip"
pass "clarified run completed: branch=agent/issue-$IID_AU pushed, mr_iid=$AU_MR"

