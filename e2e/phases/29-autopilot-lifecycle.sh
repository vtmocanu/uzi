# shellcheck shell=bash
# phase:    autopilot-lifecycle
# title:    PRD #19 autopilot: map + opt-in the repo owner
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #19 — admin settings + autopilot. The poller is already at ~1s and the
# reconcile cadence at every-2-ticks (both set with the MR-close phase's api
# recreate), so the FullSync-eviction dedup assertion has a bounded wait. Map the
# repo owner's forge username and opt them into autopilot: the two consent gates
# an unattended run requires (Decision 4).
say "PRD #19 autopilot: map + opt-in the repo owner"

CONN_ID="$(apiget /api/forge/connections | jq -r '.connections[0].id // empty')"
[ -n "$CONN_ID" ] || fail "no seeded forge connection to map"

# Carry-item (M3): human_username saves verified-or-warned. The fake knows no human
# accounts, so a real forge lookup runs and comes back empty → the value still saves,
# WITH a warning (never a hard reject). This is also the owner→username mapping the
# autopilot attribution needs.
WARN="$(apiput "/api/forge/connections/$CONN_ID" '{"human_username":"owner-alice"}' | jq -r '.warning // empty')"
[ -n "$WARN" ] || fail "human_username save should return a verified-or-warned warning"
[ "$(apiget /api/forge/connections | jq -r '.connections[0].human_username')" = owner-alice ] \
  || fail "human_username did not persist despite the warning"
pass "owner mapped to owner-alice (unverifiable → saved WITH a warning)"

[ "$(apiput /api/me/autopilot '{"enabled":true}' | jq -r '.user.autopilot_enabled')" = true ] \
  || fail "autopilot opt-in did not stick"
pass "repo owner opted into autopilot"

# --- autopilot #1: happy path ------------------------------------------------
say "autopilot #1: happy path — mapped+opted-in owner adds the label → unattended run → MR + success comment"
IID_AP="$(create_autopilot_issue "E2E autopilot happy" \
  "Implements prds/4-agent-runtime-workers.md under autopilot." owner-alice owner-alice)"
RUN_AP="$(wait_run_for_issue "$IID_AP" 40)"
[ "$(apiget "/api/runs/$RUN_AP" | jq -r '.run.auto_approve')" = true ] \
  || fail "autopilot run is not marked auto_approve"
pass "issue #$IID_AP: poller created an unattended auto_approve run ($RUN_AP) — no manual start"

wait_autopilot_done "$RUN_AP" completed 120
AP="$(apiget "/api/runs/$RUN_AP")"
# The plan is recorded as a run MESSAGE (kind:"plan"), emitted+flushed before the
# auto-approve verdict — NOT in run.plan_md, which only the awaiting_approval report
# sets and autopilot deliberately skips (so the run never parks at the gate).
apiget "/api/runs/$RUN_AP/messages" \
  | jq -e '[.messages[]? | select(.kind=="plan")] | length >= 1' >/dev/null \
  || fail "autopilot run recorded no plan message"
[ "$(echo "$AP" | jq -r '.run.branch')" = "agent/issue-$IID_AP" ] || fail "autopilot branch mismatch"
MR_AP="$(echo "$AP" | jq -r '.run.mr_iid')"
{ [ "$MR_AP" != null ] && [ "$MR_AP" -gt 0 ]; } || fail "autopilot run opened no MR (got $MR_AP)"
pass "run completed unattended (never parked at the gate), plan recorded, MR !$MR_AP opened"

# PRD #37 (autopilot variant): an autopilot run resolves its roster with NO human at
# the gate and RECORDS it (Decision 6 — the resolved selection rides the running
# state report, not an approve input). The seed repo ships .claude/agents/, so the
# resolved default is the repo source with no exclusions.
[ "$(echo "$AP" | jq -r '.run.agent_source')" = repo ] \
  || fail "autopilot run did not record a resolved agent_source (got $(echo "$AP" | jq -c '.run.agent_source'))"
echo "$AP" | jq -e '.run.agent_exclusions == []' >/dev/null \
  || fail "autopilot run's resolved exclusions should be [] (got: $(echo "$AP" | jq -c '.run.agent_exclusions'))"
pass "PRD #37: autopilot run recorded its resolved roster (agent_source=repo, no exclusions) with no human interaction"

