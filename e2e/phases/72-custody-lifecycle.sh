# shellcheck shell=bash
# phase:    custody-lifecycle
# title:    PRD #1349 M7: recovery custody-limit wedge repro + owner-disposition unblock
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID UZI_BIN UZI_TOKEN_VAL
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #1349 M7 — the dedicated custody-lifecycle regression. It reproduces the
# 2026-09-14 GitLab-lane wedge that #1349 fixes and PROVES the fix keeps later claims
# unblocked. The incident (GitHub Actions run 34813174866, sanitized): stub-failed code
# runs plus phase 45's SIGKILL/reclaim left EIGHT open custody holds for one owner, and
# ClaimRun's owner-scoped custody-admission gate (the default custody_hold_limit, 8) then refused
# every subsequent code-run claim for that owner — ten later phases timed out queued
# against a healthy, idle, freshly-registered worker, and direct database mutation was
# the only recovery path in production.
#
# This phase drives that exact predicate end to end through the LIVE claim loop, not the
# SQL in isolation (the store-level version is api/internal/store/recovery_exact_livedb_test.go
# and api/internal/workersvc/claim_custody_livedb_test.go). It:
#
#   WEDGE   — seeds the owner up to exactly the admission limit with open holds, creates
#             a code run, and shows it stays QUEUED with online + idle worker samples while
#             the server's own custody aggregate reports it BLOCKED (blocked_runs >= 1 —
#             the SAME predicate ClaimRun and workersvc/health.go's reasonCustodyLimit
#             gate on). Worker snapshots at entry, before the queued window and before
#             discard require online + idle; they do not establish continuous idleness.
#   UNBLOCK — disposes ONE exact hold through the real owner path shipped by M5
#             (`uzi run discard <run-id> --hold <hold-id> --yes`, i.e.
#             DELETE /api/runs/{id}/recovery-holds/{holdID}?confirm=discard), dropping
#             the owner below the limit, and proves the SAME run is then claimed and
#             reaches the plan gate (awaiting_approval), demonstrating the custody
#             unblock after the sampled worker preconditions. The recovery-capable
#             worker mints a fresh hold, confirmed through the owner CLI list surface.
#
# NOT harness cleanup. This phase proves PRODUCT behavior: the admission gate blocks at
# the limit, and the exact owner-disposition path frees a slot. The FK-safe throwaway-DB
# recovery-table reset (e2e/reset-recovery-tables.sh) is a SEPARATE, harness-isolation-only
# tool — never evidence for this contract; see that script's header.
#
# ── PRD #1349 safety-test matrix → existing coverage (this phase adds the full-stack
#    wedge/unblock; it deliberately does NOT duplicate the unit/live-DB scenarios below) ──
#   final-rejected empty turn → limit path (M3) .... agent/test/sdk-executor.test.ts,
#                                                    agent/test/limit.test.ts
#   empty-terminal RELEASE vs commit-then-fail
#     CAPTURE/RETAIN (M2) ......................... agent/test/runner-recovery-generation.test.ts
#   same-worker two-generation release (M4) ....... api/internal/workersvc/custody_multihold_livedb_test.go
#                                                    (TestSetStateCompletedSameWorkerMultiGenReleasesOnlyCurrentLiveDB)
#   generation-exact reserve/release (M1/M4) ...... api/internal/recovery/reserve_release_generation_livedb_test.go
#   default-limit (8) admission-counted holds block a claim (M1) api/internal/store/recovery_exact_livedb_test.go,
#                                                    api/internal/workersvc/claim_custody_livedb_test.go
#   owner discard + confirm gate + owner scope +
#     sibling safety + Bearer/cookie mount (M5) ... api/internal/handler/recovery_owner_holds_livedb_test.go
#   episode dedup + one-DM-per-episode (M6) ....... api/internal/slacksvc/custody_episode_test.go,
#                                                    api/internal/store/custody_episode_livedb_test.go
#   per-run custody nudge SUPPRESSED for the
#     episode reconciler (M6) ..................... api/internal/workersvc/health_custody_nudge_test.go
#   board alert + Workers resolution surface (M6) . web/src/components/CustodyBoardAlert.test.tsx,
#                                                    web/src/components/RecoveryHoldsSurface.test.tsx
#   No scenario in the matrix is uncovered; this phase is the integrated proof M7 asks for.
# =============================================================================
say "PRD #1349 M7: custody-limit wedge repro + owner-disposition unblock"
trap 'if [ "$?" -ne 0 ]; then cat "$RUNROOT/custody-window.jsonl" 2>/dev/null || true; inventory_diagnostic_snapshot || true; fi' EXIT

