# shellcheck shell=bash
# phase:    steer-queue-delivery
# title:    PRD #95: steer-queue delivery — Queued -> Delivered on consume, no run_message, no forge/token
# critical: no
# lane:     gitlab
# executor: any
# race-sensitive: yes
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #95 — steer-queue delivery (Problem 3): a follow-up shows as Queued on submit,
# flips to Delivered when the worker consumes it, is NEVER mirrored into run_messages
# (Decision 4 — the headline invariant), and spends no token / writes no forge (the
# whole reason the queue is a DTO over run_user_inputs, not a run_message).
#
# The stub DOES consume inputs naturally — no /_e2e mutator or hand-driven worker call
# is needed: the runner's SteeringChannel polls GET /api/worker/runs/{id}/inputs
# (→ ConsumeInputs), consuming EVERY pending input and buffering a follow_up for the
# agent's next turn. We submit the follow-up in the queued/claiming window BEFORE the
# run is owned and its steering poll has run, then read it back as Queued; the gate's
# poll then flips it to Delivered while the run sits at awaiting_approval — the S3
# "Delivered — applies after approval" case, driven end to end by the real worker poll.
# (The dropped-frame reconnect self-heal, S1, is proven in
# web/src/lib/useRunStream.test.tsx — e2e has no browser WS, so it is not re-tested here.)
#
# ── THE Queued OBSERVATION IS MADE DETERMINISTIC BY A VAULT LOCK (PRD #97 M9) ──
# History, because the wrong version of this comment cost two runs: it used to state the
# steering poll runs "every WORKER_POLL_INTERVAL (3s)" and that Queued was therefore
# observed "DETERMINISTICALLY". Both were false, and the second followed from the first:
#   • 3s is the PRODUCT DEFAULT (`agent/src/config.ts:220`).
#   • This harness OVERRIDES it to 500ms (`e2e/docker-compose.e2e.yml:182`), and that is
#     what drives the SteeringChannel poll (main.ts:124 -> runner.ts:173 -> steering.ts:252/389).
# So the window in which `consumed_at` is still NULL was ~500ms, not ~3s, and the read
# was a coin flip: observed failing 2026-07-20 with consumed_at 332ms after created_at.
#
# The fix is NOT a wider timeout — the property is real; the PRECONDITION was unenforced.
# We now enforce it: lock the owner's vault so the worker gate provably withholds the run
# (PRD #32 asserts exactly this at the vault phase — "a locked owner's run must stay
# queued (never claimed, never failed)"), which turns "unclaimed" from a ~500ms window
# into a STABLE state. We then assert Queued twice across several worker poll cycles —
# strictly STRONGER than the old single read, which could not tell a stable state from a
# lucky snapshot — before unlocking and asserting the real Queued -> Delivered transition
# through the live worker exactly as before. No assertion is weakened; one is added.
say "PRD #95: steer-queue delivery — Queued → Delivered on consume, no run_message, no forge/token"
STEER_MRS_BEFORE="$(fake_state | jq '.mrs | length')"

# Lock FIRST: the run must be un-claimable from the instant it exists.
apipost /api/vault/lock '' >/dev/null
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = false ] \
  || fail "PRD #95: the vault must lock before the steer run is created — without it the Queued read below is a ~500ms race, not an assertion"

IID_S="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E steer","description":"steer me — see prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_S="$(create_run "$REPO_ID" "$IID_S")" || fail "steer-path run-create failed (non-transient; see stderr)"
{ [ -n "$RUN_S" ] && [ "$RUN_S" != null ]; } || fail "steer-path run was not created"

# The vault gate withholds the run at CLAIM, so it cannot be owned and no steering poll
# can exist for it. Assert that precondition explicitly: if a future change lets this run
# be claimed, it fails HERE with a clear cause instead of resurfacing as a mystery flake
# in the consumed_at assertion below.
[ "$(apiget "/api/runs/$RUN_S" | jq -r '.run.status')" = queued ] \
  || fail "PRD #95: the vault lock must keep run $RUN_S unclaimed (got $(apiget "/api/runs/$RUN_S" | jq -r '.run.status')) — the Queued observation would be a race again"

# Submit the follow-up, then read the owner queue: the write returns the created row (S2)
# and the queue shows it Queued (consumed_at null).
STEER_BODY="please also update the changelog"
SUB="$(apipost "/api/runs/$RUN_S/inputs" "$(jq -cn --arg b "$STEER_BODY" '{kind:"follow_up",body:$b}')")"
[ "$(echo "$SUB" | jq -r '.server_side')" = false ] \
  || fail "a follow_up must never be server-side (got server_side=$(echo "$SUB" | jq -r '.server_side'))"
