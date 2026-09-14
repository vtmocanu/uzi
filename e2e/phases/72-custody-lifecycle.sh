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
# ClaimRun's owner-scoped custody-admission gate (custodyHoldLimit = 8) then refused
# every subsequent code-run claim for that owner — ten later phases timed out queued
# against a healthy, idle, freshly-registered worker, and direct database mutation was
# the only recovery path in production.
#
# This phase drives that exact predicate end to end through the LIVE claim loop, not the
# SQL in isolation (the store-level version is api/internal/store/recovery_exact_livedb_test.go
# and api/internal/workersvc/claim_custody_livedb_test.go). It:
#
#   WEDGE   — seeds the owner up to exactly the admission limit with open holds, creates
#             a code run, and shows it stays QUEUED against an online, idle worker while
#             the server's own custody aggregate reports it BLOCKED (blocked_runs >= 1 —
#             the SAME predicate ClaimRun and workersvc/health.go's reasonCustodyLimit
#             gate on). A worker being online + idle is the control that rules out every
#             non-custody queued reason.
#   UNBLOCK — disposes ONE exact hold through the real owner path shipped by M5
#             (`uzi run discard <run-id> --hold <hold-id> --yes`, i.e.
#             DELETE /api/runs/{id}/recovery-holds/{holdID}?confirm=discard), dropping
#             the owner below the limit, and proves the SAME run is then claimed and
#             reaches the plan gate (awaiting_approval) — so custody, and only custody,
#             was the block. The claim mints its own fresh hold (recovery-capable
#             worker), which the phase then confirms through the owner CLI list surface.
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
#   8 open holds block a 9th claim / aggregate (M1) api/internal/store/recovery_exact_livedb_test.go,
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

# The admission ceiling (workersvc.custodyHoldLimit / apitypes CustodyHoldLimit). The
# production claim path always passes this positive default, so the live stack enforces it.
LIMIT=8
SEED_IDENT="e2e-72-custody-seed"

# --- 0) preconditions: an online, idle worker + the owner below the limit -----
# The worker (phase 13, critical) stays online for the whole suite; the end-of-phase
# quarantine leaves it idle. Both are what make the wedge attributable to custody rather
# than to a missing/busy worker.
[ "$(worker_status)" = online ] || fail "custody phase needs an online worker (got '$(worker_status)')"

ADMIN_ID="$(db_psql "SELECT id FROM users WHERE email = '$ADMIN_EMAIL'")"
[ -n "$ADMIN_ID" ] || fail "could not resolve the admin owner id for '$ADMIN_EMAIL'"

open_holds_db() { db_psql "SELECT count(*) FROM recovery_custody_holds WHERE user_id = '$ADMIN_ID' AND state = 'open'"; }
C0="$(open_holds_db)"
# A suite that arrives already AT/OVER the limit is itself the isolation regression this
# work guards against (leaked holds from an abrupt kill wedging unrelated later phases).
# Fail loudly and name the FK-safe reset rather than silently seeding a no-op.
[ "$C0" -lt "$LIMIT" ] || fail "owner already has $C0 >= $LIMIT open custody holds at phase start — the suite arrived wedged (run e2e/reset-recovery-tables.sh at the throwaway-DB reset boundary)"
pass "preconditions: worker online + idle, owner has $C0 open hold(s) (< limit $LIMIT)"

# --- 1) WEDGE: seed open holds up to exactly the admission limit ---------------
# Seed only the shortfall, tagged with SEED_IDENT so cleanup targets exactly these rows.
# These are HOLD-only fixtures (no captures/chunks): they mirror how the live incident's
# abruptly-lost, capture-less sources sit 'open' awaiting an owner decision, and how
# api/internal/store/recovery_exact_livedb_test.go seeds the same admission fixture.
# live_worker_id/live_run_id stay NULL (no FK dependency); each row gets a distinct
# generation. run_id is a plain column here — a real owned run_id is attached to the ONE
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
# `uzi run recovery` all read it) must agree with the raw count and echo the ceiling.
AGG="$(apiget /api/recovery/holds)"
[ "$(printf '%s' "$AGG" | jq -r '.aggregate.open_holds')" = "$LIMIT" ] \
  || fail "aggregate.open_holds != $LIMIT (got $(printf '%s' "$AGG" | jq -r '.aggregate.open_holds'))"
[ "$(printf '%s' "$AGG" | jq -r '.aggregate.custody_hold_limit')" = "$LIMIT" ] \
  || fail "aggregate.custody_hold_limit != $LIMIT (got $(printf '%s' "$AGG" | jq -r '.aggregate.custody_hold_limit'))"
pass "owner aggregate agrees: open_holds=$LIMIT, custody_hold_limit=$LIMIT"

# --- 2) create a code run — it must WEDGE (stay queued, blocked by custody) ----
IID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E custody wedge","description":"implements prds/4-agent-runtime-workers.md — must stay queued while the owner is at the custody limit"}' \
  | jq -r '.card.iid')"
