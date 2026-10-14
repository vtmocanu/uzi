-- Owner-confirmed discard of a worker-held run_not_owned terminal outcome (issue #2984). The
-- service layer is workersvc/outcome_discard.go. LOCK ORDER, every writer, no exceptions:
--   L0  LockWorkerForOutcomeDiscard   (workers row FOR KEY SHARE, first)
--   L1  LockWorkerOutcomeDiscards     (advisory, class + worker objid)
--   L2  worker_blocked_outcomes rows
--   L3  worker_outcome_discards rows
-- The PUT takes L0-L2 and never a directive row; authorize and /begin take L0-L3; /complete,
-- /reconcile and owner cancel take L0, L1 and L3. Run and user rows are only ever read plain.
-- No query here runs a network call and none runs inside the registration/heartbeat/claim tx.

-- name: LockWorkerForOutcomeDiscard :one
-- L0. FOR KEY SHARE conflicts only with DELETE and key updates of the worker row, so the
-- ordinary heartbeat UPDATE is not blocked while a delete of the worker (or its owner) waits.
-- No row means the worker was deleted: the caller stops.
SELECT 1::int AS present FROM workers WHERE id = @id FOR KEY SHARE;

-- name: LockWorkerOutcomeDiscards :exec
-- L1. Two-int advisory lock: class 1970958180 = 0x757A6F64 ("uzod"), the value of
-- store.WorkerOutcomeDiscardLockClass in migrate.go; TestWorkerOutcomeDiscardLockClassMatchesSQL
-- pins this literal to it. XACT-scoped, so it must run on a transaction-bound Queries.
SELECT pg_advisory_xact_lock(
    1970958180,
    hashtext(sqlc.arg(worker_id)::uuid::text)
);

-- name: DeleteWorkerBlockedOutcomes :exec
-- L2 for the wholesale-replace PUT: the DELETE takes the row locks.
DELETE FROM worker_blocked_outcomes WHERE worker_id = @worker_id;

-- name: InsertWorkerBlockedOutcome :exec
INSERT INTO worker_blocked_outcomes
    (worker_id, run_id, claim_generation, journal_sha256, blocked_reason, loss_complete,
     loss_sha256, incomplete_reason, loss, reported_at)
VALUES (@worker_id, @run_id, @claim_generation, @journal_sha256, @blocked_reason, @loss_complete,
        @loss_sha256, @incomplete_reason, sqlc.narg('loss')::jsonb, @reported_at);

-- name: GetWorkerBlockedOutcomeForUpdate :one
-- L2 for authorize and /begin.
SELECT * FROM worker_blocked_outcomes
WHERE worker_id = @worker_id AND run_id = @run_id AND claim_generation = @claim_generation
FOR UPDATE;

-- name: ListWorkerBlockedOutcomesForOwner :many
-- The owner view: a row is returned only when the caller owns BOTH the worker and the row's run,
-- evaluated in the query so a row of a run the caller does not own never leaves the database.
SELECT o.* FROM worker_blocked_outcomes o
JOIN workers w ON w.id = o.worker_id AND w.user_id = @user_id
JOIN runs r ON r.id = o.run_id AND r.user_id = @user_id
WHERE o.worker_id = @worker_id
ORDER BY o.run_id, o.claim_generation;

-- name: OpenCustodyHoldForWorkerRun :one
-- True while an open recovery custody hold of the run still points at this worker as its live
-- holder: the run's work may then be needed by the recovery path, so no discard.
SELECT EXISTS (
    SELECT 1 FROM recovery_custody_holds
    WHERE run_id = @run_id AND live_worker_id = @worker_id AND state = 'open') AS held;

-- name: ExpireOutcomeDiscardsForKey :exec
-- Authorize's stale-row expiry, inside the locked transaction and before the INSERT. An
-- authorized row past expires_at, or an executing row past lease_until plus the grace, becomes
-- expired. Time comes from the caller's clock.
UPDATE worker_outcome_discards
SET state = 'expired',
    result_reason = CASE WHEN state = 'executing' THEN 'lease_expired' ELSE 'authorization_expired' END,
    updated_at = @now
WHERE worker_id = @worker_id AND run_id = @run_id AND claim_generation = @claim_generation
  AND ((state = 'authorized' AND expires_at <= @now)
    OR (state = 'executing' AND lease_until + make_interval(secs => @grace_seconds::int) <= @now));

-- name: SupersedeAuthorizedOutcomeDiscards :exec
-- A still-authorized directive for the same journal whose confirmed tuple differs from the one
-- being authorized now is cancelled, so the insert below does not collide with a stale tuple.
UPDATE worker_outcome_discards
SET state = 'cancelled', result_reason = 'superseded', updated_at = @now
WHERE worker_id = @worker_id AND run_id = @run_id AND claim_generation = @claim_generation
  AND state = 'authorized'
  AND (journal_sha256 <> @journal_sha256 OR loss_sha256 <> @loss_sha256);

-- name: GetActiveOutcomeDiscardForKey :one
-- L3 read of the live directive of one journal (authorized or executing).
SELECT * FROM worker_outcome_discards
WHERE worker_id = @worker_id AND run_id = @run_id AND claim_generation = @claim_generation
  AND state IN ('authorized', 'executing')
FOR UPDATE;

-- name: InsertOutcomeDiscard :one
-- ON CONFLICT repeats the partial unique index predicate, which is how Postgres infers it.
INSERT INTO worker_outcome_discards
    (worker_id, run_id, claim_generation, journal_sha256, loss_sha256, confirmed_by, evidence,
     state, lease_until, expires_at, created_at, updated_at)
VALUES (@worker_id, @run_id, @claim_generation, @journal_sha256, @loss_sha256, @confirmed_by,
        @evidence, 'authorized', @expires_at, @expires_at, @now, @now)
ON CONFLICT (worker_id, run_id, claim_generation) WHERE state IN ('authorized', 'executing') DO NOTHING
RETURNING *;

-- name: GetOutcomeDiscard :one
SELECT * FROM worker_outcome_discards WHERE id = @id;

-- name: GetOutcomeDiscardForUpdate :one
-- L3.
SELECT * FROM worker_outcome_discards WHERE id = @id FOR UPDATE;

-- name: ExpireOutcomeDiscardsForWorker :many
-- Lazy expiry on the worker's read, under L0 and L1: every live directive of the worker past its
-- authorization deadline, or executing past lease_until plus the grace, becomes expired.
UPDATE worker_outcome_discards
SET state = 'expired',
    result_reason = CASE WHEN state = 'executing' THEN 'lease_expired' ELSE 'authorization_expired' END,
    updated_at = @now
WHERE worker_id = @worker_id
  AND ((state = 'authorized' AND expires_at <= @now)
    OR (state = 'executing' AND lease_until + make_interval(secs => @grace_seconds::int) <= @now))
RETURNING id;

-- name: ListLiveOutcomeDiscardsForWorker :many
SELECT * FROM worker_outcome_discards
WHERE worker_id = @worker_id AND state IN ('authorized', 'executing')
ORDER BY created_at, id;

-- name: BeginOutcomeDiscard :one
UPDATE worker_outcome_discards
SET state = 'executing', lease_until = @lease_until, updated_at = @now
WHERE id = @id AND state = 'authorized'
RETURNING *;

-- name: FinishOutcomeDiscard :one
-- Moves a directive to a final state from one of the allowed source states and records the
-- machine reason. No row means the directive was not in an allowed source state.
UPDATE worker_outcome_discards
SET state = @state, result_reason = @reason, updated_at = @now
WHERE id = @id AND state = ANY(@from_states::text[])
RETURNING *;

-- name: SweepExpireOutcomeDiscards :execrows
-- Retention sweep, expiry half. FOR UPDATE SKIP LOCKED never waits on a row an owner or worker
-- request holds, so the sweep cannot deadlock with the request locks above.
WITH due AS (
    SELECT id, state FROM worker_outcome_discards
    WHERE (state = 'authorized' AND expires_at <= @now)
       OR (state = 'executing' AND lease_until + make_interval(secs => @grace_seconds::int) <= @now)
    ORDER BY id
    LIMIT 500
    FOR UPDATE SKIP LOCKED
)
UPDATE worker_outcome_discards d
SET state = 'expired',
    result_reason = CASE WHEN due.state = 'executing' THEN 'lease_expired' ELSE 'authorization_expired' END,
    updated_at = @now
FROM due WHERE d.id = due.id;

-- name: SweepDeleteFinalOutcomeDiscards :execrows
-- Retention sweep: final directives older than the cutoff. Never updates worker_id.
WITH old AS (
    SELECT x.id FROM worker_outcome_discards x
    WHERE x.state IN ('completed', 'completed_unknown', 'refused', 'expired', 'cancelled')
      AND x.updated_at < @cutoff
    ORDER BY x.updated_at
    LIMIT 500
    FOR UPDATE SKIP LOCKED
)
DELETE FROM worker_outcome_discards d USING old WHERE d.id = old.id;

-- name: SweepDeleteStaleBlockedOutcomes :execrows
-- Retention sweep: reported outcomes not re-reported since the cutoff.
WITH old AS (
    SELECT x.worker_id, x.run_id, x.claim_generation FROM worker_blocked_outcomes x
    WHERE x.reported_at < @cutoff
    ORDER BY x.reported_at
    LIMIT 500
    FOR UPDATE SKIP LOCKED
)
DELETE FROM worker_blocked_outcomes o USING old
WHERE o.worker_id = old.worker_id AND o.run_id = old.run_id AND o.claim_generation = old.claim_generation;
