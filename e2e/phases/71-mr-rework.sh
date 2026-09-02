# shellcheck shell=bash
# phase:    mr-rework
# title:    PRD #966 M6: mr_rework run kind (review-landed rework on a settled MR)
# critical: no
# lane:     gitlab
# executor: any
# requires: IID MR_IID
# provides: env:E2E_FORGE_POLL_INTERVAL=2s
# handoff:  -
# mutates:  api:E2E_FORGE_POLL_INTERVAL=2s,FORGE_RECONCILE_EVERY=2
# restores: -
# =============================================================================
# PRD #966 M6 — the `mr_rework` run kind had zero wire coverage. The MR review-watcher
# (poller/mr_review_watch.go, wired unconditionally via engine.SetMRReviewWatch) queues
# an auto-approved mr_rework run for a completed issue run's open MR when, on a poll tick:
# GATE 1 the branch head pipeline is green; GATE 2 the review has SETTLED (the newest
# review comment is older than MR_REVIEW_QUIET_PERIOD, 2s in the overlay — a top-level
# note carries no diff position, so the driver leaves HeadSHA=="" and GATE 2 is the
# debounce alone); GATE 3 a kept comment id is STRICTLY above the consumed high-water;
# GATE 4 under the per-MR cap. The StubExecutor is kind-agnostic, so the run reaches
# completed and pushes one commit onto the agent branch. The happy-path run (RUN from
# phase 15) left an OPEN MR (!$MR_IID) on agent/issue-$IID; phase 27 leaves it open. This
# phase drives the detector end to end and asserts the id-gate in both directions.
#
# The forge-fake note author defaults to id 2 (a reviewer): forge-fake's /api/v4/user
# returns id 1 (the bot), and BuildReviewCommentsSnapshot drops notes authored by the bot
# id, so a bot-authored note is a SILENT no-fire. Every injected note passes author_id=2.
#
# WATCHED-REF CAP (issue #996): card #$IID (agent/issue-$IID) is the OLDEST agent branch in
# the suite — it is the phase-15 happy-path branch and this phase runs last. Both GATE 1's
# card-pipeline badge below AND the mr_rework detector read the pipeline-status cache, which
# the poller fills NEWEST-FIRST up to CI_WATCH_MAX_REFS (ListWatchedRunRefsForRepo). At the
# shipped default of 20 this ancient branch is evicted from the watched set, its pipeline is
# never synced, and card #$IID's badge stays 'none' — so the overlay raises CI_WATCH_MAX_REFS
# well above the suite's branch count (docker-compose.e2e.yml). Do not reuse an old branch
# here without keeping that cap high enough to still watch it.
say "PRD #966 M6: mr_rework run kind (review-landed rework on a settled MR)"

AGENTBRANCH="agent/issue-$IID"

# --- 1) enable mr_rework (idempotent; default-on, re-applied for subset-soundness) ---
apiput /api/admin/settings '{"settings":{"mr_rework_enabled":"true"}}' >/dev/null
pass "mr_rework_enabled=true (admin kill-switch on)"

# --- 2) re-apply the fast poller IN-PHASE (subset-soundness) ------------------
# The MR review-watcher only ticks inside the poller; the overlay default is 24h.
# Without a fast forge tick there is no candidate AT ALL: ListMRReworkCandidates gates
# on runs.mr_state='opened', which SyncMRStates stamps on a poll tick. Declared
# mutates:/provides: env: so a subset run (E2E_ONLY) is sound.
printf 'E2E_FORGE_POLL_INTERVAL=2s\nFORGE_RECONCILE_EVERY=2\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login
pass "forge poller sped to ~2s (SyncMRStates + SyncPipelines tick)"

# --- 3) POSITIVE CONTROL: the happy-path MR must be OPEN ----------------------
# phase 27 leaves MR_IID reopened (opened); assert it, and reopen defensively if a
# subset run left it otherwise, so a closed-MR precondition fails in one clear line
# rather than as a mysterious no-candidate timeout.
MRSTATE="$(fake_state | jq -r --argjson iid "$MR_IID" '.mrs[] | select(.iid==$iid) | .state')"
if [ "$MRSTATE" != opened ]; then
  flip_mr "$MR_IID" opened
  MRSTATE="$(fake_state | jq -r --argjson iid "$MR_IID" '.mrs[] | select(.iid==$iid) | .state')"