STEER_ID="$(echo "$SUB" | jq -r '.id')"
{ [ "$STEER_ID" != null ] && [ "$STEER_ID" -gt 0 ]; } || fail "follow_up write did not return the created row id (S2), got '$STEER_ID'"
[ "$(echo "$SUB" | jq -r '.created_at // empty')" != "" ] || fail "follow_up write did not return created_at (S2)"
Q="$(apiget "/api/runs/$RUN_S/inputs")"
[ "$(echo "$Q" | jq '.inputs | length')" = 1 ] || fail "steer queue should list exactly the one follow_up (got $(echo "$Q" | jq -c '.inputs'))"
[ "$(echo "$Q" | jq -r '.inputs[0].body')" = "$STEER_BODY" ] || fail "queued follow_up body mismatch"
[ "$(echo "$Q" | jq -r '.inputs[0].consumed_at')" = null ] \
  || fail "a freshly-submitted follow_up must be Queued (consumed_at null), got $(echo "$Q" | jq -c '.inputs[0]')"

# STABILITY (PRD #97 M9): re-read after several worker poll cycles. Under the lock this
# must STILL be Queued — that is what distinguishes an enforced precondition from a lucky
# snapshot, and it is the assertion the old single read could never make.
sleep 1.5   # ~3 worker poll cycles (500ms each, overlay :182)
[ "$(apiget "/api/runs/$RUN_S/inputs" | jq -r '.inputs[0].consumed_at')" = null ] \
  || fail "PRD #95: the follow_up was consumed while the owner's vault was LOCKED — the worker gate should have withheld run $RUN_S entirely"
[ "$(apiget "/api/runs/$RUN_S" | jq -r '.run.status')" = queued ] \
  || fail "PRD #95: run $RUN_S left 'queued' while the vault was locked"
pass "follow_up submitted: write returned the row (id=$STEER_ID); Queued is STABLE across ~3 worker poll cycles under the vault lock (not a snapshot)"

# Unlock: the worker may now claim, and the run proceeds to the gate where its steering
# poll consumes the follow_up (stamping consumed_at) while the run stays
# awaiting_approval — Delivered, S3 flavor. Everything from here is the ORIGINAL
# assertion set, driven by the real worker poll.
apipost /api/vault/unlock "{\"password\":\"$ADMIN_PASS\"}" >/dev/null
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = true ] \
  || fail "PRD #95: the vault must be unlocked again — later phases (and the dedicated PRD #32 phase) assume an unlocked admin vault"
wait_status "$RUN_S" awaiting_approval
wait_eq delivered 30 "run $RUN_S follow-up delivery" run_input_delivery "$RUN_S"
DLV="$(apiget "/api/runs/$RUN_S/inputs")"
[ "$(echo "$DLV" | jq -r '.inputs[0].consumed_at')" != null ] || fail "consumed follow_up must show Delivered (consumed_at set)"
[ "$(echo "$DLV" | jq -r '.inputs[0].id')" = "$STEER_ID" ] || fail "delivered row id drifted from the submitted id"
[ "$(apiget "/api/runs/$RUN_S" | jq -r '.run.status')" = awaiting_approval ] \
  || fail "the run should still be at the gate when the follow_up is consumed (S3 delivered-applies-after-approval)"