# The default of the custody_hold_limit admin setting (#3150); the e2e stack leaves it at 8.
# ADR-2445 uses admission_counted_holds, separate from total open_holds. This is a
# statement-snapshot gate, not a strict ceiling under concurrency or late staleness.
LIMIT=8
SEED_IDENT="e2e-72-custody-seed"

# Sample once per boundary; busy includes chats as well as active runs. Every
# worker must be idle, and at least one must be online. Row ordering is not a
# readiness signal. Do not wait for a busy worker:
# that would hide a broken phase-isolation precondition.
assert_custody_worker_idle() {
  local context="$1" snapshot observed
  snapshot="$(apiget /api/workers)" \
    || fail "$context: worker snapshot API error; status=unknown busy=unknown"
  observed="$(printf '%s' "$snapshot" | jq -cse '
    if length != 1 then error("expected one worker snapshot")
    else .[0] |
      if (.workers | type) == "array" and (.workers | length) > 0 then
        {status: .workers[0].status, busy: .workers[0].busy,
         online_count: ([.workers[] | select(.status == "online")] | length),
         busy_count: ([.workers[] | select(.busy == true)] | length),
         all_idle: all(.workers[]; .busy == false)}
      else error("expected a nonempty worker array") end
    end' 2>/dev/null)" \
    || fail "$context: invalid worker snapshot; status=unknown busy=unknown"
  printf '%s' "$observed" | jq -e '.online_count > 0 and .all_idle == true' >/dev/null \
    || fail "$context: needs online idle worker; observed $observed"
}

# --- 0) preconditions: sample an online, idle worker + owner below the limit --
assert_custody_worker_idle "custody phase entry"

ADMIN_ID="$(db_psql "SELECT id FROM users WHERE email = '$ADMIN_EMAIL'")"
[ -n "$ADMIN_ID" ] || fail "could not resolve the admin owner id for '$ADMIN_EMAIL'"

open_holds_db() { db_psql "SELECT count(*) FROM recovery_custody_holds WHERE user_id = '$ADMIN_ID' AND state = 'open'"; }
# Leave cross-phase source protection intact. With the real agent online, let
# pending final receipts settle before seeding this phase's own custody fixture.
wait_custody_headroom 1
# Headroom remains owner-wide/all-generation. Separately verify observed older
# held FINALs without discarding older holds or requiring archive FINALs to be held.
OLDER_HELD_RECEIPTS="$(db_psql_rows "SELECT h.run_id,h.generation
  FROM recovery_custody_holds h JOIN runs r ON r.id=h.run_id
  WHERE h.user_id='$ADMIN_ID' AND r.user_id=h.user_id AND h.state='released'
    AND h.final_disposition='held_publication' AND h.generation>0 AND h.generation<r.claim_generation
  ORDER BY h.run_id,h.generation")" || fail "could not observe earlier held receipts"
# One assertion per row in this finite snapshot, no retries; a failure blocks seeding.
while IFS='|' read -r older_run older_generation; do
  [ -n "$older_run" ] || continue
  assert_earlier_held_custody_receipt "$older_run" "$older_generation"
done <<< "$OLDER_HELD_RECEIPTS"
C0="$(open_holds_db)"
# A suite that arrives already AT/OVER the limit is itself the isolation regression this
# work guards against (leaked holds from an abrupt kill wedging unrelated later phases).
# Fail loudly and name the FK-safe reset rather than silently seeding a no-op.
[ "$C0" -lt "$LIMIT" ] || fail "owner already has $C0 >= $LIMIT open custody holds at phase start — the suite arrived wedged (run e2e/reset-recovery-tables.sh at the throwaway-DB reset boundary)"
pass "preconditions: worker sampled online + idle at entry, owner has $C0 open hold(s) (< limit $LIMIT)"

