# shellcheck shell=bash
# phase:    api-outage-outbox
# title:    PRD #1391: worker outbox survives an api outage (spill+drain, quota tombstones)
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID UZI_WORKER_TOKEN
# provides: -
# handoff:  -
# mutates:  compose:api(force-recreated: heartbeat-stale window raised for the outage), compose:agent(force-recreated: outbox knobs); two stub runs created
# restores: api + agent force-recreated back to their defaults; stub runs cancelled best-effort
# race-sensitive: yes
# =============================================================================
# PRD #1391 M5 (Run A) — the worker message OUTBOX survives an api outage. When the api
# is unreachable for longer than WORKER_TRANSIENT_TRIP_MS the batcher no longer trips
# (no false `failed` report); it SPILLS run messages to a durable on-disk outbox, and a
# per-worker drainer replays them in seq order once the api returns. The stub stands in
# for a live, message-producing run via the UZI_STUB_OUTBOX sentinel (stub-only), which
# streams status frames across the whole outage window so some are produced — and thus
# spilled — while the api is down.
#
# TWO CASES, each with its own agent config and its own outage:
#   1. SPILL + DRAIN (default quotas): assert the FIRST recovered heartbeat shows
#      outbox_pending_messages > 0 — the PRD BARRIER (replay STARTED within one
#      heartbeat, reported BEFORE that heartbeat's drain); the run reaches `completed`
#      with NO transient `failed` report; and every emitted frame lands exactly once in
#      a CONTIGUOUS 1..N seq order (replay COMPLETED after the backlog).
#   2. QUOTA -> TOMBSTONES (tiny WORKER_OUTBOX_RUN_MAX_BYTES): the outage forces the
#      oldest spilled segment to be evicted to one compact range record, which replay
#      expands into one per-seq `message_dropped` tombstone so the stream stays
#      contiguous. The strict tombstone check is guarded observed-only (segment sizing
#      is variable, D2 eviction is soft), but the contiguous-stream invariant it exists
#      to protect is asserted unconditionally, so the guard is never vacuous.
#
# WHY the api heartbeat-stale window is RAISED here: the e2e boots with
# E2E_WORKER_HEARTBEAT_STALE=15s, but each outage cuts the api for >15s, so at recovery
# the worker's last heartbeat is stale and the 2s sweeper would REQUEUE the running run
# before its next heartbeat lands — turning the clean spill+drain into a stale_claim
# retire (D11) and breaking the assertion. Raising the window (api force-recreate) keeps
# the worker leased across the outage; it is restored at the end.
#
# NOTE: this phase runs AFTER 51-auto-stop-poison, which (via 50) left the agent stopped;
# it brings the agent back up itself. 59-restart-agent still runs after and re-ensures it.
say "PRD #1391 M5: worker outbox survives an api outage (spill+drain, quota tombstones)"
if [ "$EXECUTOR" != stub ]; then
  say "PRD #1391 outbox scenario: SKIPPED (stub-only — UZI_STUB_OUTBOX is a stub sentinel; executor=$EXECUTOR)"
else

