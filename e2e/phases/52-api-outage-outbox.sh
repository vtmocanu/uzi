# shellcheck shell=bash
# phase:    api-outage-outbox
# title:    PRD #1391 + #1742: worker outbox survives an api outage (M5 spill+drain/tombstones, M6 write-ahead terminal replay; finalize-resume across an agent SIGKILL)
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID UZI_WORKER_TOKEN UZI_BIN UZI_TOKEN_VAL
# provides: -
# handoff:  -
# mutates:  compose:api(force-recreated: heartbeat-stale window raised for the outage; stop/started per case for each outage), compose:agent(force-recreated: outbox knobs; short stub-outbox stream for the M6 cases; RESTARTED mid-outage for the boot-gate case; SIGKILLed + started for the #1742 cases); eight stub runs created (the last two for the #1742 finalize-resume cases); the agent container is SIGKILLed and started again mid-outage in those two cases (case 8 does so twice; cases 7 and 8 run with the completion interlock on) and runs.requeue_count of each of those two runs is set to RUN_MAX_REQUEUES in-DB (SQL UPDATE, once, during its first cut with the api already stopped, before its FIRST restart only) so only the one-shot finalize allowance can requeue them; admin completion_interlock_rollout set false for cases 3-5, true from case 6; one run's started_at/completion_attempts back-dated in-DB to reach the timeout-sweep carve-out
# restores: api + agent force-recreated back to their defaults (stream length + quotas reset); completion_interlock_rollout back to true (also on EXIT); stub runs (incl. the two #1742 runs) cancelled best-effort
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
# PRD #1391 M6 (Run B) — the worker also survives an outage that swallows a run's TERMINAL
# OUTCOME, not just its messages. `completed`/`failed` of a run-lane attempt is journaled
# WRITE-AHEAD to the outbox (M3, keyed by claim generation) BEFORE its first network send and
# fenced on `runs.last_seq` at the api, so a run that finishes while the api is down replays its
# messages first and its outcome only once the trace is contiguous — no false `failed`, nothing
# redone. The M6 cases use the SAME UZI_STUB_OUTBOX sentinel with a SHORTER stream
# (UZI_STUB_OUTBOX_TICKS, e2e-only, exported below) so the run reaches its terminal INSIDE a
# bounded outage that stays under the raised heartbeat-stale window:
#   3. FINISH-DURING-OUTAGE: a run reaches `completed` (journaled write-ahead) while the api is
#      down; on recovery it ends `completed` AFTER its gapless messages (the fence), at its
#      original generation (replayed, not re-executed), with no transient `failed`. The judge /
#      notification enqueue rides the SAME fenced SetState, so it cannot fire ahead of the trace.
#   4. RESTART-MID-OUTAGE (SC3): the agent CONTAINER is restarted mid-outage. Its /data outbox
#      tree (the message spill + the pre-existing `.reserve` + the terminal journal) persists on
#      the agentdata named volume, so the restart resumes onto that existing tree (the outbox init
#      / reserve-grow-on-upgrade path) and the D7 BOOT GATE lands the journaled outcome BEFORE any
#      new claim — proven by the generation staying put and no requeue being charged.
#   5. CARVE-OUT-LOST-TERMINAL: a run placed (in the DB) in the post-attempt timeout-sweep carve-out
#      (completion_attempts>0 + a live worker) the ordinary timeout sweep is DELIBERATELY blind to
#      — reached directly in the DB — loses its terminal to the outage; the journal replay lands
#      `completed` (never the sweep's run_timeout `failed`), so it does not sit non-terminal forever.
#   Cases 3-5 create their runs with the admin Completion check OFF: an interlocked run cannot
#   reach `completed` before a live api issues its permit, so it never journals a terminal inside
#   the outage these cases need.
#   6. INTERLOCKED-FINISH-DURING-OUTAGE (Completion check back ON): the run retries its completion
#      permit through the outage instead of failing (`fetch failed` used to fail it, and the journal
#      replayed that `failed`), then completes at its original generation once the api is back.
#
# WHY the M5 outages are SPILL-GATED rather than fixed-length: the spill trip fires only
# inside a failed flush, and a failed flush costs a connect timeout whose length is a
# property of the HOST's networking, not of this phase (see wait_spilled). A fixed 20s
# outage entered spill AFTER the api was already back on the CI runner. Both M5 cases
# therefore cut the api, wait for the spill, and only then hold the outage open.
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
say "PRD #1391 M5+M6: worker outbox survives an api outage (spill+drain, quota tombstones; write-ahead terminal replay)"
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
  # Cases create one fresh run at a time. Re-check before each initial claim;
  # earlier completion can precede final custody acknowledgement.
  wait_custody_headroom 1
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
# wait_spilled RUN [TIMEOUT] — block until the batcher has ENTERED SPILL MODE for RUN,
# read from the agent's own log. The spill trip is evaluated ONLY inside a FAILED flush,
# and `enterSpill` needs TWO of them: the first arms the failure clock, the second
# compares it against WORKER_TRANSIENT_TRIP_MS. What a failed flush COSTS is host
# dependent — a refused connect returns in ~1s against Docker Desktop, while the CI
# runner's bridge black-holes the SYN and the fetch pays the full connect timeout,
# measured at ~10s per attempt (2026-09-19 nightly artifacts, agent.log: flush failures
# at 06:16:14.613 and 06:16:25.552 for a 20s outage). So the spill starts anywhere from
# ~4s to ~21s in and a FIXED sleep cannot bound it: on that nightly the batcher entered
# spill 0.37s AFTER the api was back and drained 1.65s later, leaving no window at all
# for the first recovered heartbeat to report a depth. Gate the outage on the EVENT
# instead, then hold it open for the caller's window so a real backlog builds.
# Returns 0 once observed and 1 on timeout; it never calls `fail` itself, because it runs
# with the api STOPPED and a `fail` there would leave the whole stack down for every later
# phase. The caller restores the api first and judges after.
wait_spilled() {
  local run="$1" timeout="${2:-60}" f="$RUNROOT/.outbox-spill.log"
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    "${COMPOSE[@]}" logs --no-color agent > "$f" 2>/dev/null || true
    # awk, not `grep … | grep -q`: it reads to EOF, so there is no early-exiting reader to
    # SIGPIPE its writer (CLAUDE.md), and it matches BOTH literals on the SAME line, so a
    # spill logged for an earlier case's run can never satisfy this one.
    if awk -v r="$run" 'index($0, "entered spill mode") && index($0, r) { hit = 1 } END { exit(hit ? 0 : 1) }' "$f"; then
      pass "batcher entered spill mode for run $run $((SECONDS - start))s into the outage"
      return 0
    fi
    sleep 1
  done
  return 1
}
# wait_terminal_lost RUN [TIMEOUT] — block until run RUN has both REACHED+JOURNALED its terminal
# write-ahead AND had its FIRST live send of that terminal FAIL against the down api, read as an
# ORDERED pair from the agent's own log:
#   1. "outbox: terminal journaled write-ahead" (terminal-resolve.ts, the durable-before-first-send
#      success path) — the outcome is on disk;
#   2. AFTER it, for the same run, a send failure of that terminal: the per-attempt
#      "state report failed, retrying" (client.ts, the first transient miss, ~one connect timeout in)
#      or the terminal exhaustion "terminal report send failed; leaving the journal for a later
#      resolve" (terminal-resolve.ts). Once the run is journaled it produces no further NON-terminal
#      /state report, so the first post-journal send failure IS the terminal send's — which is what
#      forces recovery through the persisted journal rather than a fresh live report.
# This is the M6 analogue of wait_spilled, and it closes the race CodeRabbit flagged on the naive
# "journaled" gate: gating on the journal event alone let api_back race the FIRST send and let that
# send succeed post-recovery, so neither replay path was exercised. Gating on the send FAILURE proves
# it by construction. We key on the per-attempt retry rather than the full retry-exhaustion event on
# purpose: the terminal retry schedule is [1,2,4,8,16]s over 6 attempts, and on the CI lane where each
# failed connect black-holes ~10s, exhaustion alone is ~90s. Including run work and api restart
# could exceed the heartbeat-stale window below (300s) and swap this into a stale-requeue failure.
# The FIRST failed attempt is the same
# proof and fires ~one connect timeout after the terminal, keeping the whole outage well under the
# window. TIMEOUT bounds it likewise. Returns 0 once observed and 1 on timeout; it never calls `fail`
# itself (it runs with the api STOPPED) — the caller restores the api and judges after.
# TIMEOUT defaults to 180s. The old 80s under-shot on the gitlab CI lane (nightly run 36680011794,
# 2026-09-30): with the api down, finalize itself takes ~60s, because PR-description staging waits
# ~10s on a failed fetch and MR creation spends ~32s in transient forge retries, so the terminal
# was journaled ~70s after the cut and its first send failed ~80s after it.
wait_terminal_lost() {
  local run="$1" timeout="${2:-180}" f="$RUNROOT/.outbox-terminal.log"
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    "${COMPOSE[@]}" logs --no-color agent > "$f" 2>/dev/null || true
    # awk, not `grep … | grep -q` (CLAUDE.md): reads to EOF so no SIGPIPE. Each structured-log line
    # carries run_id, so `index($0, r)` scopes to THIS run; the journaled line must be seen FIRST
    # (j=1) before a send failure counts, so an earlier case's line can never satisfy this one.
    if awk -v r="$run" '
        index($0, r) && index($0, "terminal journaled write-ahead") { j = 1 }
        j && index($0, r) && (index($0, "state report failed, retrying") || index($0, "terminal report send failed")) { hit = 1 }
        END { exit(hit ? 0 : 1) }' "$f"; then
      pass "run $run reached its terminal (journaled) and lost its first send to the outage $((SECONDS - start))s in"
      return 0
    fi
    sleep 1
  done
  return 1
}
# wait_permit_retrying RUN [TIMEOUT] — block until run RUN, finishing while the api is down, is
# RETRYING its completion-permit request ("completion permit request failed, retrying", client.ts)
# rather than failing. An interlocked run cannot journal `completed` before it holds a permit, so it
# never reaches wait_terminal_lost's journaled terminal during an outage: this is its CASE 6 gate.
# Same contract as the other wait_*: 0 once observed, 1 on timeout, never `fail`s (the api is down).
wait_permit_retrying() {
  local run="$1" timeout="${2:-60}" f="$RUNROOT/.outbox-permit.log"
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    "${COMPOSE[@]}" logs --no-color agent > "$f" 2>/dev/null || true
    if awk -v r="$run" 'index($0, r) && index($0, "completion permit request failed, retrying") { hit = 1 } END { exit(hit ? 0 : 1) }' "$f"; then
      pass "run $run is retrying its completion permit against the down api $((SECONDS - start))s in"
      return 0
    fi
    sleep 1
  done
  return 1
}
# set_interlock ON|OFF — the admin Completion check switch (completion_interlock_rollout), read
# once when a run is CREATED. Cases 3-5 test the #1391 terminal journal/replay, which needs the
# terminal to exist while the api is down; an interlocked run cannot reach `completed` without a
# permit from a live api (it waits, CASE 6), so those cases run un-interlocked, as they did before
# the interlock became the default (#1644). CASE 6 turns it back on.
set_interlock() {
  apiput /api/admin/settings "{\"settings\":{\"completion_interlock_rollout\":\"$1\"}}" >/dev/null
}
# api_back — bring the api up after an outage and wait for the harness's HTTP path
# through web to recover. Compose nginx now re-resolves the api address after its
# five-second DNS cache expires; the standalone compose-web-dns phase proves the
# address-swap behavior directly. Case 4 still exercises the real outage path.
api_back() {
  "${COMPOSE[@]}" up -d --wait api >/dev/null
  wait_http
  login
}
# outage RUN SECS — cut the api, wait until the batcher has SPILLED run RUN, hold the
# outage SECS longer so a real backlog accumulates, bring the api back, re-login (the
# bounce drops the session), then assert the barrier. Spill-gated rather than
# fixed-length: see wait_spilled for why a bare `sleep` cannot bound the spill. The
# total stays well under the 300s heartbeat-stale window raised below (~21s worst-case
# spill + 25s the longest hold). No `docker compose start` (no phase uses it): stop,
# then api_back, which waits for the existing web proxy to recover.
outage() {
  local run="$1" secs="$2" spilled=0
  say "cutting the api (> WORKER_TRANSIENT_TRIP_MS=3s) so the batcher spills run $run, then holding it down ${secs}s past the spill"
  "${COMPOSE[@]}" stop api >/dev/null 2>&1
  if wait_spilled "$run"; then spilled=1; fi
  sleep "$secs"
  api_back
  # Judge the spill only AFTER the api is back: a `fail` with it still stopped would take
  # every later phase down with this one.
  [ "$spilled" = 1 ] \
    || fail "outbox phase: the batcher never entered spill mode for run $run while the api was down — nothing was spilled, so there is no barrier to observe"
  observe_barrier "$run"
}