wait_notes "$IID_AP" 1 40
notes_text "$IID_AP" | grep -qF "opened a merge request" || fail "expected the success comment with the MR link"
pass "exactly one success comment referencing the MR"

wait_card_column "$IID_AP" "Human Review" 40
pass "board label moved: card #$IID_AP resolved to Human Review"

# --- autopilot #2: no-consent (+ retry re-eval + eviction dedup) -------------
say "autopilot #2: no-consent — label added by an unmapped user → one explanatory comment, no run"
IID_NC="$(create_autopilot_issue "E2E autopilot no-consent" \
  "Implements prds/4-agent-runtime-workers.md" someone-else someone-else)"
wait_notes "$IID_NC" 1 40
notes_text "$IID_NC" | grep -qF "did not start a run" || fail "expected the no-eligible-user comment"
assert_no_run_for_issue "$IID_NC" 4  # 2 poll ticks (2s each): act + confirm-stable (Decision 5, PRD #97 M5)
pass "one 'no eligible user' comment, no run"

# Retry gesture: remove + re-add mints a larger event id → re-evaluated exactly once.
add_label_event "$IID_NC" remove someone-else
add_label_event "$IID_NC" add someone-else
wait_notes "$IID_NC" 2 40
pass "label remove+re-add (new event id) → re-evaluated once → second comment"

# A FullSync (eviction + resync of the issue cache) must NOT re-comment: the dedup
# marker lives in autopilot_triggers, not the evictable issue cache. This is a
# RECONCILE-driven negative, so Decision 5's floor is 2 RECONCILE periods, not 2 poll
# ticks: FORGE_POLL_INTERVAL=2s x FORGE_RECONCILE_EVERY=2 = a 4s reconcile period, so
# the floor is 8s. It sat at 6s — the only sub-floor window in the suite (PRD #97 M9;
# M5 correctly refused to LOWER it, M9 raises it to the floor). One reconcile to evict
# + one to confirm no re-comment followed.
sleep 8
[ "$(note_count "$IID_NC")" = 2 ] || fail "a FullSync eviction re-commented (trigger dedup must survive eviction)"
assert_no_run_for_issue "$IID_NC" 0
pass "no re-comment (and still no run) across a FullSync eviction"

# --- autopilot #3: failure path ----------------------------------------------
say "autopilot #3: failure path — the run fails (stub sentinel) → exactly one failure comment"
IID_FL="$(create_autopilot_issue "E2E autopilot failure" \
  "Implements prds/4-agent-runtime-workers.md then fails: UZI_STUB_FAIL" owner-alice owner-alice)"
RUN_FL="$(wait_run_for_issue "$IID_FL" 40)"
wait_status "$RUN_FL" failed 120
wait_notes "$IID_FL" 1 40
notes_text "$IID_FL" | grep -qF "could not complete" || fail "expected the failure comment"
notes_text "$IID_FL" | grep -qF "/runs/$RUN_FL" || fail "failure comment is missing the run link"
sleep 4   # 2 poll ticks (2s each): a duplicate would appear within a couple of ticks (Decision 5, PRD #97 M5)
[ "$(note_count "$IID_FL")" = 1 ] || fail "failure path posted more than one comment"
pass "exactly one failure comment (fixed template + run link), no failure_reason echoed"

# --- autopilot #5: no PRD link is no longer a gate (PRD #764) -----------------
# Pre-change (Gate B), a uzi+autopilot issue with no prds/*.md link got a "no PRD
# link" comment and NO run. Post-PRD #764 a PRD link is optional: the same issue now
# starts an unattended autopilot run. This is the fail-pre-change discriminator on
# the autopilot path.
say "autopilot #5: no PRD link is no longer a gate — a uzi+autopilot issue with no link runs"
IID_NP="$(create_autopilot_issue "E2E autopilot no-prd" \
  "This issue points at no plan file whatsoever." owner-alice owner-alice)"
RUN_NP="$(wait_run_for_issue "$IID_NP" 40)"
[ -n "$RUN_NP" ] && [ "$RUN_NP" != null ] || fail "a uzi+autopilot issue with no PRD link must now start a run"
wait_status "$RUN_NP" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "a uzi+autopilot issue with no PRD link runs unattended to completion ($RUN_NP)"

