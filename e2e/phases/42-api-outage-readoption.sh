# shellcheck shell=bash
# phase:    api-outage-readoption
# title:    PRD #1390: api outage recovery — boot grace, heartbeat re-adoption, claim dedupe
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID UZI_WORKER_TOKEN
# provides: -
# handoff:  -
# mutates:  compose:agent(force-recreated: max_concurrent_runs 2, then UZI_E2E_DROP_ON_SENTINEL for case c); compose:api(restarted for case a, force-recreated with UZI_ACTIVE_SNAPSHOT_DISABLED for case d); a transient second worker registered then deleted; stub runs created
# restores: agent + api force-recreated back to their defaults (cap 1, snapshot enabled, drop seam off); stub runs cancelled best-effort; the transient second worker deleted
# race-sensitive: yes
# =============================================================================
# PRD #1390 M4 — the api-SIDE outage recovery this branch has landed:
#   M1  boot grace           — at api start the three stale-worker passes are skipped
#                              for SWEEPER_BOOT_GRACE (default 60s) after the listener is
#                              ready, so an api restart under live runs does NOT requeue them.
#   M2a/M2b snapshot + heartbeat reconciliation — the worker reports the run-lane attempts it
#                              is executing ({run_id, claim_generation, phase}) on every
#                              heartbeat; a stale-requeued run listed by a fresh snapshot at its
#                              generation is RE-ADOPTED to its exact phase (requeue refunded,
#                              park time banked), and a run-lane `running` run the worker STOPS
#                              listing (a silent execution loss) is requeued after the fence.
#   M3  claim dedupe          — ClaimRun never returns a run a fresh snapshot lists; a re-adopted
#                              run advances its generation by exactly one per reclaim.
#
# FOUR CASES, driven against the compose stack (executor: stub, lane: gitlab). The two
# "setup" runs of the PRD (one kept EXECUTING, one PARKED at the plan gate) are minted
# per case by make_hold_run / make_gate_run so a case's timing never has to outlast the
# stub's hold. The stub sentinels used:
#   UZI_STUB_HOLD  — keeps a run genuinely `running` (stalled suppressed, abort-aware) across
#                    the whole outage + reconciliation window (agent/src/executor.ts).
#   UZI_STUB_DROP  — with the env-gated seam UZI_E2E_DROP_ON_SENTINEL=1, makes the worker END
#                    one execution locally with NO terminal report and pause its claim loop
#                    (agent/src/runner.ts), modelling a live worker that silently loses a run.
#
# CASE ORDER is (b), (a), (c), (d), NOT the PRD's lettering: case (b)'s LIVE stale requeue must
# fire, which needs the boot grace to have elapsed — trivially true at phase start (the api has
# been up since the harness boot) but NOT right after case (a) restarts the api. Running (b)
# first, then the api-restart case (a), keeps every case's precondition honest. Each case is
# labelled with the PRD milestone it proves.
say "PRD #1390 M4: api outage recovery — boot grace, heartbeat re-adoption, claim dedupe"
if [ "$EXECUTOR" != stub ]; then
  say "PRD #1390 readoption scenario: SKIPPED (stub-only — UZI_STUB_HOLD/UZI_STUB_DROP are stub sentinels; executor=$EXECUTOR)"
else