# DIAGNOSTIC (PRD #97 M4/M9), not an assertion. READ THE LABEL CAREFULLY: since M9 this
# number spans the DELIBERATE vault-lock window, so it is a total submit→delivery span,
# NOT a race margin. There is no race margin here any more — the lock makes Queued a
# stable state by construction, so the old "how close did we come" reading no longer
# applies and reporting it as one would be a lie of exactly the kind M9 exists to remove.
# It is still worth printing: a sudden jump means delivery-after-unlock got slower.
# Never fails the run — a jq hiccup degrades to "unknown" rather than aborting a ~9-min
# suite over a print. jq's fromdateiso8601 cannot parse fractional seconds, so split the
# timestamp and add the milliseconds back by hand (verified against the real 2026-07-20
# failure pair: .296723Z → .628886Z yields 332).
STEER_MARGIN_MS="$(jq -rn --arg c "$(echo "$DLV" | jq -r '.inputs[0].created_at')" \
                          --arg d "$(echo "$DLV" | jq -r '.inputs[0].consumed_at')" '
  def epochms: capture("^(?<t>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<f>[0-9]+))?Z$")
    | ((.t + "Z") | fromdateiso8601) * 1000 + (((.f // "0") + "000")[0:3] | tonumber);
  (($d | epochms) - ($c | epochms)) | tostring' 2>/dev/null || echo unknown)"
[ -n "$STEER_MARGIN_MS" ] || STEER_MARGIN_MS=unknown
say "PRD #95 DIAGNOSTIC: total submit→delivery span ≈ ${STEER_MARGIN_MS}ms (spans the deliberate vault-lock window — NOT a race margin; the lock removed the race)"
pass "worker consumed the follow_up at the gate: queue Queued → Delivered, run still awaiting_approval (S3)"

# Decision 4 — the headline invariant: the follow_up is a DTO over run_user_inputs,
# NEVER a run_message. Its body appears in NO message payload, and the gapless per-run
# seq is intact (no server-injected message racing the worker's local seq allocator).
SMSGS="$(apiget "/api/runs/$RUN_S/messages")"
echo "$SMSGS" | jq -e --arg b "$STEER_BODY" '[.messages[] | select((.payload | tostring) | contains($b))] | length == 0' >/dev/null \
  || fail "the follow_up body leaked into run_messages — Decision 4 says a follow_up is NEVER a run_message"
echo "$SMSGS" | jq -e '(.messages | length) as $n | [.messages[].seq] == [range(1; $n+1)]' >/dev/null \
  || fail "run_messages seq is not gapless 1..N after the follow_up round-trip (a follow_up must not perturb the seq stream)"
pass "Decision 4: no run_message written for the follow_up; run_messages seq still gapless 1..$(echo "$SMSGS" | jq '.messages | length')"

# No forge write, no token spend attributable to the steer round-trip: the fake forge
# recorded no new MR, no branch was pushed for this issue, and the parked run banked no
# usage (a steer neither runs the agent nor writes the forge).
[ "$(fake_state | jq '.mrs | length')" = "$STEER_MRS_BEFORE" ] \
  || fail "the follow_up path created a forge MR (before=$STEER_MRS_BEFORE, after=$(fake_state | jq '.mrs | length')) — a steer must never write the forge"
if git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_S"; then
  fail "a branch was pushed for the steer run while it was still at the gate — no forge write may attribute to the follow_up"
fi
apiget "/api/runs/$RUN_S" | jq -e '((.run.usage.input_tokens // 0) == 0) and ((.run.usage.output_tokens // 0) == 0)' >/dev/null \
  || fail "the steer run banked token usage while parked at the gate — a follow_up spends no tokens (got: $(apiget "/api/runs/$RUN_S" | jq -c '.run.usage'))"
pass "no forge write + no token spend for the follow_up path (MR count unchanged, no branch, run.usage zero)"

# The queue survives the run going terminal (B1): cancel to clean up, then the same
# Delivered follow_up is still readable on the now-terminal run (it lives in
# run_user_inputs, not the composer's unmounted component state).
#
# Terminal status is `cancelled`, NOT `failed` (PRD #503 M1, landed in #521): a cancel
# consumed by a LIVE worker is reported as `failed`, but SetState's failed arm routes off
# the run's already-stamped stop_kind='cancelled' to CancelRunByWorker — status
# 'cancelled', fail_origin NULL, never judged — so an operator cancellation is no longer
# misclassified as an agent failure. The server-side no-poller path (CancelRunServerSide,
# used by the queued-run cancel phase above) converges on the SAME 'cancelled' status, so
# this assertion no longer depends on which path the cancel takes.
#
# stop_kind keeps this non-vacuous: it is the server-stamped deliberate-stop signal
# (PRD #33), so 'cancelled' proves the run ended because of THIS cancel, not a coincidental
# failure (which would surface as status 'failed', caught by wait_status). The reason text
# moved too: CancelRunByWorker touches neither failure_reason nor stop_reason, and this
# cancel carries an empty body (no operator -m message), so there is no reason string to
# assert — the old failure_reason="run cancelled" check would now be vacuously empty.
apipost "/api/runs/$RUN_S/inputs" '{"kind":"cancel","body":""}' >/dev/null
wait_status "$RUN_S" cancelled
[ "$(apiget "/api/runs/$RUN_S" | jq -r '.run.stop_kind // empty')" = "cancelled" ] \
  || fail "a live-worker cancel must terminate the run as cancelled(stop_kind=cancelled), got status='$(apiget "/api/runs/$RUN_S" | jq -r '.run.status')' stop_kind='$(apiget "/api/runs/$RUN_S" | jq -r '.run.stop_kind // empty')'"
[ "$(apiget "/api/runs/$RUN_S/inputs" | jq -r '.inputs[0].consumed_at')" != null ] \
  || fail "the delivered follow_up must remain readable (and Delivered) after the run goes terminal (B1 survive-terminal)"
pass "steer queue survives terminal: the Delivered follow_up is still listed on the now-terminal (cancelled) run (B1)"