# --- helpers -----------------------------------------------------------------
# outbox_depth RUN — the /api/workers outbox_pending_messages for the worker that OWNS
# run RUN (0 when the field is null/absent). Selected by the run's worker_id, NOT
# .workers[0]: phases 50/51 left synthetic worker rows in the list, so [0] is not
# reliably the real agent worker.
outbox_depth() {
  local wid
  wid="$(apiget "/api/runs/$1" | jq -r '.run.worker_id // empty')"
  [ -n "$wid" ] || { printf '0'; return 0; }
  apiget /api/workers | jq -r --arg id "$wid" '(.workers[] | select(.id==$id) | .outbox_pending_messages) // 0'
}
# tick_count RUN — how many of the stub's streamed "stub: outbox tick N" frames have
# landed on the run (proves the stream is FLOWING before we cut the api).
tick_count() {
  apiget "/api/runs/$1/messages" \
    | jq '[.messages[] | select((.payload.text? // "") | startswith("stub: outbox tick"))] | length'
}
# observe_barrier RUN — poll until the owning worker reports outbox depth > 0 on a
# recovered heartbeat: the PRD barrier (replay STARTED within one heartbeat, reported
# BEFORE that heartbeat's drain). Fails if never seen within the deadline.
observe_barrier() {
  local run="$1" depth deadline=$((SECONDS + 60))
  while [ "$SECONDS" -lt "$deadline" ]; do
    depth="$(outbox_depth "$run")"
    if [ -n "$depth" ] && [ "$depth" -gt 0 ] 2>/dev/null; then
      pass "outbox depth $depth on the first recovered heartbeat (barrier: replay started, reported before the drain)"
      return 0
    fi
    sleep 0.3
  done
  fail "outbox phase: never observed outbox_pending_messages>0 after recovery for run $run"
}
# wait_drained RUN — poll until the owning worker reports outbox depth 0: the empty
# report that follows a fully replayed backlog (the api re-nulls the DTO field).
wait_drained() {
  local run="$1" depth deadline=$((SECONDS + 120))
  while [ "$SECONDS" -lt "$deadline" ]; do
    depth="$(outbox_depth "$run")"
    [ "$depth" = 0 ] && return 0
    sleep 0.5
  done
  fail "outbox phase: run $run outbox never drained back to 0 (last depth ${depth:-none})"
}
# assert_contiguous RUN — the run's persisted stream is a gapless 1..N sequence. Polled
# briefly so a last-moment persist after the empty report settles. This is what proves
# a range record was expanded PER SEQ (a single-message range replay would leave holes).
assert_contiguous() {
  local run="$1" msgs deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    msgs="$(apiget "/api/runs/$run/messages")"
    if printf '%s' "$msgs" | jq -e '.messages | (length > 0) and (([.[].seq] | sort) == [range(1; length+1)])' >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  fail "outbox phase: run $run stream is not a gapless 1..N sequence after drain: $(printf '%s' "$msgs" | jq -c '[.messages[].seq] | sort')"
}
# make_outbox_run — create + approve a UZI_STUB_OUTBOX stub run and wait until its stream
# is flowing; leave the id in OUTBOX_RUN (a GLOBAL, so a `fail`/`pass` in here runs in the
# current shell and a captured command-substitution can't swallow its exit — the 50/51
# idiom). The caller has already recreated the agent with the case's outbox knobs.
make_outbox_run() {
  local iid n deadline
  iid="$(apipost "/api/repos/$REPO_ID/issues" \
    '{"title":"E2E outbox UZI_STUB_OUTBOX","description":"implements prds/1391-worker-outbox-durable-reports.md UZI_STUB_OUTBOX"}' \
    | jq -r '.card.iid')"
  { [ -n "$iid" ] && [ "$iid" != null ]; } || fail "outbox phase: could not create the stub issue"
  OUTBOX_RUN="$(create_run "$REPO_ID" "$iid")" || fail "outbox phase: run-create failed (non-transient; see stderr)"
  { [ -n "$OUTBOX_RUN" ] && [ "$OUTBOX_RUN" != null ]; } || fail "outbox phase: run was not created"
  wait_status "$OUTBOX_RUN" awaiting_approval
  apipost "/api/runs/$OUTBOX_RUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  wait_status "$OUTBOX_RUN" running 60
  # Wait until the steady stream is FLOWING (>= 3 ticks landed) so cutting the api next
  # guarantees frames are produced DURING the outage.
  deadline=$((SECONDS + 40)); n=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    n="$(tick_count "$OUTBOX_RUN")"
    { [ -n "$n" ] && [ "$n" -ge 3 ] 2>/dev/null; } && break
    sleep 0.5
  done
  { [ -n "$n" ] && [ "$n" -ge 3 ] 2>/dev/null; } \
    || fail "outbox phase: the stub stream never reached 3 ticks (got ${n:-none}) — sentinel not streaming?"
  pass "stub run $OUTBOX_RUN streaming ($n outbox ticks landed) before the outage"
}
# outage RUN SECS — cut the api for SECS (> WORKER_TRANSIENT_TRIP_MS so the batcher
# SPILLS), bring it back, re-login (the bounce drops the session), then assert the
# barrier. No `docker compose start` (no phase uses it): stop, then `up -d --wait api`
# + wait_http + login (the 15-happy-path-restart idiom).
outage() {
  local run="$1" secs="$2"
  say "cutting the api for ${secs}s (> WORKER_TRANSIENT_TRIP_MS=3s) so the batcher spills run $run to the outbox"
  "${COMPOSE[@]}" stop api >/dev/null 2>&1
  sleep "$secs"
  "${COMPOSE[@]}" up -d --wait api >/dev/null
  wait_http
  login
  observe_barrier "$run"
}