# --- helpers -----------------------------------------------------------------
# run_field RUN COL — a scalar runs column straight from the db (COL is a fixed literal
# from THIS file, never user input). Empty string for a SQL NULL.
run_field() { db_psql "SELECT $2 FROM runs WHERE id = '$1'"; }
# custody_open RUN — count of OPEN recovery custody holds for a run (the "no new hold" probe).
custody_open() { db_psql "SELECT count(*) FROM recovery_custody_holds WHERE run_id = '$1' AND state = 'open'"; }
# war_lists WORKER RUN — "1" if the worker_active_runs snapshot table lists RUN for WORKER.
war_lists() { db_psql "SELECT count(*) FROM worker_active_runs WHERE worker_id = '$1' AND run_id = '$2'"; }
# cancel_run RUN — best-effort server-side cancel (frees the worker slot; the HOLD sentinel is
# abort-aware, so a cancel unwinds it promptly). Never reddens a passed case.
cancel_run() { apipost "/api/runs/$1/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true; }

# make_hold_run — create + approve a UZI_STUB_HOLD stub run and leave it genuinely `running`
# (the stub holds one tool call open, so `stalled` is suppressed). Leaves the id in HOLD_RUN
# (a GLOBAL, so a `fail` in here runs in the current shell — the 50/51/52 idiom).
make_hold_run() {
  local iid
  iid="$(apipost "/api/repos/$REPO_ID/issues" \
    '{"title":"E2E readopt HOLD (running)","description":"implements prds/4-agent-runtime-workers.md UZI_STUB_HOLD"}' \
    | jq -r '.card.iid')"
  { [ -n "$iid" ] && [ "$iid" != null ]; } || fail "readopt: could not create the HOLD issue"
  HOLD_RUN="$(create_run "$REPO_ID" "$iid")" || fail "readopt: HOLD run-create failed (non-transient; see stderr)"
  { [ -n "$HOLD_RUN" ] && [ "$HOLD_RUN" != null ]; } || fail "readopt: HOLD run was not created"
  wait_status "$HOLD_RUN" awaiting_approval
  apipost "/api/runs/$HOLD_RUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  wait_status "$HOLD_RUN" running 60
}
# make_gate_run — create a plain stub run and leave it PARKED at the plan gate
# (awaiting_approval): its execute promise stays live, so the worker keeps listing it in its
# snapshot at that held phase. Leaves the id in GATE_RUN (a GLOBAL, same reason as above).
make_gate_run() {
  local iid
  iid="$(apipost "/api/repos/$REPO_ID/issues" \
    '{"title":"E2E readopt GATE (awaiting_approval)","description":"implements prds/4-agent-runtime-workers.md"}' \
    | jq -r '.card.iid')"
  { [ -n "$iid" ] && [ "$iid" != null ]; } || fail "readopt: could not create the GATE issue"
  GATE_RUN="$(create_run "$REPO_ID" "$iid")" || fail "readopt: GATE run-create failed (non-transient; see stderr)"
  { [ -n "$GATE_RUN" ] && [ "$GATE_RUN" != null ]; } || fail "readopt: GATE run was not created"
  wait_status "$GATE_RUN" awaiting_approval
}

# --- recreate the agent at max_concurrent_runs 2 (fact 1); restored at the end -------
# Exported so it out-ranks the overlay's ${UZI_E2E_MAX_CONCURRENT_RUNS:-1} default, exactly as
# phase 45 sets it (via the env-file) / phase 52 exports E2E_WORKER_HEARTBEAT_STALE.
export UZI_E2E_MAX_CONCURRENT_RUNS=2
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
# Wait for the RECREATED worker's registration to actually LAND its advertised cap — not
# merely for `online`, which `wait_worker_online` reads off `.workers[0].status`. The
# recreated container reuses the join token and re-registers into the SAME row, so that
# row still reads `online` at the OLD cap until the fresh register overwrites
# max_concurrent_runs; a single read here is exactly the race phases 45 and 46 already
# guard with this loop. Without it the whole phase failed 0.35s after compose printed
# `Started`, before the new agent process had registered at all (2026-09-19 nightly).
cap_deadline=$((SECONDS + 40)); CAP=""
while [ $SECONDS -lt $cap_deadline ]; do
  CAP="$(apiget /api/workers | jq -r '[.workers[] | select(.status=="online")][0].max_concurrent_runs')"
  [ "$CAP" = 2 ] && break
  sleep 0.3
done
[ "$CAP" = 2 ] || fail "readopt: worker did not advertise max_concurrent_runs=2 after recreate (got ${CAP:-none})"
pass "agent recreated at max_concurrent_runs=2"

# Clear the admin owner's accumulated cross-phase recovery custody holds so the claim
# admission gate does not wedge our claims in `queued` (the same guard phases 46/52 apply). A
# CTE keeps the TOP-LEVEL statement a SELECT so the scalar read is a bare count.
RA_ADMIN_ID="$(db_psql "SELECT id FROM users WHERE email = '$ADMIN_EMAIL'")"
[ -n "$RA_ADMIN_ID" ] || fail "readopt: could not resolve the admin owner id for '$ADMIN_EMAIL'"
RA_CLEARED="$(db_psql "WITH del AS (DELETE FROM recovery_custody_holds
                                    WHERE user_id = '$RA_ADMIN_ID' AND state = 'open'
                                    RETURNING 1)
                      SELECT count(*) FROM del")"
pass "cleared ${RA_CLEARED:-0} accumulated custody hold(s) so the admission gate does not wedge our claims"

# =============================================================================
# CASE (b) — LIVE stale requeue + heartbeat re-adoption (proves M2a/M2b/M2c).
# The api stays UP; only the agent is partitioned off the network for longer than the stale
# window, so the sweeper marks the worker offline and requeues both runs (the stale-worker
# path). On reconnect the very next heartbeat carries the snapshot and RE-ADOPTS each run to
# its exact phase, refunding the requeue and banking the queued interval for the gate run.
say "CASE (b): api up, partition ONLY the agent > the stale window → stale requeue → re-adopt on reconnect (M2a/M2b/M2c)"
make_hold_run;  E="$HOLD_RUN"
make_gate_run;  G="$GATE_RUN"
WORKER1="$(run_field "$E" worker_id)"
[ -n "$WORKER1" ] || fail "case b: the HOLD run has no worker_id"
[ "$(run_field "$G" worker_id)" = "$WORKER1" ] || fail "case b: the two runs are not on the same worker"
GEN_E="$(run_field "$E" claim_generation)"
GEN_G="$(run_field "$G" claim_generation)"
BP_G_BEFORE="$(run_field "$G" budget_paused_seconds)"
CUST_E_BEFORE="$(custody_open "$E")"
CUST_G_BEFORE="$(custody_open "$G")"
pass "case b: HOLD run $E running, GATE run $G at the plan gate, both on worker $WORKER1 (gen E=$GEN_E G=$GEN_G)"

# Resolve the agent container + its compose network so we can cut ONLY the agent (the api and
# db stay reachable to the harness). The container id is stable across a network disconnect.
AGENT_CID="$("${COMPOSE[@]}" ps -q agent)"
[ -n "$AGENT_CID" ] || fail "case b: could not resolve the agent container id"
AGENT_NET="$(docker inspect -f '{{json .NetworkSettings.Networks}}' "$AGENT_CID" | jq -r 'keys[0] // empty')"
[ -n "$AGENT_NET" ] || fail "case b: could not resolve the agent container network"

say "partitioning the agent off the network ($AGENT_NET) for ~22s (> the 15s stale window) — api stays up"
docker network disconnect "$AGENT_NET" "$AGENT_CID" >/dev/null 2>&1 \
  || fail "case b: could not disconnect the agent from its network"
# Poll (against the still-up api's db) for the stale-worker requeue. Both runs go queued.
wait_status "$E" queued 45
wait_status "$G" queued 45
# The stale requeue charged both and stamped provenance; the gate run's PARK interval was
# banked (D2/D5). requeue_count 0->1, stale_requeue_generation == claim_generation.
[ "$(run_field "$E" requeue_count)" = 1 ] || fail "case b: HOLD run requeue_count != 1 after the stale requeue (got $(run_field "$E" requeue_count))"
[ "$(run_field "$G" requeue_count)" = 1 ] || fail "case b: GATE run requeue_count != 1 after the stale requeue (got $(run_field "$G" requeue_count))"
[ "$(run_field "$E" stale_requeue_generation)" = "$GEN_E" ] || fail "case b: HOLD run stale_requeue_generation != claim_generation ($GEN_E)"
[ "$(run_field "$G" stale_requeue_generation)" = "$GEN_G" ] || fail "case b: GATE run stale_requeue_generation != claim_generation ($GEN_G)"
BP_G_REQ="$(run_field "$G" budget_paused_seconds)"
[ "$BP_G_REQ" -gt "$BP_G_BEFORE" ] || fail "case b: gate run budget_paused_seconds did not grow on the stale requeue ($BP_G_BEFORE -> $BP_G_REQ) — RequeueRunsOfStaleWorkers must bank the gate run's awaiting_approval park interval"
pass "case b: stale requeue landed — both charged (requeue_count=1), stale_requeue_generation set, gate park time banked ($BP_G_BEFORE -> $BP_G_REQ)"

# Reconnect the agent. The api is already up, so restore the agent AFTER (ordering: the api was
# never down here, so there is nothing to restore before it). The worker resumes heartbeating
# on its own — no re-register, no re-login — and the next heartbeat's snapshot re-adopts.
say "reconnecting the agent → the next heartbeat's snapshot re-adopts both runs to their exact phase"
docker network connect "$AGENT_NET" "$AGENT_CID" >/dev/null 2>&1 \
  || fail "case b: could not reconnect the agent to its network"
# Within one heartbeat each run is back in its EXACT phase.
wait_status "$E" running 30
wait_status "$G" awaiting_approval 30
# requeue REFUNDED (matching provenance), stale_requeue_generation CLEARED, generation UNCHANGED.
[ "$(run_field "$E" requeue_count)" = 0 ] || fail "case b: HOLD run requeue_count not refunded to 0 (got $(run_field "$E" requeue_count))"
[ "$(run_field "$G" requeue_count)" = 0 ] || fail "case b: GATE run requeue_count not refunded to 0 (got $(run_field "$G" requeue_count))"
[ -z "$(run_field "$E" stale_requeue_generation)" ] || fail "case b: HOLD run stale_requeue_generation not cleared (got $(run_field "$E" stale_requeue_generation))"
[ -z "$(run_field "$G" stale_requeue_generation)" ] || fail "case b: GATE run stale_requeue_generation not cleared (got $(run_field "$G" stale_requeue_generation))"
[ "$(run_field "$E" claim_generation)" = "$GEN_E" ] || fail "case b: HOLD run generation changed across re-adoption (re-adopt must not re-claim)"
[ "$(run_field "$G" claim_generation)" = "$GEN_G" ] || fail "case b: GATE run generation changed across re-adoption (re-adopt must not re-claim)"
# The gate run's queued interval banked into budget_paused_seconds (it GREW again on re-adopt).
BP_G_AFTER="$(run_field "$G" budget_paused_seconds)"
[ "$BP_G_AFTER" -gt "$BP_G_REQ" ] || fail "case b: the re-adopt did not bank the queued interval on top of the requeue's park bank (D5) ($BP_G_REQ -> $BP_G_AFTER)"
# No NEW custody hold from re-adoption (re-adopt leaves custody untouched, D6).
[ "$(custody_open "$E")" = "$CUST_E_BEFORE" ] || fail "case b: HOLD run gained a custody hold across re-adoption (was $CUST_E_BEFORE, now $(custody_open "$E"))"
[ "$(custody_open "$G")" = "$CUST_G_BEFORE" ] || fail "case b: GATE run gained a custody hold across re-adoption (was $CUST_G_BEFORE, now $(custody_open "$G"))"
# The worker's snapshot table lists BOTH runs again.
[ "$(war_lists "$WORKER1" "$E")" = 1 ] || fail "case b: worker_active_runs does not list the HOLD run after re-adoption"
[ "$(war_lists "$WORKER1" "$G")" = 1 ] || fail "case b: worker_active_runs does not list the GATE run after re-adoption"
# And the operator sees them on `uzi admin workers` (M2c reported_runs).
apiget /api/admin/workers \
  | jq -e --arg w "$WORKER1" --arg e "$E" --arg g "$G" \
      '[.workers[] | select(.id==$w) | .reported_runs[].run_id] as $rr | (($rr | index($e)) != null) and (($rr | index($g)) != null)' >/dev/null \
  || fail "case b: uzi admin workers did not report both runs for worker $WORKER1"
pass "case b: re-adopted to exact phase — requeue refunded, provenance cleared, gen unchanged, gate queued-interval banked ($BP_G_BEFORE -> $BP_G_AFTER), no new custody hold, worker_active_runs + admin workers list both"
cancel_run "$E"; cancel_run "$G"

# =============================================================================
# CASE (a) — BOOT GRACE (proves M1). The api is stopped under both live runs for longer than
# the stale window and started again. At boot the sweep runs (the worker's last heartbeat is
# now stale), but the three stale-worker passes are SKIPPED for SWEEPER_BOOT_GRACE, so NEITHER
# run is requeued. The worker reconnects within a heartbeat and both read their ORIGINAL status.
say "CASE (a): stop the api under both runs > the stale window, start it → NO requeue inside the boot grace (M1)"
make_hold_run;  EA="$HOLD_RUN"
make_gate_run;  GA="$GATE_RUN"
# The durable boot-grace discriminator is the GATE run's budget_paused_seconds: a broken grace
# would stale-requeue it at boot (banking its park interval) and only THEN re-adopt it, so the
# park time would GROW even though the requeue is later refunded. With the grace it is NEVER
# requeued, so it stays put. Capture the baseline before the outage.
BP_GA_BEFORE="$(run_field "$GA" budget_paused_seconds)"
pass "case a: HOLD run $EA running, GATE run $GA at the plan gate (gate budget baseline=$BP_GA_BEFORE)"

say "stopping the api for ~20s (> the 15s stale window) with both runs live"
"${COMPOSE[@]}" stop api >/dev/null 2>&1
sleep 20
"${COMPOSE[@]}" up -d --wait api >/dev/null
wait_http
login
# Give the worker a heartbeat cycle to reconnect, then assert nothing was requeued INSIDE the
# grace: both read their original status, neither was charged, and the gate run's park time did
# NOT move (the durable proof that the boot sweep's stale passes were skipped).
wait_status "$EA" running 30
wait_status "$GA" awaiting_approval 30
[ "$(run_field "$EA" requeue_count)" = 0 ] || fail "case a: HOLD run was requeued inside the boot grace (requeue_count=$(run_field "$EA" requeue_count))"
[ "$(run_field "$GA" requeue_count)" = 0 ] || fail "case a: GATE run was requeued inside the boot grace (requeue_count=$(run_field "$GA" requeue_count))"
[ -z "$(run_field "$EA" stale_requeue_generation)" ] || fail "case a: HOLD run carries a stale_requeue_generation (a stale requeue fired inside the grace)"
[ -z "$(run_field "$GA" stale_requeue_generation)" ] || fail "case a: GATE run carries a stale_requeue_generation (a stale requeue fired inside the grace)"
BP_GA_AFTER="$(run_field "$GA" budget_paused_seconds)"
[ "$BP_GA_AFTER" = "$BP_GA_BEFORE" ] || fail "case a: GATE run park time moved across the restart ($BP_GA_BEFORE -> $BP_GA_AFTER) — a stale requeue fired inside the boot grace"
pass "case a: boot grace held — both runs read their ORIGINAL status, no requeue, no provenance, gate park time unchanged ($BP_GA_BEFORE)"
cancel_run "$EA"; cancel_run "$GA"

# =============================================================================
# CASE (c) — silent execution loss (missing path) + no sibling steal + one reclaim (M2b/M3).
# With the api + heartbeat ALIVE, the env-gated drop seam makes the worker END one execution
# locally with NO terminal report and pause its claim loop. The run stays `running`; it is
# requeued only AFTER the missing-run fence (stale window + one heartbeat interval); a second
# authenticated worker cannot steal it; and resuming the first worker's claim loop reclaims it
# exactly once (generation +1).
say "CASE (c): a live worker silently loses one execution → requeued after the fence, no sibling steal, exactly one reclaim (M2b missing path + M3)"
# Arm the seam and recreate the agent (fresh worker under the drop env; still cap 2). Any
# leftover queued run would confuse the claim, so make sure only run X exists.
export UZI_E2E_DROP_ON_SENTINEL=1
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
pass "case c: agent recreated with the drop seam armed (UZI_E2E_DROP_ON_SENTINEL=1)"

XIID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E readopt DROP","description":"implements prds/4-agent-runtime-workers.md UZI_STUB_DROP"}' \
  | jq -r '.card.iid')"
{ [ -n "$XIID" ] && [ "$XIID" != null ]; } || fail "case c: could not create the DROP issue"
X="$(create_run "$REPO_ID" "$XIID")" || fail "case c: DROP run-create failed (non-transient; see stderr)"
{ [ -n "$X" ] && [ "$X" != null ]; } || fail "case c: DROP run was not created"
# The seam ends the flight right after the first `running` report (before the clone), so X
# reports running and then the worker silently drops it — X stays `running` with no terminal
# report and the claim loop is paused.
wait_status "$X" running 60
GEN_X0="$(run_field "$X" claim_generation)"
WORKER_C="$(run_field "$X" worker_id)"
DROP_T=$SECONDS
[ "$(run_field "$X" requeue_count)" = 0 ] || fail "case c: X already charged a requeue at the drop (got $(run_field "$X" requeue_count))"
pass "case c: run $X reported running then was silently dropped (gen=$GEN_X0, worker=$WORKER_C); claim loop paused"

# NOT before the fence. The missing-run fence is the stale window (15s) + one heartbeat interval
# (the api's 15s default) = ~30s; well under it, X must still be `running` and uncharged.
sleep 18
[ "$(run_field "$X" status)" = running ] || fail "case c: X was requeued BEFORE the fence (status=$(run_field "$X" status) after $((SECONDS - DROP_T))s)"
[ "$(run_field "$X" requeue_count)" = 0 ] || fail "case c: X was charged a requeue before the fence"
pass "case c: X still running $((SECONDS - DROP_T))s after the drop — not requeued before the fence"

# AFTER the fence the heartbeat's missing-run reconciliation requeues X: requeue_count +1, and
# the generation is UNCHANGED while it sits `queued` (a requeue never advances the generation).
wait_status "$X" queued 60
Q_ELAPSED=$((SECONDS - DROP_T))
[ "$Q_ELAPSED" -ge 25 ] || fail "case c: X requeued too early ($Q_ELAPSED s < ~30s fence)"
[ "$(run_field "$X" requeue_count)" = 1 ] || fail "case c: X requeue_count != 1 after the missing-run requeue (got $(run_field "$X" requeue_count))"
[ "$(run_field "$X" claim_generation)" = "$GEN_X0" ] || fail "case c: X generation changed while queued (a requeue must not re-claim)"
pass "case c: X requeued after the fence (${Q_ELAPSED}s) — requeue_count=1, generation unchanged ($GEN_X0)"

# Precondition for the sibling probes below: X must be the ONLY claimable (queued) run for this
# owner/repo. Claiming is user-scoped (idx_runs_claimable ON runs(user_id, status, created_at)),
# so a leftover queued run would be a legitimate pick for the sibling's probe and would falsely
# trip the "a sibling stole the requeued run" check. Enforce it explicitly (it was only prose).
CLAIMABLE_N="$(db_psql "SELECT count(*) FROM runs WHERE user_id = '$RA_ADMIN_ID' AND repo_id = '$REPO_ID' AND status = 'queued'")"
CLAIMABLE_X="$(db_psql "SELECT count(*) FROM runs WHERE user_id = '$RA_ADMIN_ID' AND repo_id = '$REPO_ID' AND status = 'queued' AND id = '$X'")"
{ [ "$CLAIMABLE_N" = 1 ] && [ "$CLAIMABLE_X" = 1 ]; } || fail "case c: precondition broken — the only claimable (queued) run for the owner/repo must be X, found ${CLAIMABLE_N:-0} queued run(s) (X queued=${CLAIMABLE_X:-0}); a leftover claimable run would let a probe legitimately claim it and falsely trip the sibling-steal check"
pass "case c: X is the sole claimable (queued) run for the owner/repo before the sibling probes"

# A SECOND authenticated worker (join token minted like phase 13) makes one-shot claim probes
# while X is queued and the first worker's claim loop is paused. It must receive NOTHING (X is
# held to worker $WORKER_C by affinity, and no fresh snapshot lists it for a sibling); it is
# recorded then deleted while idle.
SIB_TOKEN="$(apipost /api/workers '{"name":"e2e-readopt-sibling"}' | jq -r '.token')"
{ [ -n "$SIB_TOKEN" ] && [ "$SIB_TOKEN" != null ]; } || fail "case c: could not mint a second worker join token"
SIB_ID="$(curl -fsS -H "Authorization: Bearer $SIB_TOKEN" -X POST "$BASE/api/worker/register" \
  -H 'Content-Type: application/json' -d '{"name":"e2e-readopt-sibling","version":"0.0.0-e2e"}' | jq -r '.worker_id')"
{ [ -n "$SIB_ID" ] && [ "$SIB_ID" != null ]; } || fail "case c: the second worker did not register"
[ "$SIB_ID" != "$WORKER_C" ] || fail "case c: the second worker reused the first worker's id"
for _ in 1 2 3; do
  code="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $SIB_TOKEN" \
    -X POST "$BASE/api/worker/runs/claim" -H 'Content-Type: application/json' -d '{}')"
  [ "$code" = 204 ] || fail "case c: the second worker's claim probe returned $code (want 204 no-content) — a sibling stole the requeued run"
  sleep 0.3
