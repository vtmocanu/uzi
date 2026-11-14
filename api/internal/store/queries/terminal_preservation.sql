-- Issue #4007: a terminal run's preservation ref refs/uzi-terminal/<run-id>
-- (run_terminal_preservations) and the per-generation contribution records pushed into it
-- (run_terminal_contributions), migrations 00448 and 00449. The guard triggers enforce the state
-- machines; every writer is a CAS on the expected state, so a lost race reads as 0 rows.
-- Lock order, wherever more than one is taken: worker, run, hold, preservation, contribution.

-- name: HasOpenGuardedHoldForWorker :one
-- The pre-body gate's hold predicate, which needs no generation: an open inventory-guarded hold of
-- the run whose original and live worker are both the caller. Unlocked and indexed
-- (idx_recovery_custody_holds_run); the generation-exact reads decide the publication itself.
SELECT EXISTS (
    SELECT 1 FROM recovery_custody_holds
    WHERE run_id = @run_id AND user_id = @user_id AND state = 'open' AND inventory_guarded
      AND original_worker_id = @worker_id::uuid AND live_worker_id = @worker_id::uuid
) AS held;

-- name: InsertTerminalPreservation :one
-- Insert-or-load by run_id. A conflict returns the EXISTING row, unchanged (the no-op update locks
-- it for the rest of the transaction), so the caller compares the returned identity against its own
-- and refuses on a mismatch rather than overwriting it.
INSERT INTO run_terminal_preservations (run_id, user_id, repo_id, ref, lifetime_at_bind)
VALUES (@run_id, @user_id, @repo_id, @ref, @lifetime_at_bind)
ON CONFLICT (run_id) DO UPDATE SET updated_at = run_terminal_preservations.updated_at
RETURNING *;

-- name: GetTerminalPreservationByRun :one
SELECT * FROM run_terminal_preservations WHERE run_id = @run_id;

-- name: InsertTerminalContribution :one
-- Insert-or-load by (run_id, generation), with the same conflict contract as
-- InsertTerminalPreservation. A row is born prepared, or recorded for an abandonment disposition.
INSERT INTO run_terminal_contributions
    (preservation_id, run_id, generation, worker_id, user_id, hold_id, authority, tip, prior_tip,
     coverage_digest, manifest, disposition, state)
VALUES (@preservation_id, @run_id, @generation, @worker_id, @user_id, sqlc.narg('hold_id'), @authority, @tip,
        sqlc.narg('prior_tip'), @coverage_digest, @manifest, @disposition,
        CASE WHEN @disposition::text = 'preserved' THEN 'prepared' ELSE 'recorded' END)
ON CONFLICT (run_id, generation) DO UPDATE SET updated_at = run_terminal_contributions.updated_at
RETURNING *;

-- name: GetTerminalContributionByRunGeneration :one
SELECT * FROM run_terminal_contributions WHERE run_id = @run_id AND generation = @generation;

-- name: ListTerminalContributionsForRun :many
-- Every contribution of the run, newest generation first.
SELECT * FROM run_terminal_contributions WHERE run_id = @run_id ORDER BY generation DESC;

-- name: ListTerminalPreservationsDue :many
-- The sweep's due list: rows whose next check has come (on the database clock) in a state that
-- still has work, oldest first. Served by idx_run_terminal_preservations_due.
SELECT * FROM run_terminal_preservations
WHERE next_check_at <= now() AND state IN ('bound', 'publishing', 'live', 'retiring')
ORDER BY next_check_at ASC, id ASC
LIMIT @lim::int;

-- name: CountLiveTerminalPreservationsForRepo :one
-- The repo-removal guard: rows whose live pointer references one of the repo's runs (exactly
-- the rows the runs.repo_id cascade would RESTRICT on). Owner-scoped through the connection.
SELECT count(*) FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE s.repo_id = @repo_id::uuid AND r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveTerminalRefsForRepo :many
-- What the repo-removal 409 names: the run, its preservation state and the terminal ref.
SELECT s.run_id, s.state, s.ref
FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE s.repo_id = @repo_id::uuid AND r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;

-- name: CountLiveTerminalPreservationsForConnection :one
-- The forge-connection-removal guard: live rows on any of the connection's repos.
SELECT count(*) FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveTerminalRefsForConnection :many
-- What the forge-connection-removal 409 names.
SELECT s.run_id, s.state, s.ref
FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;

-- ---------------------------------------------------------------------------------------------
-- Writers (m3). Every state or tip writer is a CAS: the expected state (and the old tip) are in the
-- WHERE, so a lost race and a guard-refused change (the guards RETURN NULL) both read as zero rows.
-- The caller rolls back on zero rows, re-reads under the run lock and logs "illegal transition" when
-- the row is still in the state it expected.
-- ---------------------------------------------------------------------------------------------

-- name: GetTerminalPreservationForUpdate :one
-- The locked read of the run's preservation (lock order: worker, run, hold, preservation,
-- contribution). The tx2 outcome writers take it after the run lock.
SELECT * FROM run_terminal_preservations WHERE run_id = @run_id FOR UPDATE;

