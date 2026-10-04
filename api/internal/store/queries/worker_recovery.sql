-- name: LockFrozenWorkerSnapshotRuns :many
-- Internal transaction seam: LockWorkerRecoveryParents has already acquired the
-- complete parent set. FrozenTargets is its server-generated identity ledger;
-- LockedParentIds is the union of its actual returned locks, supplied separately.
-- This query acquires only supplied, still-owned target rows in ID order. It
-- neither discovers new parent locks nor locks cross_checks rows.
SELECT r.id FROM runs r
WHERE r.id = ANY(@run_ids::uuid[]) AND r.worker_id = @worker_id
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
ORDER BY r.id
FOR UPDATE OF r;

-- name: UpsertFrozenWorkerActiveRun :execrows
-- Internal transaction seam: the caller has already held the frozen parents and
-- prelocked snapshot targets. Fence membership and current identity before the
-- INSERT can acquire FK locks. An unchanged orphan can be recorded here; this
-- snapshot write does not admit or re-execute it.
-- Reported claim_generation remains independent of the frozen server generation,
-- preserving UpsertWorkerActiveRun storage, lease, pending-since and epoch behavior.
INSERT INTO worker_active_runs (
    worker_id, run_id, claim_generation, phase,
    terminal_pending, terminal_pending_until, terminal_pending_since, snapshot_epoch, reported_at
)
SELECT @worker_id, @run_id, @claim_generation, @phase,
       @terminal_pending,
       CASE WHEN @terminal_pending::boolean
            THEN now() + make_interval(secs => @lease_seconds::int)
            ELSE NULL END,
       CASE WHEN @terminal_pending::boolean
            THEN COALESCE(sqlc.narg(pending_since)::timestamptz, now())
            ELSE NULL END,
       @snapshot_epoch, now()
WHERE EXISTS (
    SELECT 1 FROM runs r
    WHERE r.id = @run_id AND r.worker_id = @worker_id AND r.claim_released_at IS NULL
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
ON CONFLICT (worker_id, run_id) DO UPDATE SET
    claim_generation       = EXCLUDED.claim_generation,
    phase                  = EXCLUDED.phase,
    terminal_pending       = EXCLUDED.terminal_pending,
    terminal_pending_until = EXCLUDED.terminal_pending_until,
    terminal_pending_since = EXCLUDED.terminal_pending_since,
    snapshot_epoch         = EXCLUDED.snapshot_epoch,
    reported_at            = EXCLUDED.reported_at;