[ -n "$IID" ] && [ "$IID" != null ] || fail "could not create the wedge issue"
WEDGE_RUN="$(create_run "$REPO_ID" "$IID")" || fail "wedge run-create failed (non-transient; see stderr)"
[ -n "$WEDGE_RUN" ] && [ "$WEDGE_RUN" != null ] || fail "wedge run was not created"
pass "created code run $WEDGE_RUN for issue #$IID (owner at the custody limit)"

# NEGATIVE CONTROL: an unblocked claim lands in ~1-2s (worker polls every 500ms), so a
# run that stays 'queued' across a multi-second window against an online, idle worker is
# blocked. Poll ~12s; any departure from 'queued' is a wedge-repro failure.
NEG_WINDOW=12
start=$SECONDS
while [ $((SECONDS - start)) -lt "$NEG_WINDOW" ]; do
  s="$(apiget "/api/runs/$WEDGE_RUN" | jq -r '.run.status')"
  [ "$s" = queued ] || fail "wedge run left 'queued' (got '$s') while the owner is at the custody limit — the admission gate did not block the claim"
  sleep 0.5
done
record_margin "custody wedge: run stayed queued" "$((SECONDS - start))" "$NEG_WINDOW"
pass "wedge reproduced: run $WEDGE_RUN stayed queued for ${NEG_WINDOW}s against an online, idle worker"

# The server's OWN custody-block signal: blocked_runs counts the owner's queued
# code-publishing runs ONLY when open_holds >= the limit (recovery.sql GetCustodyAggregateForOwner,
# the same predicate ClaimRun and health.go's reasonCustodyLimit use). >= 1 here proves the
# run is queued FOR CUSTODY, not for a worker/capability/priority reason.
BLOCKED="$(apiget /api/recovery/holds | jq -r '.aggregate.blocked_runs')"
[ "$BLOCKED" -ge 1 ] || fail "aggregate.blocked_runs is $BLOCKED, want >= 1 — the server does not see the run as custody-blocked"
pass "server confirms the block: aggregate.blocked_runs=$BLOCKED (queued code run(s) held by the custody limit)"

# --- 3) UNBLOCK: dispose ONE exact hold through the real owner path (M5) -------
# The owner discard targets an exact hold on one of THEIR runs, so attach one seeded hold
# to the (real, owned) wedge run — a fixture detail that makes the seeded prior-work hold
# reachable through the shipped owner API against a real owned run id. Sibling seeds and
# the owner's baseline holds are untouched.
#
# NOT `RETURNING id`: db_psql is `psql -tAc … | tr -d '\r\n'`, so psql's command TAG is
# welded onto the returned row and yields `<uuid>UPDATE 1` — non-empty, passes a bare -n
# guard, and only breaks the later `uzi run recovery` id match. Same trap the phase-37/39
# fixtures document. Attach the row without RETURNING, then read the id back with a SELECT
# (the wedge run_id + seed identity uniquely name the just-attached hold) and assert its SHAPE.
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
DISCARD_OUT="$(uzi_cli run discard "$WEDGE_RUN" --hold "$HOLD_ID" --yes --json)" \
  || fail "uzi run discard failed (exit $?): $DISCARD_OUT"
[ "$(printf '%s' "$DISCARD_OUT" | jq -r '.discarded')" = true ] \
  || fail "uzi run discard did not report discarded=true (got: $DISCARD_OUT)"
pass "owner disposed the exact hold: uzi run discard $WEDGE_RUN --hold $HOLD_ID --yes → discarded"

# --- 4) PROVE the unblock: the SAME run now claims and reaches the plan gate ---
# With the owner below the limit, the still-online, still-idle worker claims the run
# within a poll and the stub drives it to awaiting_approval. Reaching the gate proves
# custody — and only custody — held it: nothing else about the worker or run changed.
wait_status "$WEDGE_RUN" awaiting_approval 60
pass "unblock proven: run $WEDGE_RUN was claimed and reached the plan gate after the disposition"

# The claim minted its own fresh hold (recovery-capable worker on a code kind), so the
# owner is protected again — and the wedge is cleared: no queued run remains blocked.
BLOCKED_AFTER="$(apiget /api/recovery/holds | jq -r '.aggregate.blocked_runs')"
[ "$BLOCKED_AFTER" = 0 ] || fail "aggregate.blocked_runs is $BLOCKED_AFTER after the unblock, want 0 (the episode did not clear)"
NEW_HOLD="$(uzi_cli run recovery "$WEDGE_RUN" --json | jq -r 'map(select(.state == "open")) | length')"
[ "$NEW_HOLD" -ge 1 ] || fail "the unblocked claim did not open a fresh custody hold for run $WEDGE_RUN"
pass "episode cleared: blocked_runs=0 and the claimed run holds its own fresh open custody hold"

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