# --- raise the api heartbeat-stale window so the outage never requeues the run --------
# Exported so it out-ranks the env-file's E2E_WORKER_HEARTBEAT_STALE=15s (compose ranks
# shell env above --env-file), exactly as phase 46 exports UZI_E2E_MAX_CONCURRENT_RUNS.
export E2E_WORKER_HEARTBEAT_STALE=300s
"${COMPOSE[@]}" up -d --wait --no-deps --force-recreate api >/dev/null
wait_http
login
pass "api recreated with a 300s heartbeat-stale window so the outage cannot requeue the running run"

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
# A longer post-spill hold than case 1 so many small segments spill and the quota reliably binds.
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
# PRD #1391 M6 (Run B) — write-ahead terminal reports survive the SAME outage. A run-lane
# `completed`/`failed` is journaled write-ahead to the outbox (M3, keyed by claim generation)
# BEFORE its first network send and fenced on runs.last_seq at the api, so a run that finishes
# while the api is down replays its messages first and its outcome only once the trace is
# contiguous. These cases reuse the UZI_STUB_OUTBOX sentinel but with the SHORT e2e-only stream
# (UZI_STUB_OUTBOX_TICKS) so the run reaches its terminal INSIDE a bounded outage that still stays
# under the 300s heartbeat-stale window raised above — the worker stays leased across the outage.
say "M6 (Run B): write-ahead terminal reports survive an api outage (finish-during, restart, sweep carve-out; interlocked permit wait)"
unset WORKER_OUTBOX_RUN_MAX_BYTES            # back to DEFAULT quotas (a clean spill, no eviction) for the M6 runs
export UZI_STUB_OUTBOX_TICKS=20              # ~20s stream (vs the 90s Run A default), e2e-only; unset at restore
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
pass "agent recreated with WORKER_TRANSIENT_TRIP_MS=3s + a short UZI_STUB_OUTBOX stream for the M6 terminal-replay cases"

