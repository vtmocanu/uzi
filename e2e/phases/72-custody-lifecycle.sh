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
#   WEDGE   — seeds the owner with LIMIT open holds of its own, so admission-counted
#             holds are at or above the limit independent of baseline settlement (an
#             idle worker opens no new claim holds, so the baseline can only shrink as
#             earlier phases' holds settle). It creates a code run, and shows it
#             stays QUEUED with online + idle worker samples while
#             the server's own custody aggregate reports it BLOCKED (blocked_runs >= 1 —
#             the SAME predicate ClaimRun and workersvc/health.go's reasonCustodyLimit
#             gate on). Worker snapshots at entry, before the queued window and before
#             discard require online + idle; they do not establish continuous idleness.
#   UNBLOCK — disposes exactly K seeded holds through the real owner path shipped by M5
#             (`uzi run discard <run-id> --hold <hold-id> --yes`, i.e.
#             DELETE /api/runs/{id}/recovery-holds/{holdID}?confirm=discard), where
#             K = admission_counted_holds - LIMIT + 1 is read from one aggregate snapshot,
#             dropping the owner below the limit, and proves the SAME run is then claimed and
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
#                                                    api/internal/workersvc/claim_custody_livedb_test.go,
#                                                    api/internal/workersvc/claim_custody_limit_setting_livedb_test.go
#                                                    (exact boundaries: blocked at the limit, admitted
#                                                    at limit-1). This full-stack phase proves the exact
#                                                    boundary only when the baseline is empty.
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
# Remove this phase's own seeds on failure: later phases (73-76) run after this one and
# would otherwise inherit the seeded custody. Skips when the owner was never resolved.
custody_seed_cleanup() {
  [ -n "${ADMIN_ID:-}" ] || return 0
  [ -n "${SEED_IDENT:-}" ] || return 0
  # Cancel the wedge run first (when it exists) so it is not left claimable once the seeds go.
  [ -n "${WEDGE_RUN:-}" ] && { apipost "/api/runs/$WEDGE_RUN/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true; }
  db_psql "DELETE FROM recovery_custody_holds WHERE user_id = '$ADMIN_ID' AND original_worker_identity = '${SEED_IDENT:-}'" >/dev/null 2>&1 || true
}
trap 'if [ "$?" -ne 0 ]; then cat "$RUNROOT/custody-window.jsonl" 2>/dev/null || true; inventory_diagnostic_snapshot || true; custody_seed_cleanup || true; fi' EXIT

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

# custody_aggregate: ONE aggregate snapshot, numerically validated (as
# wait_custody_headroom in lib.sh does); prints "counted open limit".
custody_aggregate() {
  local snapshot
  # fail prints to stdout and exits; inside the caller's command substitution that text would be
  # swallowed, so send it to stderr where the log keeps it.
  snapshot="$(apiget /api/recovery/holds)" || fail "custody aggregate: API error" >&2
  printf '%s' "$snapshot" | jq -er '.aggregate | [.admission_counted_holds, .open_holds, .custody_hold_limit]
    | if all(.[]; type == "number" and . >= 0 and . == floor) then map(tostring) | join(" ")
      else error("invalid") end' \
    || fail "custody aggregate: invalid admission_counted_holds/open_holds/custody_hold_limit" >&2
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
pass "preconditions: worker sampled online + idle at entry, owner has $C0 baseline open hold(s) (< limit $LIMIT)"

# --- 1) WEDGE: seed LIMIT open holds, regardless of the baseline ---------------
# Seed a full LIMIT of our own, tagged with SEED_IDENT so cleanup targets exactly these rows.
# The baseline (C0 holds from earlier phases, e.g. one pending FINAL): an idle worker opens no
# new claim holds, so it can only shrink as those holds settle, never grow: seeding LIMIT keeps
# admission_counted_holds >= LIMIT independent of any baseline release.
# These are HOLD-only fixtures (no captures/chunks): they mirror how the live incident's
# abruptly-lost, capture-less sources sit 'open' awaiting an owner decision, and how
# api/internal/store/recovery_exact_livedb_test.go seeds the same admission fixture.
# live_worker_id/live_run_id stay NULL (no FK dependency), so ADR-2445 keeps every
# seed admission-counted: no seed proves an exact healthy live current-generation claim.
# Each row gets a distinct generation. run_id is a plain column here: real owned run ids
# are attached to the holds that get discarded, below.
NEED=$LIMIT
db_psql "INSERT INTO recovery_custody_holds
           (user_id, run_id, generation, state, original_worker_id, original_worker_identity)
         SELECT '$ADMIN_ID', gen_random_uuid(), g, 'open', gen_random_uuid(), '$SEED_IDENT'
         FROM generate_series(1, $NEED) AS g" >/dev/null
SEEDED="$(db_psql "SELECT count(*) FROM recovery_custody_holds
                     WHERE user_id = '$ADMIN_ID' AND original_worker_identity = '$SEED_IDENT' AND state = 'open'")"
[ "$SEEDED" = "$LIMIT" ] || fail "seeding did not create exactly $LIMIT open seed holds (got '$SEEDED')"

# The owner-wide aggregate (GET /api/recovery/holds, read by the board alert / Workers surface /
# `uzi run recovery`) must show admission pressure at or above the limit (baseline included),
# agree with total custody, and echo the configured limit (default 8). One snapshot.
AGG_LINE="$(custody_aggregate)" || fail "custody aggregate unreadable"
read -r AGG_COUNTED AGG_OPEN AGG_LIMIT <<< "$AGG_LINE"
[ "$AGG_COUNTED" -ge "$LIMIT" ] || fail "aggregate.admission_counted_holds=$AGG_COUNTED < $LIMIT after seeding $LIMIT holds"
[ "$AGG_OPEN" = "$AGG_COUNTED" ] || fail "aggregate.open_holds=$AGG_OPEN != admission_counted_holds=$AGG_COUNTED (every open hold should be admission-counted here)"
[ "$AGG_LIMIT" = "$LIMIT" ] || fail "aggregate.custody_hold_limit=$AGG_LIMIT != $LIMIT"
pass "seeded $NEED open custody hold(s) over $C0 baseline hold(s); aggregate: admission_counted_holds=$AGG_COUNTED open_holds=$AGG_OPEN custody_hold_limit=$AGG_LIMIT"

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

# --- 3) UNBLOCK: dispose exactly K seeded holds through the real owner path (M5) ---
# Dropping below the limit needs K = counted - LIMIT + 1 discards. K comes from one validated
# aggregate snapshot. An idle worker opens no new claim holds, so the baseline can only shrink
# as earlier holds settle; counted can only fall from here and K is an upper bound: if counted grew instead, the unblock wait below
# fails (never a false pass).
AGG_LINE="$(custody_aggregate)" || fail "custody aggregate unreadable"
read -r UNB_COUNTED UNB_OPEN _ <<< "$AGG_LINE"
K=$((UNB_COUNTED - LIMIT + 1))
{ [ "$K" -ge 1 ] && [ "$K" -le "$LIMIT" ]; } \
  || fail "unblock needs K=$K discards (counted=$UNB_COUNTED open=$UNB_OPEN limit=$LIMIT), want 1 <= K <= $LIMIT"

# The owner discard targets an exact hold on one of THEIR runs, so attach K seeded holds
# to the (real, owned) wedge run: a fixture detail that makes the seeded prior-work holds
# reachable through the shipped owner API against a real owned run id. Sibling seeds and
# the owner's baseline holds are untouched.
db_psql "UPDATE recovery_custody_holds SET run_id = '$WEDGE_RUN'
         WHERE id IN (SELECT id FROM recovery_custody_holds
                       WHERE user_id = '$ADMIN_ID' AND original_worker_identity = '$SEED_IDENT' AND state = 'open'
                       ORDER BY id LIMIT $K)" >/dev/null
# db_psql_rows (not db_psql): db_psql strips newlines and would fuse the rows.
HOLD_IDS_RAW="$(db_psql_rows "SELECT id FROM recovery_custody_holds
                      WHERE user_id = '$ADMIN_ID' AND run_id = '$WEDGE_RUN'
                        AND original_worker_identity = '$SEED_IDENT' AND state = 'open'
                      ORDER BY id")" || fail "could not read back the attached seed holds"
HOLD_IDS=()
while IFS= read -r hold_line; do
  [ -n "$hold_line" ] || continue
  [[ "$hold_line" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] \
    || fail "attached seed hold row is not a uuid: '$hold_line'"
  HOLD_IDS+=("$hold_line")
done <<< "$HOLD_IDS_RAW"
[ "${#HOLD_IDS[@]}" -eq "$K" ] || fail "attached ${#HOLD_IDS[@]} seed holds to the wedge run, want exactly K=$K"

# The owner CLI list surface (M5): `uzi run recovery <run-id> --json` narrows the
# owner-wide holds to this run and must show exactly the attached ids, open, before disposition.
REC_JSON="$(uzi_cli run recovery "$WEDGE_RUN" --json)" || fail "uzi run recovery --json failed (exit $?)"
WANT_IDS="$(printf '%s\n' "${HOLD_IDS[@]}" | jq -R . | jq -sc 'sort')"
printf '%s' "$REC_JSON" | jq -e --argjson want "$WANT_IDS" \
  'map({id, state}) | sort_by(.id) == ($want | map({id: ., state: "open"}))' >/dev/null \
  || fail "uzi run recovery did not list exactly the $K open attached hold(s) on run $WEDGE_RUN (want $WANT_IDS; got $REC_JSON)"
pass "owner CLI lists exactly the $K attached open hold(s) on run $WEDGE_RUN"

# The disposition itself: exact hold discards, non-interactively with --yes (no TTY).
assert_custody_worker_idle "custody pre-discard"
for HOLD_ID in "${HOLD_IDS[@]}"; do
  DISCARD_OUT="$(uzi_cli run discard "$WEDGE_RUN" --hold "$HOLD_ID" --yes --json)" \
    || fail "uzi run discard failed (exit $?): $DISCARD_OUT"
  [ "$(printf '%s' "$DISCARD_OUT" | jq -r '.discarded')" = true ] \
    || fail "uzi run discard did not report discarded=true (got: $DISCARD_OUT)"
done
pass "owner disposed $K exact hold(s): uzi run discard $WEDGE_RUN --hold <id> --yes -> discarded"

# --- 4) PROVE the unblock: the SAME run now claims and reaches the plan gate ---
# With the owner below the limit, the worker can claim the run and the stub drives
# it to awaiting_approval. A claim legitimately makes the worker busy, so the idle
# assertion belongs before discard, while custody still blocks the run.
wait_status "$WEDGE_RUN" awaiting_approval 60
pass "unblock proven: run $WEDGE_RUN was claimed and reached the plan gate after the disposition"

# The claim minted its own fresh hold (recovery-capable worker on a code kind). The healthy
# current-generation non-decision hold at awaiting_approval is open but excluded from admission,
# so open_holds exceeds admission_counted_holds by exactly that one and the counted holds sit
# below the limit. The hold is still open and protects source; nothing was released/discarded.
# The wedge is cleared: no queued run remains custody-blocked.
BLOCKED_AFTER="$(apiget /api/recovery/holds | jq -r '.aggregate.blocked_runs')"
[ "$BLOCKED_AFTER" = 0 ] || fail "aggregate.blocked_runs is $BLOCKED_AFTER after the unblock, want 0 (the episode did not clear)"
# Identify the fresh hold without provenance in any owner DTO (original_worker_identity is
# never exposed): owner- and run-scoped, open, and not one of this phase's seeds.
FRESH_RAW="$(db_psql_rows "SELECT id FROM recovery_custody_holds
                      WHERE user_id = '$ADMIN_ID' AND run_id = '$WEDGE_RUN' AND state = 'open'
                        AND original_worker_identity IS DISTINCT FROM '$SEED_IDENT'
                      ORDER BY id")" || fail "could not read the fresh hold of run $WEDGE_RUN"
FRESH_IDS=()
while IFS= read -r fresh_line; do
  [ -n "$fresh_line" ] || continue
  [[ "$fresh_line" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] \
    || fail "fresh hold row is not a uuid: '$fresh_line'"
  FRESH_IDS+=("$fresh_line")
done <<< "$FRESH_RAW"
[ "${#FRESH_IDS[@]}" -eq 1 ] || fail "run $WEDGE_RUN has ${#FRESH_IDS[@]} open non-seed holds, want exactly 1 fresh hold"
FRESH_ID="${FRESH_IDS[0]}"
uzi_cli run recovery "$WEDGE_RUN" --json \
  | jq -e --arg h "$FRESH_ID" 'map(select(.id == $h and .state == "open")) | length == 1' >/dev/null \
  || fail "uzi run recovery did not list the fresh open hold $FRESH_ID on run $WEDGE_RUN"
AGG_LINE="$(custody_aggregate)" || fail "custody aggregate unreadable"
read -r AFTER_COUNTED AFTER_OPEN _ <<< "$AGG_LINE"
[ $((AFTER_OPEN - AFTER_COUNTED)) -eq 1 ] \
  || fail "open_holds - admission_counted_holds = $((AFTER_OPEN - AFTER_COUNTED)) after the healthy claim, want 1 (open=$AFTER_OPEN counted=$AFTER_COUNTED)"
[ "$AFTER_COUNTED" -lt "$LIMIT" ] || fail "admission_counted_holds=$AFTER_COUNTED still >= $LIMIT after the unblock"
pass "episode cleared: blocked_runs=0, open_holds=$AFTER_OPEN, admission_counted_holds=$AFTER_COUNTED (< $LIMIT); fresh hold $FRESH_ID remains open"

# --- 5) cleanup: leave the owner as we found it (self-cleaning; phases 73-76 run after this one) ---
# Cancel the wedge run so the end-of-phase quarantine finds nothing to reap, then remove
# ONLY this phase's seed fixtures (matched by SEED_IDENT, in any state). The seeds carry
# no captures/chunks, so a plain delete is FK-safe; the standalone reset script is for the
# throwaway store-it/e2e DB boundary, not this in-suite fixture cleanup.
apipost "/api/runs/$WEDGE_RUN/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true
( wait_status "$WEDGE_RUN" cancelled 30 ) || true
db_psql "DELETE FROM recovery_custody_holds WHERE user_id = '$ADMIN_ID' AND original_worker_identity = '$SEED_IDENT'" >/dev/null
FINAL="$(open_holds_db)"
pass "cleanup: wedge run cancelled, $NEED seeded hold(s) removed (owner open holds now $FINAL)"
