# shellcheck shell=bash
# phase:    schedules-sweep
# title:    PRD #966 M4: scheduled Planned-sweep (catalog enable, run-now tallies, uzi-gate skip, open-MR skip)
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID UZI_BIN UZI_TOKEN_VAL
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #966 M4 — the scheduling subsystem drives production every night yet had no
# wire coverage. This phase exercises the `planned-sweep` catalog default end to end
# through the live CLI (uzc_ Bearer), the scheduler's RunNow seam, the worker, and
# the fake forge:
#
#   - `catalog enable` is idempotent: a second enable reports the row already enabled
#     (created=false), not a duplicate.
#   - A sweep matches its candidates from the CACHED issues table (ListSweepCandidateIssues
#     selects state='opened' AND labels @> ["Planned"], the selector label only — NOT the
#     uzi label), so an issue is invisible until a repo `sync` populates the cache.
#   - The uzi-label eligibility gate is applied at the RUN-CREATION seam, not at candidate
#     selection: an issue with `Planned` but no `uzi` matches (matched++) yet is SKIPPED
#     with reason `not_eligible`. `matched` is the positive control that the query saw both.
#   - A completed run that still owns an OPEN MR blocks a re-fire of the same issue with
#     `open_mr_exists` (or `already_running` if its run is still live).
#
# THE last_fire FORK, decided from the code (NOT run-now response == schedule.last_fire):
# schedsvc RunNow does NOT persist last_fire. scheduler.go:244 ("RunNow does NOT reach
# [advance], so a manual fire never persists a last_fire (Decision 3)") and
# TestRunNowDoesNotPersistLastFire pin this behaviourally. So `schedule get` after a
# run-now returns last_fire = null; the run-now RESPONSE tallies (matched/started/skips)
# are the authoritative outcome and are what this phase asserts. We additionally assert
# `schedule get --json | .last_fire == null` as a POSITIVE control of Decision 3, so a
# future refactor that routes RunNow through advance() reddens here, not silently.
say "PRD #966 M4: scheduled Planned-sweep (catalog enable, run-now tallies, uzi-gate skip, open-MR skip)"

# --- catalog enable is idempotent -------------------------------------------
# --create-missing-labels POSTs the selector label to the forge. On the gitlab lane the
# fake's GET /labels returns a hardcoded [] and POST is not persisted (forge-fake.mjs),
# so label existence is NOT observable via a follow-up GET — we assert the enable
# succeeds, never a label GET. The issue's OWN labels (set at creation) are what
# candidate-matching reads.
EN1="$(uzi_cli schedule catalog enable planned-sweep --repo "$REPO_ID" --create-missing-labels --json)" \
  || fail "schedule catalog enable planned-sweep failed (exit $?)"
SID="$(echo "$EN1" | jq -r '.[0].schedule_id')"
{ [ -n "$SID" ] && [ "$SID" != null ]; } || fail "catalog enable returned no schedule_id: $EN1"
echo "$EN1" | jq -e '.[0].created == true' >/dev/null \
  || fail "first catalog enable should report created=true: $EN1"
pass "catalog enable planned-sweep created schedule $SID"

EN2="$(uzi_cli schedule catalog enable planned-sweep --repo "$REPO_ID" --json)" \
  || fail "second schedule catalog enable failed (exit $?)"
echo "$EN2" | jq -e --arg sid "$SID" '.[0].created == false and .[0].schedule_id == $sid' >/dev/null \
  || fail "a second catalog enable must report the SAME schedule already enabled (created=false): $EN2"
pass "a second catalog enable reports already-enabled (created=false, same schedule $SID)"

# --- stage two candidates, sync into the issues cache ------------------------
# A: Planned + uzi (eligible). B: Planned only (matches the query, fails the uzi gate).
# Created via the raw /_e2e/issues mutator (NOT create_autopilot_issue, which hardcodes
# the autopilot label) so the label arrays are exactly what we set.
IID_A="$(fake_post /_e2e/issues \
  "$(jq -nc '{title:"E2E sweep A (eligible)",description:"planned sweep, uzi-eligible",labels:["Planned","uzi"]}')" | jq -r '.iid')"
{ [ -n "$IID_A" ] && [ "$IID_A" != null ]; } || fail "could not stage sweep issue A on the fake"
IID_B="$(fake_post /_e2e/issues \
  "$(jq -nc '{title:"E2E sweep B (no uzi)",description:"planned sweep, not uzi-eligible",labels:["Planned"]}')" | jq -r '.iid')"
{ [ -n "$IID_B" ] && [ "$IID_B" != null ]; } || fail "could not stage sweep issue B on the fake"
# The sweep reads the CACHED issues table, so sync AFTER staging or candidates are invisible.
apipost "/api/repos/$REPO_ID/sync" '' >/dev/null
pass "staged sweep issues A #$IID_A (Planned+uzi) and B #$IID_B (Planned), synced into the cache"