# rb_run_field RUN COL — a scalar runs column straight from the db (COL is a fixed literal from
# THIS file, never user input). Empty string for a SQL NULL (the 42-readoption idiom).
rb_run_field() { db_psql "SELECT $2 FROM runs WHERE id = '$1'"; }

# Each M6 outage is EVENT-GATED on the terminal being journaled AND its first live send lost to the
# outage (wait_terminal_lost), not a fixed sleep: the run must REACH+JOURNAL its terminal AND have its
# first send fail while the api is unreachable, so recovery is forced through the persisted journal.
# How long the short stream takes to get there depends on the runner's speed, not this phase (a fixed
# 30s under-shot it on the slow gitlab CI lane — the #1391 M6 regression this replaces). The gate's
# timeout stays under the 300s heartbeat-stale window, so the worker is never swept stale
# mid-outage and the run is never requeued. Budget: the gate (180s) plus, for case 4, a container
# restart and the api start, well inside 300s.

# Cases 3-5 exercise the terminal journal, so their runs are created un-interlocked (set_interlock).
# The trap restores the default even if a case fails.
set_interlock false
trap 'set_interlock true 2>/dev/null || true' EXIT

# =============================================================================
# CASE 3 — a run FINISHING during the outage is `completed` AFTER its messages (the fence), at its
# original generation (replayed, not re-executed), with no false `failed` (SC1/SC2).
say "CASE 3: a run finishing during the outage -> completed after its messages (fence), no false failed"
make_outbox_run
RUN_B1="$OUTBOX_RUN"
GEN_B1="$(rb_run_field "$RUN_B1" claim_generation)"
say "cutting the api so run $RUN_B1 reaches AND journals its terminal (write-ahead) while it is down"
"${COMPOSE[@]}" stop api >/dev/null 2>&1
terminal_lost_b1=0; if wait_terminal_lost "$RUN_B1"; then terminal_lost_b1=1; fi
api_back
# Judge only AFTER the api is back: a `fail` with it still stopped would take every later phase down
# with this one.
[ "$terminal_lost_b1" = 1 ] \
  || fail "case 3: run $RUN_B1 never reached+journaled its terminal AND lost its first send while the api was down — the finish-during-outage precondition never held, so the first send could have landed post-recovery and the fence replay was never exercised"
# On recovery the drainer replays the run's messages FIRST; the terminal SetState is then fenced
# on runs.last_seq (409 messages_pending until the trace is contiguous through the journal's
# messages_through_seq), so `completed` — and the judge/notification enqueue that rides the SAME
# applied SetState (service.go:2759-2768) — can only land AFTER the whole trace. wait_status
# completed ALSO fails on any transient `failed`, so SC1's "no false failed" is enforced here.
wait_status "$RUN_B1" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
wait_drained "$RUN_B1"
assert_contiguous "$RUN_B1"
RB1_FR="$(apiget "/api/runs/$RUN_B1" | jq -r '.run.failure_reason // ""')"
[ -z "$RB1_FR" ] \
  || fail "case 3: run carried a failure_reason after a transient outage — the journaled completed was not the outcome: $RB1_FR"
# Replayed from the journal, not re-executed: the write-ahead outcome is sent at its ORIGINAL
# claim generation, so the generation is unchanged (a re-execution would re-claim, +1).
[ "$(rb_run_field "$RUN_B1" claim_generation)" = "$GEN_B1" ] \
  || fail "case 3: generation advanced across the outage — the outcome was re-executed, not replayed (gen $GEN_B1 -> $(rb_run_field "$RUN_B1" claim_generation))"
# A `completed` run whose whole persisted stream is gapless 1..N (assert_contiguous above) could
# only reach terminal once last_seq was contiguous through the fence; the per-seq fence refusal
# itself is asserted directly in api/internal/workersvc/terminal_fence_livedb_test.go.
pass "case 3: run finished during the outage -> completed AFTER its gapless messages (fence held), gen unchanged ($GEN_B1), no false failed"