# --- 1) WEDGE: seed open holds up to exactly the admission limit ---------------
# Seed only the shortfall, tagged with SEED_IDENT so cleanup targets exactly these rows.
# These are HOLD-only fixtures (no captures/chunks): they mirror how the live incident's
# abruptly-lost, capture-less sources sit 'open' awaiting an owner decision, and how
# api/internal/store/recovery_exact_livedb_test.go seeds the same admission fixture.
# live_worker_id/live_run_id stay NULL (no FK dependency), so ADR-2445 keeps every
# seed admission-counted: no seed proves an exact healthy live current-generation claim.
# Each row gets a distinct generation. run_id is a plain column here — a real owned run_id is attached to the ONE
# hold that gets discarded, below.
NEED=$((LIMIT - C0))
db_psql "INSERT INTO recovery_custody_holds
           (user_id, run_id, generation, state, original_worker_id, original_worker_identity)
         SELECT '$ADMIN_ID', gen_random_uuid(), g, 'open', gen_random_uuid(), '$SEED_IDENT'
         FROM generate_series(1, $NEED) AS g" >/dev/null
AFTER_SEED="$(open_holds_db)"
[ "$AFTER_SEED" = "$LIMIT" ] || fail "seeding did not bring the owner to exactly $LIMIT open holds (got $AFTER_SEED)"
pass "seeded $NEED open custody hold(s); owner now at the admission limit ($AFTER_SEED/$LIMIT)"

# The owner-wide aggregate (GET /api/recovery/holds — the board alert / Workers surface /
# `uzi run recovery` all read it) must agree with total custody, separately establish
# admission pressure (including the baseline holds), and echo the configured limit (default 8).
AGG="$(apiget /api/recovery/holds)"
[ "$(printf '%s' "$AGG" | jq -r '.aggregate.open_holds')" = "$LIMIT" ] \
  || fail "aggregate.open_holds != $LIMIT (got $(printf '%s' "$AGG" | jq -r '.aggregate.open_holds'))"
[ "$(printf '%s' "$AGG" | jq -r '.aggregate.custody_hold_limit')" = "$LIMIT" ] \
  || fail "aggregate.custody_hold_limit != $LIMIT (got $(printf '%s' "$AGG" | jq -r '.aggregate.custody_hold_limit'))"
[ "$(printf '%s' "$AGG" | jq -r '.aggregate.admission_counted_holds')" = "$LIMIT" ] \
  || fail "aggregate.admission_counted_holds != $LIMIT (got $(printf '%s' "$AGG" | jq -r '.aggregate.admission_counted_holds'))"
pass "owner aggregate agrees: open_holds=$LIMIT, admission_counted_holds=$LIMIT, custody_hold_limit=$LIMIT"

# --- 2) create a code run — it must WEDGE (stay queued, blocked by custody) ----
IID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E custody wedge","description":"implements prds/4-agent-runtime-workers.md — must stay queued while the owner is at the custody limit"}' \
  | jq -r '.card.iid')"
[ -n "$IID" ] && [ "$IID" != null ] || fail "could not create the wedge issue"
WEDGE_RUN="$(create_run "$REPO_ID" "$IID")" || fail "wedge run-create failed (non-transient; see stderr)"
[ -n "$WEDGE_RUN" ] && [ "$WEDGE_RUN" != null ] || fail "wedge run was not created"
pass "created code run $WEDGE_RUN for issue #$IID (owner at the custody limit)"