# --- raise the api heartbeat-stale window so the outage never requeues the run --------
# Exported so it out-ranks the env-file's E2E_WORKER_HEARTBEAT_STALE=15s (compose ranks
# shell env above --env-file), exactly as phase 46 exports UZI_E2E_MAX_CONCURRENT_RUNS.
export E2E_WORKER_HEARTBEAT_STALE=90s
"${COMPOSE[@]}" up -d --wait --no-deps --force-recreate api >/dev/null
wait_http
login
pass "api recreated with a 90s heartbeat-stale window so the outage cannot requeue the running run"

# Clear the admin owner's accumulated cross-phase recovery custody holds so the claim
# admission gate (workersvc/budget.go: custodyHoldLimit=8) does not wedge our claims in
# 'queued' — the same guard phase 46 applies (the cross-phase accumulation is tracked
# with the custody-recovery work). A CTE keeps the TOP-LEVEL statement a SELECT so the
# scalar read is a bare count, not a welded command tag.
OB_ADMIN_ID="$(db_psql "SELECT id FROM users WHERE email = '$ADMIN_EMAIL'")"
[ -n "$OB_ADMIN_ID" ] || fail "outbox phase: could not resolve the admin owner id for '$ADMIN_EMAIL'"
OB_CLEARED="$(db_psql "WITH del AS (DELETE FROM recovery_custody_holds
                                     WHERE user_id = '$OB_ADMIN_ID' AND state = 'open'
                                     RETURNING 1)
                       SELECT count(*) FROM del")"
pass "cleared ${OB_CLEARED:-0} accumulated custody hold(s) so the admission gate does not wedge the outbox claims"

# =============================================================================
# CASE 1 — spill + drain, contiguous, no drops, no `failed` report.
say "CASE 1: spill + ordered drain (default quotas) — barrier, contiguity, no failed report"
# Lower ONLY the spill trip window; leave the quotas at their defaults so no drop occurs.
export WORKER_TRANSIENT_TRIP_MS=3s
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
pass "agent recreated with WORKER_TRANSIENT_TRIP_MS=3s (default quotas)"

make_outbox_run
RUN1="$OUTBOX_RUN"
outage "$RUN1" 20
wait_status "$RUN1" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
wait_drained "$RUN1"
assert_contiguous "$RUN1"

M1="$(apiget "/api/runs/$RUN1/messages")"
# No drops: default quotas + the small status frames stay under the spill-buffer cap.
printf '%s' "$M1" | jq -e '[.messages[] | select(.kind=="status" and (.payload.event? == "message_dropped"))] | length == 0' >/dev/null \
  || fail "case 1: unexpected message_dropped tombstone(s) — a clean spill+drain must carry none"
# No transient `failed` report (SC1): a permanent-failure trip reaches the run's DTO
# `failure_reason` via reportState (batcher.ts trip() -> onPermanentFailureReport ->
# runner reportState{status:"failed"}), NOT the message stream — so assert on the run
# DTO, not the frames. wait_status already fails on a `failed` transition; this pins
# that the completed run carries no residual transient failure_reason either.
RUN1_FR="$(apiget "/api/runs/$RUN1" | jq -r '.run.failure_reason // ""')"
[ -z "$RUN1_FR" ] \
  || fail "case 1: run carried a failure_reason after a transient outage — spill did not hold: $RUN1_FR"