# =============================================================================
# CASE 4 — the agent CONTAINER restarted mid-outage lands the journaled outcome from the D7 BOOT
# GATE, before any new claim (SC3). The run first spills MESSAGES during the outage, so its outbox
# dir holds a message-only spill; combined with the suite's pre-existing `.reserve` this is the
# EXISTING disk tree the restart resumes onto (the outbox init / reserve-grow-on-upgrade path runs
# against it on boot). /data is the `agentdata` named volume, so the tree persists across a restart.
say "CASE 4: agent restarted mid-outage -> boot gate lands the journaled outcome before any claim (SC3)"
make_outbox_run
RUN_B2="$OUTBOX_RUN"
GEN_B2="$(rb_run_field "$RUN_B2" claim_generation)"
RQ_B2="$(rb_run_field "$RUN_B2" requeue_count)"
say "cutting the api so run $RUN_B2 spills its messages and journals its terminal while down"
"${COMPOSE[@]}" stop api >/dev/null 2>&1
# Gate BEFORE the restart on the terminal being journaled AND its first live send lost: the run must
# have spilled its messages AND installed its terminal journal (the batcher is final-flushed before
# the terminal is journaled, so the message spill is already on disk by the time the journal line
# lands) AND had its first send fail unresolved, so the restarted worker's boot gate is the only path
# left to land the on-disk outcome. Judged after api_back — a `fail` with the api down would wedge
# every later phase.
terminal_lost_b2=0; if wait_terminal_lost "$RUN_B2"; then terminal_lost_b2=1; fi
# Restart the agent CONTAINER while the api is still down. Its original execution process dies, so
# the ONLY path left to terminal is the boot gate replaying the on-disk journal. `restart` reuses
# the same container + the same /data volume, so the existing outbox tree is what the worker boots
# onto (no --force-recreate, which would keep the named volume too but discard the container).
say "restarting the agent container mid-outage (its /data outbox tree persists across the restart)"
"${COMPOSE[@]}" restart agent >/dev/null 2>&1
# Bring the api back. The restarted worker — parked in its register-retry while the api was down —
# registers with its pending-terminal snapshot, then the boot gate resolves the journal BEFORE the
# claim loops start.
api_back
wait_worker_online
[ "$terminal_lost_b2" = 1 ] \
  || fail "case 4: run $RUN_B2 never journaled its terminal AND lost its first send before the restart — the original process died with no unresolved on-disk outcome, so the boot gate had nothing to replay"
wait_status "$RUN_B2" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
wait_drained "$RUN_B2"
assert_contiguous "$RUN_B2"
# Landed via the BOOT GATE, before any new claim: the terminal is sent at the JOURNALED generation
# with no re-claim, so the generation is unchanged AND no requeue was charged. A re-execution
# (journal missing / run re-claimed) would advance the generation and bank a requeue.
[ "$(rb_run_field "$RUN_B2" claim_generation)" = "$GEN_B2" ] \
  || fail "case 4: generation advanced across the restart — re-executed, not landed from the boot gate (gen $GEN_B2 -> $(rb_run_field "$RUN_B2" claim_generation))"
[ "$(rb_run_field "$RUN_B2" requeue_count)" = "$RQ_B2" ] \
  || fail "case 4: the run was requeued across the restart (re-executed) rather than replayed from the boot gate (requeue_count $RQ_B2 -> $(rb_run_field "$RUN_B2" requeue_count))"
RB2_FR="$(apiget "/api/runs/$RUN_B2" | jq -r '.run.failure_reason // ""')"
[ -z "$RB2_FR" ] \
  || fail "case 4: run carried a failure_reason after the restart — the journaled outcome was not the result: $RB2_FR"
pass "case 4: agent restart mid-outage -> boot gate landed the journaled completed at gen $GEN_B2 (no re-claim, no requeue), stream gapless"

# =============================================================================
# CASE 5 — a run in the timeout-sweep CARVE-OUT (completion_attempts>0, set in the DB; the run itself is not interlocked) whose lost terminal lands on replay: it does not sit non-terminal forever.
say "CASE 5: a sweep-carve-out run's lost terminal lands on replay (never sits non-terminal forever)"
make_outbox_run
RUN_B3="$OUTBOX_RUN"
GEN_B3="$(rb_run_field "$RUN_B3" claim_generation)"
# Place the run in the completion-interlock carve-out the ordinary timeout sweep is DELIBERATELY
# blind to (SweepRunningTimeout / sweep.go: `completion_attempts > 0 AND a live worker`): a
# post-attempt run past its wall budget that the wall-clock sweep will NOT terminal-fail. We reach
# that exact DB state directly — completion_attempts=1 + a long-past started_at — rather than
# driving the whole budget-exhaustion protocol, because the point of THIS case is that a LOST
# terminal on such a run lands via the write-ahead journal, not the interlock protocol itself.
db_psql "UPDATE runs SET completion_attempts = 1, started_at = now() - interval '30 days' WHERE id = '$RUN_B3'" >/dev/null
[ "$(rb_run_field "$RUN_B3" completion_attempts)" = 1 ] \
  || fail "case 5: could not place run $RUN_B3 in the interlock carve-out (completion_attempts != 1)"
# The carve-out holds: the run is 30 days past its wall yet the 2s sweeper spares it (live worker +
# completion_attempts>0). Watch it stay `running` across several sweep ticks — a broken carve-out
# would fail it with fail_origin='run_timeout' right here, before the outage.
CO_DEADLINE=$((SECONDS + 6))
while [ "$SECONDS" -lt "$CO_DEADLINE" ]; do
  CO_ST="$(rb_run_field "$RUN_B3" status)"
  [ "$CO_ST" = running ] \
    || fail "case 5: the past-wall carve-out run left 'running' before the outage (status=$CO_ST) — the timeout-sweep carve-out did not hold"
  sleep 1
done
pass "case 5: past-wall carve-out run held at 'running' across the sweep (carve-out engaged; the wall-clock sweep will never terminalise it)"
# Now lose its terminal to an outage. Without the journal replay this run would stay non-terminal
# forever (the carve-out spares it from the timeout sweep AND the judge sweep never touches it).
say "cutting the api so the carve-out run's terminal is journaled and its first send lost"
"${COMPOSE[@]}" stop api >/dev/null 2>&1
terminal_lost_b3=0; if wait_terminal_lost "$RUN_B3"; then terminal_lost_b3=1; fi
api_back
[ "$terminal_lost_b3" = 1 ] \
  || fail "case 5: the carve-out run $RUN_B3 never journaled its terminal AND lost its first send while the api was down — with the outcome unresolved through a journal it would sit non-terminal forever (the carve-out spares it from both sweeps), which is the exact loss this case proves is repaired"
# The journal replays: the run reaches `completed` (never the sweep's run_timeout `failed`), at its
# original generation, with a gapless trace.
wait_status "$RUN_B3" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
wait_drained "$RUN_B3"
assert_contiguous "$RUN_B3"
[ "$(rb_run_field "$RUN_B3" claim_generation)" = "$GEN_B3" ] \
  || fail "case 5: generation advanced — the terminal was re-executed, not replayed from the journal (gen $GEN_B3 -> $(rb_run_field "$RUN_B3" claim_generation))"