fi
[ "$MRSTATE" = opened ] || fail "MR !$MR_IID is not open (got '$MRSTATE'); mr_rework has no candidate"
pass "positive control: MR !$MR_IID on $AGENTBRANCH is open"

# --- 4) GATE 1: green head pipeline on the agent branch -----------------------
fake_post "/_e2e/pipelines" "{\"ref\":\"$AGENTBRANCH\",\"status\":\"success\"}" >/dev/null
wait_card_pipeline "$IID" success 20
pass "GATE 1: green pipeline on $AGENTBRANCH visible on card #$IID"

# now-60s, RFC3339: > the 2s quiet period so GATE 2's debounce passes. GNU date, then
# BSD date fallback (matching phase 61's +1h idiom).
OLD_TS="$(date -u -d '-60 seconds' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-60S +%Y-%m-%dT%H:%M:%SZ)"

# active-uniqueness (gate control c): run list must never show two non-terminal
# mr_rework runs on MR_IID (backed by uq_runs_one_active_mr_rework). Asserted at every
# poll below. Fed the current `run list --json` on stdin so one CLI call serves both
# the id lookup and this invariant.
active_mrr() { jq --argjson mr "$MR_IID" \
  '[.[] | select(.kind=="mr_rework" and .mr_iid==$mr and (.status|IN("completed","failed","cancelled")|not))] | length'; }
count_mrr()  { jq --argjson mr "$MR_IID" \
  '[.[] | select(.kind=="mr_rework" and .mr_iid==$mr)] | length'; }

# --- 5) FIRST FIRE: inject a top-level note above the (zero) high-water -------
BEFORE_COMMITS="$(git --git-dir="$RUNROOT/fakeremote/repo.git" rev-list --count "refs/heads/$AGENTBRANCH")"
fake_post "/_e2e/mrs/$MR_IID/discussions" \
  "{\"id\":5001,\"author_id\":2,\"body\":\"please rework\",\"created_at\":\"$OLD_TS\",\"system\":false}" >/dev/null
RUN_MRR=""
start=$SECONDS
while [ $((SECONDS - start)) -lt 60 ]; do
  LIST="$(uzi_cli run list --json)" || fail "uzi run list --json failed (exit $?)"
  [ "$(printf '%s' "$LIST" | active_mrr)" -le 1 ] \
    || fail "gate(c): two active mr_rework runs on MR !$MR_IID (uq_runs_one_active_mr_rework)"
  RUN_MRR="$(printf '%s' "$LIST" | jq -r --argjson mr "$MR_IID" \
    'first(.[] | select(.kind=="mr_rework" and .mr_iid==$mr) | .id) // empty')"
  [ -n "$RUN_MRR" ] && break
  sleep 0.5
done
[ -n "$RUN_MRR" ] || fail "no mr_rework run appeared for MR !$MR_IID within 60s (first fire)"
record_margin "mr_rework first fire (mr !$MR_IID)" "$((SECONDS - start))" 60
# Assert the kind from the wire (CLI scalar), and that issue_iid is NULL (mr_rework
# carries mr_iid, never issue_iid).
[ "$(uzi_cli run get "$RUN_MRR" --field kind)" = mr_rework ] \
  || fail "run $RUN_MRR kind is not mr_rework"
[ "$(apiget "/api/runs/$RUN_MRR" | jq -r '.run.issue_iid')" = null ] \
  || fail "mr_rework run $RUN_MRR must carry a NULL issue_iid"
wait_status "$RUN_MRR" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
AFTER_COMMITS="$(git --git-dir="$RUNROOT/fakeremote/repo.git" rev-list --count "refs/heads/$AGENTBRANCH")"
[ "$AFTER_COMMITS" = "$((BEFORE_COMMITS + 1))" ] \
  || fail "mr_rework run must add exactly one commit on $AGENTBRANCH ($BEFORE_COMMITS -> $AFTER_COMMITS)"