-- name: GetTerminalPreservationForShare :one
-- The completed-publication gate's authoritative read (lock order: worker, run, hold, preservation,
-- contribution, repo binding). The gate is serialized by the run row lock it already holds: every
-- contribution writer takes the run row first, which also covers a contribution inserted after
-- this read. FOR SHARE is defence in depth.
SELECT * FROM run_terminal_preservations WHERE run_id = @run_id FOR SHARE;

-- name: GetTerminalContributionForShare :one
-- The contribution of exactly one generation, taken after the preservation row. No row means the
-- run never opted in at that generation, and the gate is a no-op.
SELECT * FROM run_terminal_contributions WHERE run_id = @run_id AND generation = @generation FOR SHARE;

-- name: MarkTerminalPreservationPublishing :one
-- bound, not_published, live or publishing -> publishing, setting the live pointer FIRST (before
-- the contribution's prepared -> invoked marker, whose guard needs the preservation publishing).
UPDATE run_terminal_preservations SET
    state = 'publishing', live_run_id = run_id, updated_at = now()
WHERE id = @id AND state = @expected_state::text
  AND tip IS NOT DISTINCT FROM sqlc.narg('expected_tip')::text
RETURNING *;

-- name: MarkTerminalPreservationNotPublished :one
-- bound -> not_published: the abandonment of a run that never had a ref.
UPDATE run_terminal_preservations SET state = 'not_published', updated_at = now()
WHERE id = @id AND state = 'bound' AND tip IS NULL
RETURNING *;

-- name: SettleTerminalPreservationLive :one
-- publishing -> live at a new tip: the tip advances only to the tip of a published contribution of
-- this preservation prepared against the old tip (the guard), and first_published_at is set once.
UPDATE run_terminal_preservations SET
    state = 'live', tip = @tip, first_published_at = COALESCE(first_published_at, now()),
    last_error = NULL, attempts = 0, next_check_at = @next_check_at, updated_at = now()
WHERE id = @id AND state = 'publishing'
  AND tip IS NOT DISTINCT FROM sqlc.narg('expected_tip')::text
RETURNING *;

-- name: SettleTerminalPreservationRefused :one
-- publishing -> live (it had a tip: the ref is as before) or not_published (it had none: no ref was
-- created), after the contribution was recorded refused. The text is already scrubbed and bounded.
UPDATE run_terminal_preservations SET
    state = CASE WHEN tip IS NULL THEN 'not_published' ELSE 'live' END,
    live_run_id = CASE WHEN tip IS NULL THEN NULL ELSE live_run_id END,
    last_error = sqlc.narg('last_error')::text, next_check_at = @next_check_at, updated_at = now()
WHERE id = @id AND state = 'publishing'
  AND tip IS NOT DISTINCT FROM sqlc.narg('expected_tip')::text
RETURNING *;

-- name: ScheduleTerminalPreservationCheck :execrows
-- A single-row scheduling write (no state, no tip, so it needs no run lock): when the sweep next
-- looks at the row, and the scrubbed outcome text of the last attempt. A NULL text keeps the
-- existing last_error (a scheduling write must not erase the last recorded failure).
UPDATE run_terminal_preservations SET
    next_check_at = @next_check_at, last_error = COALESCE(sqlc.narg('last_error')::text, last_error), updated_at = now()
WHERE id = @id;

-- name: MarkTerminalContributionInvoked :one
-- prepared -> invoked: the single winner of a push is the request this returns a row to (and whose
-- transaction then commits).
UPDATE run_terminal_contributions SET state = 'invoked', create_invoked_at = now(), updated_at = now()
WHERE id = @id AND state = 'prepared'
RETURNING *;

-- name: RecordTerminalContributionPublished :one
-- invoked or publish_unknown -> published: the ref was read back at the tip.
UPDATE run_terminal_contributions SET state = 'published', published_at = now(), updated_at = now()
WHERE id = @id AND state = @expected_state::text AND state IN ('invoked', 'publish_unknown')
RETURNING *;

-- name: RecordTerminalContributionRefused :one
-- invoked or publish_unknown -> refused, with the refusal reason.
UPDATE run_terminal_contributions SET state = 'refused', refusal_reason = @refusal_reason, updated_at = now()
WHERE id = @id AND state = @expected_state::text AND state IN ('invoked', 'publish_unknown')
RETURNING *;

-- name: RecordTerminalContributionUnknown :one
-- invoked -> publish_unknown: the outcome could not be proven, reconcile only.
UPDATE run_terminal_contributions SET state = 'publish_unknown', updated_at = now()
WHERE id = @id AND state = 'invoked'
RETURNING *;

-- ---------------------------------------------------------------------------------------------
-- The sweep (m4). Same CAS convention as the writers above; every state writer runs under the run
-- row lock (LockRunForHeldCleanup), taken before the preservation row.
-- ---------------------------------------------------------------------------------------------

-- name: GetTerminalSweepClock :one
-- The DATABASE clock, read immediately BEFORE the forge observation (the earliest, conservative
-- stamp): it is the retire decision's observed_at, compared in MarkTerminalPreservationRetiring
-- against the same clock, so a skewed
-- application clock cannot make a stale observation look fresh.
SELECT clock_timestamp()::timestamptz AS observed_at;

-- name: DeferTerminalPreservation :execrows
-- A scheduling-only write that may also count a failed attempt (attempt_bump 0 for a plain skip).
-- A NULL text keeps the existing last_error. It changes no state and no tip, so it needs no run lock;
-- the expected state keeps a stale item from rescheduling a row another writer already moved on.
UPDATE run_terminal_preservations SET
    next_check_at = @next_check_at,
    last_error = COALESCE(sqlc.narg('last_error')::text, last_error),
    attempts = attempts + @attempt_bump::int,
    updated_at = now()
WHERE id = @id AND state = @expected_state::text;

-- name: MarkTerminalPreservationAbandoned :one
-- publishing or retiring -> abandoned: the ref is at a foreign tip and is never touched again. The
-- live pointer is cleared (an abandoned row owns no ref).
UPDATE run_terminal_preservations SET
    state = 'abandoned', live_run_id = NULL, last_error = sqlc.narg('last_error')::text, updated_at = now()
WHERE id = @id AND state = @expected_state::text AND state IN ('publishing', 'retiring')
RETURNING *;

-- name: MarkTerminalPreservationDeleted :one
-- retiring -> deleted: the ref was CAS-deleted or confirmed absent. The live pointer is cleared.
UPDATE run_terminal_preservations SET
    state = 'deleted', live_run_id = NULL, deleted_at = now(), last_error = NULL, updated_at = now()
WHERE id = @id AND state = 'retiring'
RETURNING *;

-- name: MarkTerminalPreservationRetiring :one
-- live -> retiring, the irrevocable retirement decision (tx-R1). The observation must be fresh ON THE
-- DATABASE CLOCK at this statement: observed_at older than max_age_seconds matches no row, and the
-- caller discards the decision without writing. retire_decided_at is the same clock.
UPDATE run_terminal_preservations SET
    state = 'retiring', retire_tip = @tip::text, retire_reason = @retire_reason::text,
    retire_observed_at = @observed_at::timestamptz, retire_decided_at = clock_timestamp(),
    last_error = NULL, attempts = 0, next_check_at = now(), updated_at = now()
WHERE id = @id AND state = 'live' AND tip = @tip::text
  AND clock_timestamp() - @observed_at::timestamptz <= make_interval(secs => @max_age_seconds::float8)
RETURNING *;

-- name: GetTerminalRetireBlocker :one
-- The independent retirement protections, re-checked under the run lock in tx-R1. Returns the first
-- one that holds as a typed reason, or '' when none does. The reason is written to last_error, so it
-- is a closed vocabulary and never forge text.
SELECT COALESCE(
    (CASE WHEN r.status NOT IN ('completed', 'failed', 'cancelled') THEN 'run_nonterminal' END),
    (CASE WHEN EXISTS (SELECT 1 FROM worker_active_runs w WHERE w.run_id = r.id) THEN 'claim_live' END),
    (CASE WHEN r.fail_origin IN ('push_secret_blocked', 'worker_residue_blocked') THEN 'excluded_origin' END),
    (CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = r.id AND h.state = 'open')
        THEN 'custody_hold_open' END),
    (CASE WHEN EXISTS (SELECT 1 FROM run_terminal_contributions c
        WHERE c.run_id = r.id AND c.state IN ('prepared', 'invoked', 'publish_unknown'))
        THEN 'contribution_unresolved' END),
    (CASE WHEN EXISTS (SELECT 1 FROM runs o
        WHERE o.id <> r.id AND o.status NOT IN ('completed', 'failed', 'cancelled')
          AND (o.then_fix_of_run_id = r.id OR o.review_target_run_id = r.id
               OR o.target_run_id = r.id OR o.resume_of_run_id = r.id))
        THEN 'dependent_run_active' END),
    (CASE WHEN EXISTS (SELECT 1 FROM runs o
        WHERE o.id <> r.id AND o.repo_id = r.repo_id AND o.kind IN ('mr_rework', 'ci_fix')
          AND o.status NOT IN ('completed', 'failed', 'cancelled')
          AND ((r.mr_iid IS NOT NULL AND o.mr_iid = r.mr_iid)
               OR (r.branch IS NOT NULL AND o.pipeline_ref = r.branch)))
        THEN 'mr_run_active' END),
    (CASE WHEN EXISTS (SELECT 1 FROM cross_checks cc
        WHERE (cc.lead_run_id = r.id OR cc.checker_run_id = r.id)
          AND (cc.verdict = 'pending' OR EXISTS (SELECT 1 FROM runs o
              WHERE o.id IN (cc.lead_run_id, cc.checker_run_id) AND o.id <> r.id
                AND o.status NOT IN ('completed', 'failed', 'cancelled'))))
        THEN 'cross_check_active' END),
    ''
)::text AS blocker
FROM runs r WHERE r.id = @run_id;