done
# X is unmoved by the sibling: still queued, generation still unchanged.
[ "$(run_field "$X" status)" = queued ] || fail "case c: X left queued by a sibling probe (status=$(run_field "$X" status))"
[ "$(run_field "$X" claim_generation)" = "$GEN_X0" ] || fail "case c: X generation advanced during the sibling probes"
# Delete the idle second worker (it claimed nothing, so it has no active runs).
curl -fsS -b "$JAR" -X DELETE "$BASE/api/workers/$SIB_ID" -H "X-CSRF-Token: $(csrf)" >/dev/null \
  || fail "case c: could not delete the idle second worker"
pass "case c: second worker got NOTHING on every probe and was deleted while idle; X still queued at gen $GEN_X0"

# Resume EXACTLY ONE claimant — the first worker's claim loop — by recreating the agent WITHOUT
# the drop seam (a fresh process clears the pause latch and the env). It reclaims X by affinity
# exactly once: the generation advances by exactly one.
unset UZI_E2E_DROP_ON_SENTINEL
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
wait_status "$X" awaiting_approval 90
GEN_X1="$(run_field "$X" claim_generation)"
[ "$GEN_X1" = "$((GEN_X0 + 1))" ] || fail "case c: reclaim did not advance the generation by exactly one ($GEN_X0 -> $GEN_X1)"
pass "case c: first worker's claim loop reclaimed X exactly once — generation $GEN_X0 -> $GEN_X1"
cancel_run "$X"