# All the run's tick frames arrived (>=5); combined with the depth barrier seen during
# the outage and the gapless 1..N contiguity above, this shows the spilled frames were
# replayed (NT1 counts every tick frame — pre-, during-, and post-outage — so it is the
# barrier+contiguity, not NT1 alone, that isolates the replayed ones).
NT1="$(printf '%s' "$M1" | jq '[.messages[] | select((.payload.text? // "") | startswith("stub: outbox tick"))] | length')"
{ [ -n "$NT1" ] && [ "$NT1" -ge 5 ] 2>/dev/null; } \
  || fail "case 1: fewer than 5 outbox tick frames replayed (got ${NT1:-none})"
pass "case 1: spill+drain proven — barrier seen, run completed with no failed report, $NT1 ticks replayed, stream gapless 1..$(printf '%s' "$M1" | jq '.messages | length')"

# =============================================================================
# CASE 2 — a tiny per-run quota forces eviction; replay fills the range per-seq.
say "CASE 2: quota eviction -> per-seq tombstones (tiny WORKER_OUTBOX_RUN_MAX_BYTES)"
# 1 KiB per-run quota: over the outage the run spills far more than that, so the oldest
# segment is evicted to a range record whenever a new segment would exceed the quota.
export WORKER_OUTBOX_RUN_MAX_BYTES=1024
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
pass "agent recreated with WORKER_OUTBOX_RUN_MAX_BYTES=1024 (forces quota eviction under a longer spill)"

make_outbox_run
RUN2="$OUTBOX_RUN"
# A longer outage than case 1 so many small segments spill and the quota reliably binds.
outage "$RUN2" 25
wait_status "$RUN2" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
wait_drained "$RUN2"
assert_contiguous "$RUN2"

M2="$(apiget "/api/runs/$RUN2/messages")"
DROPPED="$(printf '%s' "$M2" | jq -c '[.messages[] | select(.kind=="status" and (.payload.event? == "message_dropped")) | .seq] | sort')"
NDROP="$(printf '%s' "$DROPPED" | jq 'length')"
if [ "$NDROP" -gt 0 ] 2>/dev/null; then
  # Every tombstone is a per-seq status frame carrying the fixed quota reason. Combined
  # with the unconditional gapless assertion above, this proves the range record was
  # expanded into ONE tombstone per evicted seq (no hole, no single-marker range).
  printf '%s' "$M2" | jq -e 'all(.messages[] | select(.payload.event? == "message_dropped"); (.kind == "status") and (.payload.reason == "outbox quota"))' >/dev/null \
    || fail "case 2: a message_dropped tombstone was not a status frame with the fixed reason \"outbox quota\""
  pass "case 2: quota eviction produced $NDROP per-seq \"outbox quota\" tombstones; the stream stayed gapless (seqs: $DROPPED)"
else
  # OBSERVED-ONLY guard, and NOT vacuous: assert_contiguous above already asserted the
  # invariant the tombstones exist to protect. Tombstones cannot be GUARANTEED on every
  # host because segment sizing is variable and D2 eviction is soft (a single segment
  # already over the tiny quota is written rather than dropped, and too few segments
  # spilled would never force an eviction), so the strict per-tombstone checks run only
  # when eviction is actually observed.
  say "case 2: no quota eviction observed on this run (segment sizing / spill volume left it under the 1 KiB quota); the contiguous-stream invariant still held (observed-only, by design)"
fi

# =============================================================================
# RESTORE — return the api stale window and the agent outbox knobs to their defaults so
# later phases (59-restart-agent, 60-62) are unaffected. Mirrors phase 46's restore.
say "restore: recreate api + agent back to their defaults"
unset E2E_WORKER_HEARTBEAT_STALE WORKER_TRANSIENT_TRIP_MS WORKER_OUTBOX_RUN_MAX_BYTES
"${COMPOSE[@]}" up -d --wait --no-deps --force-recreate api >/dev/null
wait_http
login
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
pass "api + agent recreated back to their defaults"

# Best-effort cleanup: both runs completed (terminal), so a cancel is a no-op, but never
# let a cleanup blip redden a passed phase.
apipost "/api/runs/$RUN1/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true
apipost "/api/runs/$RUN2/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true

fi