# NEGATIVE CONTROL: an unblocked claim lands in ~1-2s (worker polls every 500ms).
# Sample online + idle before the window and again before disposition. These are
# boundary observations, not continuous worker monitoring. Poll ~12s; any departure
# from 'queued' is a wedge-repro failure.
assert_custody_worker_idle "custody negative window"
NEG_WINDOW=12
start=$SECONDS
while [ $((SECONDS - start)) -lt "$NEG_WINDOW" ]; do
  # Record the actual admission baseline, including receipt transitions. Print
  # only on failure; the retained rundir also carries the rows for post-mortem.
  db_psql_rows "SELECT json_build_object('kind','custody_window','observed_at',clock_timestamp(),
    'hold_id',h.id,'run_id',h.run_id,'generation',h.generation,'state',h.state,
    'final_disposition',h.final_disposition,'release_evidence',h.release_evidence,
    'released_at',h.released_at,'updated_at',h.updated_at,
    'open_count',(SELECT count(*) FROM recovery_custody_holds WHERE user_id='$ADMIN_ID' AND state='open'),
    'wedge_run','$WEDGE_RUN','wedge_status',(SELECT status FROM runs WHERE id='$WEDGE_RUN'))::text
    FROM recovery_custody_holds h WHERE h.user_id='$ADMIN_ID' ORDER BY h.id" >> "$RUNROOT/custody-window.jsonl"
  s="$(apiget "/api/runs/$WEDGE_RUN" | jq -r '.run.status')"
  [ "$s" = queued ] || fail "wedge run left 'queued' (got '$s') while the owner is at the custody limit — the admission gate did not block the claim"
  sleep 0.5
done
record_margin "custody wedge: run stayed queued" "$((SECONDS - start))" "$NEG_WINDOW"
pass "wedge reproduced: run $WEDGE_RUN stayed queued for ${NEG_WINDOW}s (worker sampled online + idle before the window)"

# The server's OWN custody-block signal: blocked_runs counts the owner's queued
# non-exempt code-publishing runs when admission_counted_holds >= the limit
# (recovery.sql GetCustodyAggregateForOwner, the same accounting ClaimRun and health.go's
# reasonCustodyLimit use). Continuation's own-hold bound still uses total open holds.
# >= 1 here establishes the server-observed custody block; the worker checks
# establish online + idle only at the sampled boundaries.
BLOCKED="$(apiget /api/recovery/holds | jq -r '.aggregate.blocked_runs')"
[ "$BLOCKED" -ge 1 ] || fail "aggregate.blocked_runs is $BLOCKED, want >= 1 — the server does not see the run as custody-blocked"
pass "server confirms the block: aggregate.blocked_runs=$BLOCKED (queued code run(s) held by the custody limit)"

# --- 3) UNBLOCK: dispose ONE exact hold through the real owner path (M5) -------
# The owner discard targets an exact hold on one of THEIR runs, so attach one seeded hold
# to the (real, owned) wedge run — a fixture detail that makes the seeded prior-work hold
# reachable through the shipped owner API against a real owned run id. Sibling seeds and
# the owner's baseline holds are untouched.
#
# db_psql now strips psql's command TAG at the chokepoint (#1351), so `RETURNING id` no longer
# welds `<uuid>UPDATE 1` onto the returned row. Kept as a separate read-back SELECT plus a
# uuid-SHAPE assertion for defense-in-depth (the wedge run_id + seed identity uniquely name the
# just-attached hold), independent of the helper's tag handling.
db_psql "UPDATE recovery_custody_holds SET run_id = '$WEDGE_RUN'
         WHERE id = (SELECT id FROM recovery_custody_holds
                       WHERE user_id = '$ADMIN_ID' AND original_worker_identity = '$SEED_IDENT' AND state = 'open'
                       LIMIT 1)" >/dev/null
HOLD_ID="$(db_psql "SELECT id FROM recovery_custody_holds
                      WHERE run_id = '$WEDGE_RUN' AND original_worker_identity = '$SEED_IDENT' AND state = 'open'")"
printf '%s' "$HOLD_ID" | grep -qE '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' \
  || fail "could not attach a seeded hold to the wedge run for discard (got: '$HOLD_ID')"

# The owner CLI list surface (M5): `uzi run recovery <run-id> --json` narrows the
# owner-wide holds to this run and must show the exact hold, open, before disposition.
REC_JSON="$(uzi_cli run recovery "$WEDGE_RUN" --json)" || fail "uzi run recovery --json failed (exit $?)"
[ "$(printf '%s' "$REC_JSON" | jq -r --arg h "$HOLD_ID" 'map(select(.id == $h and .state == "open")) | length')" = 1 ] \
  || fail "uzi run recovery did not list the open hold $HOLD_ID on run $WEDGE_RUN"