# =============================================================================
# CASE (d) — api ROLLBACK simulation (proves D7). Restart the api with
# UZI_ACTIVE_SNAPSHOT_DISABLED=1: it drops active_run_snapshot from protocol_features and
# strict-decodes heartbeats, so a worker that (from its one-shot register under the ENABLED api)
# still sends the field 400s ONCE, strips it, retries, and clears its cached feature set. The
# GUARANTEE is that liveness is never lost — assert SUSTAINED heartbeat health rather than a
# brittle "exactly one 400" (the phase does not control heartbeat/claim concurrency).
say "CASE (d): restart the api with UZI_ACTIVE_SNAPSHOT_DISABLED=1 → the worker strips the field and keeps heartbeating (D7)"
# worker_hb_epoch WORKER — the worker's last_heartbeat_at as a whole-second epoch (a
# monotonically advancing liveness clock), or empty when the row/heartbeat is absent.
worker_hb_epoch() { db_psql "SELECT COALESCE(extract(epoch FROM last_heartbeat_at)::bigint::text, '') FROM workers WHERE id = '$1'"; }
worker_db_status() { db_psql "SELECT status FROM workers WHERE id = '$1'"; }

# A live run so the worker actually has a snapshot to send across the rollback.
make_hold_run;  ED="$HOLD_RUN"
WORKER_D="$(run_field "$ED" worker_id)"
export UZI_ACTIVE_SNAPSHOT_DISABLED=1
"${COMPOSE[@]}" up -d --wait --no-deps --force-recreate api >/dev/null
wait_http
login
# The rolled-back api now omits active_run_snapshot from protocol_features and strict-decodes
# heartbeats. The worker (still sending the field from its enabled-api register) 400s once, strips
# it, retries, and clears its feature set — so first let it RECOVER to online (never re-registered),
# then prove liveness is SUSTAINED by watching its heartbeat clock keep ADVANCING (a lost heartbeat
# would freeze it). This is the D7 guarantee, not a brittle "exactly one 400".
D_END=$((SECONDS + 40))
until [ "$(worker_db_status "$WORKER_D")" = online ]; do
  [ "$SECONDS" -lt "$D_END" ] || fail "case d: the worker never came back online after the rollback (heartbeats lost to the stripped field, not retried)"
  sleep 1