# --- 1st fire: A started, B skipped not_eligible -----------------------------
RN1="$(uzi_cli schedule run-now "$SID" --json)" || fail "schedule run-now (1st fire) failed (exit $?)"
echo "$RN1" | jq -e '.matched == 2' >/dev/null \
  || fail "1st fire: matched should be 2 (the query saw both Planned candidates): $RN1"
echo "$RN1" | jq -e --argjson a "$IID_A" '(.started | length) == 1 and .started[0].issue_iid == $a' >/dev/null \
  || fail "1st fire: exactly one start, on issue A #$IID_A: $RN1"
echo "$RN1" | jq -e --argjson b "$IID_B" \
  'any(.skips[]; .issue_iid == $b and .reason == "not_eligible")' >/dev/null \
  || fail "1st fire: B #$IID_B must be skipped with reason not_eligible: $RN1"
pass "1st fire: matched=2, started=1 on A #$IID_A, B #$IID_B skipped not_eligible"

RUN_A="$(echo "$RN1" | jq -r '.started[0].run_id')"
{ [ -n "$RUN_A" ] && [ "$RUN_A" != null ]; } || fail "1st fire: no run_id for the started run: $RN1"
wait_status "$RUN_A" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
RUN_A_JSON="$(apiget "/api/runs/$RUN_A")"
BR_A="$(echo "$RUN_A_JSON" | jq -r '.run.branch // empty')"
[ -n "$BR_A" ] || fail "A's run pushed no branch: $(echo "$RUN_A_JSON" | jq -c '.run.branch')"
MR_A="$(echo "$RUN_A_JSON" | jq -r '.run.mr_iid')"
{ [ "$MR_A" != null ] && [ "$MR_A" -gt 0 ]; } || fail "A's run opened no MR (got $MR_A)"
pass "A's swept run $RUN_A completed through the stub (branch + MR !$MR_A)"

# --- Decision 3 positive control: run-now does NOT persist last_fire ----------
# (see the header). schedule get after a run-now must show last_fire = null.
uzi_cli schedule get "$SID" --json | jq -e '.last_fire == null' >/dev/null \
  || fail "run-now must NOT persist last_fire (Decision 3): schedule get shows a non-null last_fire"
pass "Decision 3 positive control: schedule get after run-now shows last_fire=null (tallies come from the run-now response)"

# --- relabel B eligible, verify the cache changed, re-fire -------------------
# Add uzi to B via the product UpdateIssue route on the fake (PUT .../issues/:iid with
# add_labels) — the harness stand-in for a human relabelling. resolveProject falls back
# to the single project for any id, so project id "1" resolves it. The /_e2e
# label-events mutator only appends an EVENT and does NOT touch issue.labels, so it is
# the wrong tool here; this route is what SETS the cached label array.
curl -fsSk -X PUT "$FAKE_BASE/api/v4/projects/1/issues/$IID_B" \
  -H 'Content-Type: application/json' -d '{"add_labels":["uzi"]}' >/dev/null \
  || fail "could not add the uzi label to B #$IID_B on the fake"
apipost "/api/repos/$REPO_ID/sync" '' >/dev/null
# Positive control: B's cached labels now include uzi (else the re-fire would prove nothing).
apiget "/api/repos/$REPO_ID/board" | jq -e --argjson iid "$IID_B" \
  'any(.board.cards[]; .iid == $iid and ((.labels // []) | index("uzi")))' >/dev/null \
  || fail "B #$IID_B cached labels do not include uzi after relabel+sync (positive control failed)"
pass "relabelled B #$IID_B to include uzi; cached labels reflect it"

RN2="$(uzi_cli schedule run-now "$SID" --json)" || fail "schedule run-now (2nd fire) failed (exit $?)"
echo "$RN2" | jq -e --argjson b "$IID_B" \
  '(.started | length) == 1 and .started[0].issue_iid == $b' >/dev/null \
  || fail "2nd fire: exactly one start, on the now-eligible B #$IID_B: $RN2"
# A already has a completed run owning an open MR (or a still-live run): its re-fire is
# skipped. Assert the REASON is one of the two blocking reasons, not a bare count.
echo "$RN2" | jq -e --argjson a "$IID_A" \
  'any(.skips[]; .issue_iid == $a and (.reason == "open_mr_exists" or .reason == "already_running"))' >/dev/null \
  || fail "2nd fire: A #$IID_A must be skipped open_mr_exists or already_running: $RN2"
pass "2nd fire: B #$IID_B now started; A #$IID_A skipped (open_mr_exists/already_running)"

RUN_B="$(echo "$RN2" | jq -r '.started[0].run_id')"
{ [ -n "$RUN_B" ] && [ "$RUN_B" != null ]; } || fail "2nd fire: no run_id for the started run: $RN2"
wait_status "$RUN_B" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "B's swept run $RUN_B completed through the stub"

# --- pause, delete, leave nothing non-terminal -------------------------------
uzi_cli schedule pause "$SID" >/dev/null || fail "schedule pause failed (exit $?)"
uzi_cli schedule delete "$SID" >/dev/null || fail "schedule delete failed (exit $?)"
pass "paused then deleted schedule $SID; both swept runs terminal (quarantine-clean)"
