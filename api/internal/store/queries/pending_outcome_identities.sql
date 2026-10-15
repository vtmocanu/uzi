-- name: UpsertFrozenWorkerPendingOutcomeIdentity :execrows
-- Caller holds the worker, frozen parents and ordered snapshot/identity targets.
-- The unchanged orphan policy matches UpsertFrozenWorkerActiveRun.
INSERT INTO worker_pending_outcome_identities (
    worker_id, run_id, claim_generation, first_seen_at, reported_until
)
SELECT @worker_id, @run_id, @claim_generation,
       COALESCE(sqlc.narg(first_seen_at)::timestamptz, now()),
       now() + make_interval(secs => @lease_seconds::int)
WHERE EXISTS (
    SELECT 1 FROM runs r
    WHERE r.id = @run_id AND r.worker_id = @worker_id
      AND r.claim_generation = @claim_generation
      AND r.claim_released_at IS NULL
      AND r.status NOT IN ('completed', 'failed', 'cancelled')
      AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON r.kind = 'cross_check' AND cc.checker_run_id = r.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN r.kind = 'cross_check'
        THEN cc.lead_run_id ELSE r.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = r.user_id
    WHERE frozen.id = r.id
      AND r.kind IS NOT DISTINCT FROM frozen.kind
      AND r.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND r.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND r.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (r.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (r.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
      )
)
ON CONFLICT (worker_id, run_id, claim_generation) DO UPDATE SET
    first_seen_at = EXCLUDED.first_seen_at,
    reported_until = EXCLUDED.reported_until;

-- name: DeleteWorkerPendingOutcomeIdentitiesNotIn :execrows
-- Complete coverage is a tuple set, not a set of run IDs.
DELETE FROM worker_pending_outcome_identities p
WHERE p.worker_id = @worker_id
  AND NOT EXISTS (
    SELECT 1 FROM (SELECT unnest(@run_ids::uuid[]) AS run_id,
                          unnest(@claim_generations::bigint[]) AS claim_generation) keep
    WHERE keep.run_id = p.run_id AND keep.claim_generation = p.claim_generation
  );

-- name: PruneWorkerPendingOutcomeIdentities :execrows
DELETE FROM worker_pending_outcome_identities
WHERE worker_id = @worker_id AND reported_until <= now();

-- name: PruneExpiredPendingOutcomeIdentities :execrows
-- One bounded indexed batch per sweep; skip another transaction's row locks.
WITH expired AS (
    SELECT worker_id, run_id, claim_generation
    FROM worker_pending_outcome_identities
    WHERE reported_until <= @expired_at::timestamptz
    ORDER BY reported_until, worker_id, run_id, claim_generation
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM worker_pending_outcome_identities p USING expired e
WHERE p.worker_id = e.worker_id AND p.run_id = e.run_id
  AND p.claim_generation = e.claim_generation;

-- name: ListFreshWorkerPendingOutcomeSince :many
-- Capture before replacing active rows. Obsolete ownership/generation/release and
-- terminal evidence never seed continuity, even before physical expiry cleanup.
SELECT evidence.run_id, evidence.claim_generation, MIN(evidence.since)::timestamptz AS first_seen_at
FROM (
    SELECT p.run_id, p.claim_generation, p.first_seen_at AS since
    FROM worker_pending_outcome_identities p
    WHERE p.worker_id = @worker_id AND p.reported_until > now()
    UNION ALL
    SELECT a.run_id, a.claim_generation, a.terminal_pending_since AS since
    FROM worker_active_runs a
    WHERE a.worker_id = @worker_id AND a.terminal_pending
      AND a.terminal_pending_until > now() AND a.terminal_pending_since IS NOT NULL
) evidence
JOIN runs r ON r.id = evidence.run_id
WHERE r.worker_id = @worker_id AND r.claim_generation = evidence.claim_generation
  AND r.claim_released_at IS NULL AND r.status NOT IN ('completed', 'failed', 'cancelled')
GROUP BY evidence.run_id, evidence.claim_generation;

-- name: ListWorkerPendingOutcomeAggregates :many
-- M3 consumes this batched projection; no DTO wiring in M2.
WITH evidence AS (
    SELECT a.worker_id, a.run_id, a.claim_generation
    FROM worker_active_runs a
    WHERE a.worker_id = ANY(@worker_ids::uuid[]) AND a.terminal_pending
      AND a.terminal_pending_until > now()
    UNION
    SELECT p.worker_id, p.run_id, p.claim_generation
    FROM worker_pending_outcome_identities p
    WHERE p.worker_id = ANY(@worker_ids::uuid[]) AND p.reported_until > now()
), eligible AS (
    SELECT e.worker_id, e.run_id, e.claim_generation FROM evidence e
    JOIN runs r ON r.id = e.run_id AND r.worker_id = e.worker_id
      AND r.claim_generation = e.claim_generation AND r.claim_released_at IS NULL
      AND r.status NOT IN ('completed', 'failed', 'cancelled')
)
SELECT w.id AS worker_id, COUNT(e.run_id)::bigint AS pending_count,
       COALESCE(w.pending_overflow_until > now(), false)::boolean AS pending_overflow_fresh
FROM workers w LEFT JOIN eligible e ON e.worker_id = w.id
WHERE w.id = ANY(@worker_ids::uuid[])
GROUP BY w.id;