done
HB1="$(worker_hb_epoch "$WORKER_D")"
[ -n "$HB1" ] || fail "case d: the worker has no last_heartbeat_at after the rollback"
# Watch several heartbeat cycles (agent interval 5s): the clock must advance while it stays online.
D_END=$((SECONDS + 30)); HB2="$HB1"
until { [ "$HB2" != "$HB1" ] && [ "$(worker_db_status "$WORKER_D")" = online ]; }; do
  [ "$SECONDS" -lt "$D_END" ] || fail "case d: the worker's heartbeat clock did not advance after the rollback (HB $HB1 -> $HB2) — a heartbeat was lost to the stripped field"
  sleep 2
  HB2="$(worker_hb_epoch "$WORKER_D")"
done
[ "$(worker_db_status "$WORKER_D")" = online ] || fail "case d: the worker went offline across the rollback observation"
pass "case d: worker kept heartbeating across the rollback (online, heartbeat clock advanced $HB1 -> $HB2) — the stripped-field retry preserved liveness"
cancel_run "$ED"

# =============================================================================
# RESTORE — return the agent + api to their defaults so later phases (43+) are unaffected.
# Order: restore the api first (it was force-recreated with the disabled flag), then the agent.
say "restore: recreate api + agent back to their defaults (snapshot enabled, cap 1, drop seam off)"
unset UZI_ACTIVE_SNAPSHOT_DISABLED UZI_E2E_MAX_CONCURRENT_RUNS UZI_E2E_DROP_ON_SENTINEL
"${COMPOSE[@]}" up -d --wait --no-deps --force-recreate api >/dev/null
wait_http
login
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
pass "api + agent recreated back to their defaults"

# Best-effort cleanup of any run this phase left non-terminal (a cancel of a terminal run is a
# no-op, but never let a cleanup blip redden a passed phase).
for r in "$E" "$G" "$EA" "$GA" "$X" "$ED"; do
  [ -n "${r:-}" ] && cancel_run "$r"
done

fi