pass "owner CLI lists the exact open hold $HOLD_ID on run $WEDGE_RUN"

# The disposition itself: exact hold discard, non-interactively with --yes (no TTY).
assert_custody_worker_idle "custody pre-discard"
DISCARD_OUT="$(uzi_cli run discard "$WEDGE_RUN" --hold "$HOLD_ID" --yes --json)" \
  || fail "uzi run discard failed (exit $?): $DISCARD_OUT"
[ "$(printf '%s' "$DISCARD_OUT" | jq -r '.discarded')" = true ] \
  || fail "uzi run discard did not report discarded=true (got: $DISCARD_OUT)"
pass "owner disposed the exact hold: uzi run discard $WEDGE_RUN --hold $HOLD_ID --yes → discarded"

# --- 4) PROVE the unblock: the SAME run now claims and reaches the plan gate ---
# With the owner below the limit, the worker can claim the run and the stub drives
# it to awaiting_approval. A claim legitimately makes the worker busy, so the idle
# assertion belongs before discard, while custody still blocks the run.
wait_status "$WEDGE_RUN" awaiting_approval 60
pass "unblock proven: run $WEDGE_RUN was claimed and reached the plan gate after the disposition"

# The claim minted its own fresh hold (recovery-capable worker on a code kind), so
# total custody is back at LIMIT. The healthy current-generation non-decision hold at
# awaiting_approval is excluded from admission: counted holds stay at LIMIT-1. The
# hold is still open and protects source; accounting has released/discarded nothing.
# The wedge is cleared: no queued run remains custody-blocked.
BLOCKED_AFTER="$(apiget /api/recovery/holds | jq -r '.aggregate.blocked_runs')"
[ "$BLOCKED_AFTER" = 0 ] || fail "aggregate.blocked_runs is $BLOCKED_AFTER after the unblock, want 0 (the episode did not clear)"
NEW_HOLD="$(uzi_cli run recovery "$WEDGE_RUN" --json | jq -r 'map(select(.state == "open")) | length')"
[ "$NEW_HOLD" -ge 1 ] || fail "the unblocked claim did not open a fresh custody hold for run $WEDGE_RUN"
AGG_AFTER="$(apiget /api/recovery/holds)"
[ "$(printf '%s' "$AGG_AFTER" | jq -r '.aggregate.open_holds')" = "$LIMIT" ] \
  || fail "aggregate.open_holds != $LIMIT after the healthy claim (got $(printf '%s' "$AGG_AFTER" | jq -r '.aggregate.open_holds'))"
[ "$(printf '%s' "$AGG_AFTER" | jq -r '.aggregate.admission_counted_holds')" = "$((LIMIT - 1))" ] \
  || fail "aggregate.admission_counted_holds != $((LIMIT - 1)) after the healthy claim (got $(printf '%s' "$AGG_AFTER" | jq -r '.aggregate.admission_counted_holds'))"
pass "episode cleared: blocked_runs=0, open_holds=$LIMIT, admission_counted_holds=$((LIMIT - 1)); fresh custody remains open"

# --- 5) cleanup: leave the owner as we found it (self-cleaning; 72 is the last phase) ---
# Cancel the wedge run so the end-of-phase quarantine finds nothing to reap, then remove
# ONLY this phase's seed fixtures (matched by SEED_IDENT, in any state). The seeds carry
# no captures/chunks, so a plain delete is FK-safe; the standalone reset script is for the
# throwaway store-it/e2e DB boundary, not this in-suite fixture cleanup.
apipost "/api/runs/$WEDGE_RUN/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true
( wait_status "$WEDGE_RUN" cancelled 30 ) || true
db_psql "DELETE FROM recovery_custody_holds WHERE user_id = '$ADMIN_ID' AND original_worker_identity = '$SEED_IDENT'" >/dev/null
FINAL="$(open_holds_db)"
pass "cleanup: wedge run cancelled, $NEED seeded hold(s) removed (owner open holds now $FINAL)"