RB3_ORIGIN="$(apiget "/api/runs/$RUN_B3" | jq -r '.run.fail_origin // ""')"
[ -z "$RB3_ORIGIN" ] \
  || fail "case 5: run carried fail_origin=$RB3_ORIGIN — a wall-clock sweep failed it instead of the journal landing 'completed'"
pass "case 5: the carve-out run's lost terminal landed on replay -> completed at gen $GEN_B3 (never sat non-terminal, never sweep-failed)"

# =============================================================================
# CASE 6 — an INTERLOCKED run finishing during the outage WAITS for the api to issue its completion
# permit, then completes. Regression: its one-shot permit request used to throw `fetch failed` into
# the generic catch, and the run was journaled and replayed as `failed` (#1644 made every issue run
# interlocked by default, which is when this phase started failing).
say "CASE 6: an interlocked run finishing during the outage waits for its completion permit, then completes"
set_interlock true
make_outbox_run
RUN_B4="$OUTBOX_RUN"
GEN_B4="$(rb_run_field "$RUN_B4" claim_generation)"
[ -n "$(rb_run_field "$RUN_B4" completion_contract_version)" ] \
  || fail "case 6: run $RUN_B4 was not stamped for the completion interlock (completion_contract_version is NULL), so it cannot prove the permit path"
say "cutting the api so run $RUN_B4 reaches its completion permit while it is down"
"${COMPOSE[@]}" stop api >/dev/null 2>&1
permit_retrying_b4=0; if wait_permit_retrying "$RUN_B4"; then permit_retrying_b4=1; fi
api_back
[ "$permit_retrying_b4" = 1 ] \
  || fail "case 6: run $RUN_B4 never retried its completion permit while the api was down (it failed, or never reached finalize during the outage)"
wait_status "$RUN_B4" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
wait_drained "$RUN_B4"
assert_contiguous "$RUN_B4"
RB4_FR="$(apiget "/api/runs/$RUN_B4" | jq -r '.run.failure_reason // ""')"
[ -z "$RB4_FR" ] \
  || fail "case 6: the interlocked run carried a failure_reason after the outage: $RB4_FR"
[ "$(rb_run_field "$RUN_B4" claim_generation)" = "$GEN_B4" ] \
  || fail "case 6: generation advanced — the run was re-executed instead of waiting out the outage (gen $GEN_B4 -> $(rb_run_field "$RUN_B4" claim_generation))"
pass "case 6: the interlocked run waited out the outage for its permit -> completed at gen $GEN_B4, no false failed"

# =============================================================================
# Issue #1742 — the finalize record (write-ahead of every finalize step) survives an agent restart
# that lands while the api is DOWN, and the one-shot finalize-resume allowance keeps that run from
# being failed as worker_lost by the orphan pass, exactly once. The cut is DETERMINISTIC: the api is
# stopped while the stub stream is still running (executor not returned), so no terminal can land, and
# the agent is restarted only once the finalize record is PROVEN durable, by BOTH the agent's own
# `finalize record durable` line (logged after the directory fsync) AND the file being on disk.
say "ISSUE #1742: finalize-resume allowance across an agent restart during an api outage (A completes, B is one-shot)"