pass "first fire: mr_rework run $RUN_MRR completed, +1 commit on $AGENTBRANCH ($BEFORE_COMMITS -> $AFTER_COMMITS)"

# --- 6) GATE CONTROL (a): id <= high-water -> NO fire (id-gate) ---------------
# The first fire is terminal, so a second fire here would prove a FRESH id-gate miss,
# not the active-run uniqueness. Inject a note at id 5000 (<= the consumed high-water
# 5001), old enough that GATE 2's debounce is not what blocks it.
BASE_COUNT="$(uzi_cli run list --json | count_mrr)"
fake_post "/_e2e/mrs/$MR_IID/discussions" \
  "{\"id\":5000,\"author_id\":2,\"body\":\"stale below hw\",\"created_at\":\"$OLD_TS\",\"system\":false}" >/dev/null
# Negative window: >= 3 poll ticks (~6s), above the 2-tick floor (PRD #97 Decision 5)
# for a 2s poll. Assert active-uniqueness each tick.
start=$SECONDS
while [ $((SECONDS - start)) -lt 8 ]; do
  [ "$(uzi_cli run list --json | active_mrr)" -le 1 ] \
    || fail "gate(c): two active mr_rework runs on MR !$MR_IID during the id<=hw window"
  sleep 0.5
done
record_margin "mr_rework id<=hw negative window (mr !$MR_IID)" "$((SECONDS - start))" 8
AFTER_A="$(uzi_cli run list --json | count_mrr)"
[ "$AFTER_A" = "$BASE_COUNT" ] \
  || fail "id-gate: a note at id 5000 (<= high-water 5001) fired a run ($BASE_COUNT -> $AFTER_A)"
pass "gate control (a): a note at/below the high-water fired no run ($BASE_COUNT mr_rework run(s), unchanged)"

# --- 7) GATE CONTROL (b): id > high-water -> FIRE (watcher still live) --------
fake_post "/_e2e/mrs/$MR_IID/discussions" \
  "{\"id\":5002,\"author_id\":2,\"body\":\"rework again\",\"created_at\":\"$OLD_TS\",\"system\":false}" >/dev/null
RUN_MRR2=""
start=$SECONDS
while [ $((SECONDS - start)) -lt 60 ]; do
  LIST="$(uzi_cli run list --json)" || fail "uzi run list --json failed (exit $?)"
  [ "$(printf '%s' "$LIST" | active_mrr)" -le 1 ] \
    || fail "gate(c): two active mr_rework runs on MR !$MR_IID (second fire)"
  RUN_MRR2="$(printf '%s' "$LIST" | jq -r --arg ex "$RUN_MRR" --argjson mr "$MR_IID" \
    'first(.[] | select(.kind=="mr_rework" and .mr_iid==$mr and .id != $ex) | .id) // empty')"
  [ -n "$RUN_MRR2" ] && break
  sleep 0.5
done
[ -n "$RUN_MRR2" ] || fail "no SECOND mr_rework run appeared for MR !$MR_IID (id>high-water should fire)"
record_margin "mr_rework second fire (mr !$MR_IID)" "$((SECONDS - start))" 60
[ "$(uzi_cli run get "$RUN_MRR2" --field kind)" = mr_rework ] \
  || fail "run $RUN_MRR2 kind is not mr_rework"
wait_status "$RUN_MRR2" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "gate control (b): a note above the high-water fired a second run $RUN_MRR2 (watcher still live), completed"

# --- 8/9) cleanup: every mr_rework run on MR_IID is terminal (quarantine-clean) ---
[ "$(uzi_cli run list --json | active_mrr)" = 0 ] \
  || fail "an mr_rework run on MR !$MR_IID is still non-terminal at phase end"
pass "all mr_rework runs on MR !$MR_IID are terminal (quarantine-clean; mr_rework_enabled left on, harmless)"