# f42_max_requeues — the api's RUN_MAX_REQUEUES as the e2e stack renders it (docker-compose.e2e.yml
# sets it). Prints the integer and returns 0; returns 1 (message on stderr) when the config cannot be
# rendered or carries no integer value: a guessed budget would make the "budget spent" premise false.
# The caller judges, with the api up.
f42_max_requeues() {
  local v
  v="$("${COMPOSE[@]}" config --format json 2>/dev/null | jq -r '.services.api.environment.RUN_MAX_REQUEUES // empty' 2>/dev/null)" || v=""
  case "$v" in
    ''|*[!0-9]*) echo "compose config did not render a numeric services.api.environment.RUN_MAX_REQUEUES (got '${v}')" >&2; return 1;;
  esac
  printf '%s' "$v"
}
# f42_durable_logged RUN GEN — 0 when the agent's log carries `finalize record durable` for RUN at claim
# generation GEN. The agent logs JSON, so run_id and claim_generation are matched as fields of the SAME
# line as the message; the generation is matched exactly (a line for G must not satisfy G+1). awk, not
# `grep … | grep -q` (CLAUDE.md): it reads to EOF, so no SIGPIPE.
f42_durable_logged() {
  local run="$1" gen="$2" f="$RUNROOT/.outbox-finalize.log"
  "${COMPOSE[@]}" logs --no-color agent > "$f" 2>/dev/null || true
  awk -v r="$run" -v g="$gen" '
    index($0, "finalize record durable") && index($0, r) && ($0 ~ ("\"claim_generation\":" g "([^0-9]|$)")) { hit = 1 }
    END { exit(hit ? 0 : 1) }' "$f"
}
# f42_cut RUN GEN — the deterministic cut for run RUN at claim generation GEN: stop the api (the run's
# stream is still running, so no terminal can land), assert there is NO durable finalize line yet, then
# wait (bounded) for BOTH proofs of the durable point. Returns 0 once both hold, 1 on timeout, 2 when the
# hand-off beat the outage (inconclusive). Like the other wait_* it never calls `fail` itself: it runs
# with the api STOPPED, and a `fail` there would wedge every later phase. The caller restores the api
# first and judges after.
f42_cut() {
  local run="$1" gen="$2" start deadline
  "${COMPOSE[@]}" stop api >/dev/null 2>&1
  if f42_durable_logged "$run" "$gen"; then
    return 2
  fi
  start=$SECONDS; deadline=$((SECONDS + 90))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if f42_durable_logged "$run" "$gen" \
      && "${COMPOSE[@]}" exec -T agent test -f "/data/outbox/$run/finalize-$gen.json" >/dev/null 2>&1; then
      pass "run $run: finalize record durable at generation $gen (log line + on-disk file) $((SECONDS - start))s after the api was cut"
      return 0
    fi
    sleep 1
  done
  return 1
}
# f42_cut_judge RC RUN GEN — judge f42_cut's status AFTER the api is back.
f42_cut_judge() {
  case "$1" in
    0) return 0;;
    2) fail "issue 1742: inconclusive: run $2 handed off to finalize (finalize record durable already logged at generation $3) before the api outage took hold — the cut was not before the hand-off";;
    *) fail "issue 1742: run $2 never showed both the 'finalize record durable' log line and finalize-$3.json on disk within 90s of the outage";;
  esac
}
# f42_restart_cycle RUN GEN MAX SET_MAX — from a cut api: check the cut is still the state the case
# needs (finalize-GEN.json present, NO terminal-GEN.json journaled), optionally (SET_MAX=1, the FIRST cut
# of a run only) raise requeue_count to the budget ceiling so ONLY the one-shot finalize allowance can
# save the run, then SIGKILL the agent (abrupt, no graceful-shutdown path, as the ADR specifies) and start
# it on the same /data volume, bring the api back and wait for the worker. The api is down for the whole
# function until api_back, so no `fail` and no set -e abort may happen before it: every step records its
# error, the api is restored, and only then is the error judged. A precondition or UPDATE error skips
# the kill (the attempt is inconclusive) but still restores the api.
f42_restart_cycle() {
  local run="$1" gen="$2" max="$3" set_max="${4:-0}" err="" rc=0 st="" regs_before=0 regs_now=0 deadline
  # One exec prints a state token, so an exec/container error (no token) is told apart from a
  # journaled terminal.
  rc=0; st="$("${COMPOSE[@]}" exec -T agent sh -c \
    "if [ ! -f '/data/outbox/$run/finalize-$gen.json' ]; then echo NO_FINALIZE; elif [ -e '/data/outbox/$run/terminal-$gen.json' ]; then echo TERMINAL_JOURNALED; else echo READY; fi" 2>/dev/null)" || rc=$?
  st="$(printf '%s' "$st" | tr -d '[:space:]')"
  case "$rc:$st" in
    0:READY) ;;
    0:NO_FINALIZE) err="inconclusive: /data/outbox/$run/finalize-$gen.json is not present before the restart";;
    0:TERMINAL_JOURNALED) err="inconclusive: a terminal was already journaled (terminal-$gen.json exists) for run $run before the restart, so the restart would not interrupt finalize";;
    *) err="could not inspect /data/outbox/$run in the agent container (exec exit $rc, output '$st')";;
  esac
  if [ -z "$err" ] && [ "$set_max" = 1 ]; then
    rc=0; db_psql "UPDATE runs SET requeue_count = $max WHERE id = '$run'" >/dev/null || rc=$?
    [ "$rc" = 0 ] || err="could not set requeue_count=$max on run $run (psql exit $rc)"
    if [ -z "$err" ]; then
      [ "$(rb_run_field "$run" requeue_count || true)" = "$max" ] || err="requeue_count on run $run is not $max after the UPDATE"
    fi
  fi
  if [ -z "$err" ]; then
    # Baseline of the agent's "registered" log lines, so the post-restart wait can prove a NEW register.
    # A failed log read must not yield a 0 baseline (the agent's earlier register would then pass the wait).
    rc=0; regs_before="$("${COMPOSE[@]}" logs agent 2>/dev/null)" || rc=$?
    if [ "$rc" = 0 ]; then
      regs_before="$(printf '%s\n' "$regs_before" | grep -c '"msg":"registered"' || true)"
    else
      err="could not read the agent logs for the register baseline (exit $rc)"
    fi
  fi
  if [ -z "$err" ]; then
    say "SIGKILLing the agent container with the api still down (run $run generation $gen)"
    rc=0; "${COMPOSE[@]}" kill -s SIGKILL agent >/dev/null 2>&1 || rc=$?
    [ "$rc" = 0 ] || err="could not SIGKILL the agent container (exit $rc)"
    rc=0; "${COMPOSE[@]}" start agent >/dev/null 2>&1 || rc=$?
    [ "$rc" = 0 ] || err="${err:+$err; }could not start the agent container again (exit $rc)"
  fi
  api_back
  [ -z "$err" ] || fail "issue 1742: $err"
  # wait_worker_online alone proves nothing here (the raised heartbeat-stale window keeps the row
  # online across the kill), so require a fresh "registered" line from the restarted agent (worker.ts
  # registerWithRetry). f42_wait_reclaim / the fail waits remain the judges of the run outcome.
  deadline=$((SECONDS + 90))
  while [ "$SECONDS" -lt "$deadline" ]; do
    regs_now="$("${COMPOSE[@]}" logs agent 2>/dev/null | grep -c '"msg":"registered"' || true)"
    [ "${regs_now:-0}" -gt "${regs_before:-0}" ] && break
    sleep 1
  done
  [ "${regs_now:-0}" -gt "${regs_before:-0}" ] \
    || fail "issue 1742: the restarted agent never logged a new 'registered' line within 90s (before=$regs_before now=${regs_now:-0}) for run $run"
  wait_worker_online
}
# f42_wait_reclaim RUN GEN TIMEOUT — poll until RUN is running at claim generation GEN. A run that goes
# failed while waiting is a failure with its origin named.
f42_wait_reclaim() {
  local run="$1" gen="$2" timeout="${3:-120}" deadline s g
  deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    s="$(rb_run_field "$run" status)"; g="$(rb_run_field "$run" claim_generation)"
    if [ "$s" = running ] && [ "$g" = "$gen" ]; then return 0; fi
    case "$s" in
      failed|cancelled) fail "issue 1742: run $run went '$s' (fail_origin=$(rb_run_field "$run" fail_origin)) while waiting to be re-claimed at generation $gen";;
    esac
    sleep 0.5
  done
  fail "issue 1742: run $run was never re-claimed and running at generation $gen (status=${s:-none} generation=${g:-none})"
}

# This single-worker harness retains the predecessor clone across the container restart.
# The exact G+1 claim MUST capture it and park before G+2 executes. A direct G+1 completion
# is a failure here, not an acceptable alternate path. Pin the extra claim to ordered log
# evidence and an available capture at EXACT G+1, never accept an arbitrary later generation.
f42_assert_capture_park() {
  local run="$1" gen="$2" log_file="$RUNROOT/.f42-capture.log" recovery_file="$RUNROOT/.f42-capture.json"
  "${COMPOSE[@]}" logs --no-color agent > "$log_file" 2>/dev/null \
    || fail "issue 1742: could not read predecessor-capture evidence for $run"
  jq -Rse --arg run "$run" --argjson gen "$gen" '
    [split("\n")[] | sub("^[^{]*"; "") | fromjson? | select(.run_id == $run)] as $events |
    [$events | to_entries[] | select(.value.msg == "run claimed") | .key] as $claims |
    [$events | to_entries[] | select(.value.msg == "run parked for transient recovery"
      and .value.detail == "recovering retained work before reseeding") | .key] as $parks |
    # runner.ts emits legacy claim_generation at park/early-terminal disposition,
    # and guarded generation at inventory disposition. Retries can log the same
    # uploaded capture again; require one distinct capture, not one log emission.
    [$events | to_entries[] | select(
      ((.value.msg == "recovery: park/early-terminal disposition outcome" and .value.claim_generation == $gen)
       or (.value.msg == "recovery: guarded inventory disposition" and .value.generation == $gen))
      and .value.state == "uploaded" and (.value.capture_id | type == "string" and length > 0))]
      | unique_by(.value.capture_id) | map(.key) as $captures |
    ($claims | length) == 3 and ($parks | length) == 1 and ($captures | length) == 1
    and $claims[1] < $parks[0] and $parks[0] < $captures[0] and $captures[0] < $claims[2]
  ' "$log_file" >/dev/null \
    || fail "issue 1742: run $run lacks the exact G+1 capture/park then G+2 claim sequence"
  uzi_cli run recovery "$run" --json > "$recovery_file" \
    || fail "issue 1742: could not read recovery captures for $run"
  jq -e --argjson gen "$gen" '
    [.[] | select(.generation == $gen) | .captures[]? | select(.state == "available")] | length == 1
  ' "$recovery_file" >/dev/null \
    || fail "issue 1742: run $run has no unique available capture at generation $gen"
  pass "run $run: exact generation $gen captured retained work and parked before the next claim"
}

# The api is up here, so a failure to resolve the budget can fail at once.
F42_MAX="$(f42_max_requeues)" || fail "issue 1742: cannot resolve RUN_MAX_REQUEUES from the rendered compose config: $(f42_max_requeues 2>&1 >/dev/null || true)"

# -----------------------------------------------------------------------------
# CASE 7 (A, completion control): the durable finalize record lets the run resume at G+1 even with the
# requeue budget spent. G+1 captures the retained predecessor clone and parks; G+2 completes.
say "CASE 7: finalize restart on spent budget -> G+1 capture park -> G+2 completes (never worker_lost)"
make_outbox_run
RUN_F1="$OUTBOX_RUN"
GEN_F1="$(rb_run_field "$RUN_F1" claim_generation)"
say "cutting the api while run $RUN_F1's stub stream is still running (generation $GEN_F1)"
rc=0; f42_cut "$RUN_F1" "$GEN_F1" || rc=$?
if [ "$rc" = 0 ]; then
  f42_restart_cycle "$RUN_F1" "$GEN_F1" "$F42_MAX" 1
else
  api_back
  f42_cut_judge "$rc" "$RUN_F1" "$GEN_F1"
fi
wait_status "$RUN_F1" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
[ "$(rb_run_field "$RUN_F1" fail_origin)" = "" ] \
  || fail "case 7: run $RUN_F1 carries fail_origin=$(rb_run_field "$RUN_F1" fail_origin) — the orphan pass failed a run the finalize allowance should have resumed"
[ "$(rb_run_field "$RUN_F1" finalize_resume_generation)" = "$GEN_F1" ] \
  || fail "case 7: finalize_resume_generation is '$(rb_run_field "$RUN_F1" finalize_resume_generation)', want $GEN_F1 — the allowance was not consumed for the durable finalize generation"
f42_assert_capture_park "$RUN_F1" "$((GEN_F1 + 1))"
[ "$(rb_run_field "$RUN_F1" claim_generation)" = "$((GEN_F1 + 2))" ] \
  || fail "case 7: run $RUN_F1 completed at generation $(rb_run_field "$RUN_F1" claim_generation), want $((GEN_F1 + 2)) after the proved G+1 capture park"
pass "case 7: run $RUN_F1 captured at generation $((GEN_F1 + 1)) and completed at $((GEN_F1 + 2)); finalize_resume_generation=$GEN_F1, never worker_lost"

# -----------------------------------------------------------------------------
# CASE 8 (B, one-shot control): the allowance is used ONCE. After the G+1 capture park, a second
# interruption of the EXECUTING generation G+2 leaves the
# spent budget to hold preserved work for owner recovery, with no second finalize allowance.
# The orphan/finalize exhaustion writers in worker_recovery.sql (#2426) preserve the
# requeue/allowance counters, stamp worker_requeue_exhausted and set retry to NULL.
assert_finalize_allowance_spent() {
  local run="$1" original="$2" executing="$3" max="$4"
  [ "$(rb_run_field "$run" status)" = recovery_wait ] || fail "case 8: spent allowance did not hold preserved work"
  [ "$(rb_run_field "$run" recovery_wait_cause)" = worker_requeue_exhausted ] || fail "case 8: wrong recovery hold cause"
  [ -z "$(rb_run_field "$run" recovery_retry_not_before)" ] || fail "case 8: exhausted hold has an automatic retry"
  [ "$(rb_run_field "$run" claim_generation)" = "$executing" ] || fail "case 8: an extra claim reused the allowance"
  [ "$(rb_run_field "$run" finalize_resume_generation)" = "$original" ] || fail "case 8: finalize allowance marker changed"
  [ "$(rb_run_field "$run" requeue_count)" = "$((max + 1))" ] || fail "case 8: spent requeue count changed"
}
say "CASE 8: allowance is one-shot: G+1 capture park, G+2 finalize interrupted -> owner recovery hold, mark stays G"
make_outbox_run
RUN_F2="$OUTBOX_RUN"
GEN_F2="$(rb_run_field "$RUN_F2" claim_generation)"
say "first cut: run $RUN_F2 at generation $GEN_F2"
rc=0; f42_cut "$RUN_F2" "$GEN_F2" || rc=$?
if [ "$rc" = 0 ]; then
  f42_restart_cycle "$RUN_F2" "$GEN_F2" "$F42_MAX" 1
else
  api_back
  f42_cut_judge "$rc" "$RUN_F2" "$GEN_F2"
fi
f42_wait_reclaim "$RUN_F2" "$((GEN_F2 + 2))" 120
f42_assert_capture_park "$RUN_F2" "$((GEN_F2 + 1))"
[ "$(rb_run_field "$RUN_F2" finalize_resume_generation)" = "$GEN_F2" ] \
  || fail "case 8: after the first interruption finalize_resume_generation is '$(rb_run_field "$RUN_F2" finalize_resume_generation)', want $GEN_F2"
# The allowance is charged as a requeue, and the second cut below must NOT reset the counter downward.
[ "$(rb_run_field "$RUN_F2" requeue_count)" = "$((F42_MAX + 1))" ] \
  || fail "case 8: after the allowance requeue requeue_count is '$(rb_run_field "$RUN_F2" requeue_count)', want $((F42_MAX + 1)) (RUN_MAX_REQUEUES $F42_MAX + the one charged requeue)"
pass "case 8: run $RUN_F2 used the allowance, captured at G+1, and is executing at generation $((GEN_F2 + 2)) (finalize_resume_generation=$GEN_F2, requeue_count=$((F42_MAX + 1)))"
# Second cut at the proved EXECUTING G+2: G+1 was the retained-clone capture, not an executor turn.
GEN_F2B=$((GEN_F2 + 2))
say "second cut: run $RUN_F2 at generation $GEN_F2B"
rc=0; f42_cut "$RUN_F2" "$GEN_F2B" || rc=$?
if [ "$rc" = 0 ]; then
  f42_restart_cycle "$RUN_F2" "$GEN_F2B" "$F42_MAX" 0
else
  api_back
  f42_cut_judge "$rc" "$RUN_F2" "$GEN_F2B"
fi
# The allowance is spent. #2426 parks recoverable work instead of failing it;
# a second automatic allowance, completion or another claim remains a failure.
F42_DEADLINE=$((SECONDS + 120)); F42_ST=""
while [ "$SECONDS" -lt "$F42_DEADLINE" ]; do
  F42_ST="$(rb_run_field "$RUN_F2" status)"
  case "$F42_ST" in recovery_wait|failed|completed|cancelled) break;; esac
  sleep 0.5
done
assert_finalize_allowance_spent "$RUN_F2" "$GEN_F2" "$GEN_F2B" "$F42_MAX"
sleep 6 # three sweep ticks prove the hold does not automatically promote/reclaim
assert_finalize_allowance_spent "$RUN_F2" "$GEN_F2" "$GEN_F2B" "$F42_MAX"
pass "case 8: run $RUN_F2 holds preserved work at generation $GEN_F2B; allowance marker and count unchanged, no automatic retry"

# Recovery custody for the held run: `uzi run recovery` must never show a silent empty hold, i.e. an
# OPEN hold with neither an available capture nor a source_only/needs_action attention.
# Wait (bounded) until no hold is still `capturing`, so the judgment is on the settled custody state.
F42_DEADLINE=$((SECONDS + 60)); F42_REC=""; F42_CAPTURING=1
while [ "$SECONDS" -lt "$F42_DEADLINE" ]; do
  F42_REC="$(uzi_cli run recovery "$RUN_F2" --json)" || fail "case 8: uzi run recovery --json failed (exit $?)"
  F42_CAPTURING="$(printf '%s' "$F42_REC" | jq -r '[.[] | select(.state == "open" and .attention == "capturing")] | length')"
  [ "$F42_CAPTURING" = 0 ] && break
  sleep 2
done
[ "$F42_CAPTURING" = 0 ] \
  || fail "case 8: a custody hold on run $RUN_F2 was still capturing after 60s: $F42_REC"
[ "$(printf '%s' "$F42_REC" | jq -r 'length')" -gt 0 ] \
  || fail "case 8: uzi run recovery lists no custody hold at all for the held run $RUN_F2"
# Every OPEN hold is either an archive_ready one (with an available capture) or a source_only/needs_action one.
F42_SILENT="$(printf '%s' "$F42_REC" | jq -r '[.[] | select(.state == "open" and (((.attention == "archive_ready") and (.has_available_capture // false)) or (.attention | IN("source_only", "needs_action")) | not)) | .id] | join(",")')"
[ -z "$F42_SILENT" ] \
  || fail "case 8: uzi run recovery shows hold(s) on run $RUN_F2 that are neither archive_ready with a capture nor source_only/needs_action: $F42_SILENT"
F42_REC_TXT="$(uzi_cli run recovery "$RUN_F2" 2>&1)" || fail "case 8: uzi run recovery failed (exit $?): $F42_REC_TXT"
# Per open hold, the human output must carry the text matching its attention: archive_ready ->
# export hint, source_only -> custody line, needs_action -> discard hint.
for F42_ATT in $(printf '%s' "$F42_REC" | jq -r '[.[] | select(.state == "open") | .attention] | unique | .[]'); do
  case "$F42_ATT" in
    archive_ready) F42_WANT='uzi run export';;
    source_only) F42_WANT='no recovery archive; custody';;
    needs_action) F42_WANT='uzi run discard';;
    *) continue;;
  esac
  case "$F42_REC_TXT" in
    *"$F42_WANT"*) ;;
    *) fail "case 8: the human recovery output for run $RUN_F2 has an open $F42_ATT hold but lacks '$F42_WANT': $F42_REC_TXT";;
  esac
done
pass "case 8: uzi run recovery on run $RUN_F2 shows no silent empty hold ($(printf '%s' "$F42_REC" | jq -r 'length') hold(s) listed)"
# End only this test-owned held run. Cancellation keeps its recovery evidence;
# it also prevents a nonterminal fixture from leaking into later credential tests.
apipost "/api/runs/$RUN_F2/inputs" '{"kind":"cancel","body":""}' >/dev/null
wait_status "$RUN_F2" cancelled 30
pass "case 8: owner cancelled the test-owned recovery hold without discarding its source"

# =============================================================================
# RESTORE — return the api stale window and the agent outbox knobs to their defaults so
# later phases (59-restart-agent, 60-62) are unaffected. Mirrors phase 46's restore.
say "restore: recreate api + agent back to their defaults"
unset E2E_WORKER_HEARTBEAT_STALE WORKER_TRANSIENT_TRIP_MS WORKER_OUTBOX_RUN_MAX_BYTES UZI_STUB_OUTBOX_TICKS
"${COMPOSE[@]}" up -d --wait --no-deps --force-recreate api >/dev/null
wait_http
login
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
wait_worker_online
pass "api + agent recreated back to their defaults"

# Best-effort cleanup: every run is terminal (completed, or failed for the case 8 run), so a cancel is a
# no-op, but never let a cleanup blip redden a passed phase.
for r in "$RUN1" "$RUN2" "$RUN_B1" "$RUN_B2" "$RUN_B3" "$RUN_B4" "$RUN_F1" "$RUN_F2"; do
  [ -n "${r:-}" ] && apipost "/api/runs/$r/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true
done

fi
