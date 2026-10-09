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

-- Frozen recovery phases ---------------------------------------------------
-- Internal transaction methods: FrozenTargets and LockedParentIds are mandatory
-- outputs of LockWorkerRecoveryParents. Parent reads stay inside the held set.
-- Exact association drift is deferred, including replacement by an already held
-- parent. Only terminal failure admits an unchanged orphan; checker admission
-- still requires the latest eligible pending plan round and active lead.

-- name: FrozenReadoptRunsFromSnapshot :many
-- PRD #1390 M2b (D5, D2): restore a `queued` run-lane run the worker still lists as a LIVE entry
-- (terminal_pending = false) at the SAME generation to its listed phase. This is a DIRECT status
-- write (not SetRunRunning), correct because the held-state content columns (open_question_id,
-- plan candidates, completion/follow-up identity) survived the stale requeue untouched (fact 4),
-- so the gate is restored by status alone. The queued interval is banked into budget_paused_seconds
-- only for the two approval/input phases (as the stale requeue did for the park). The requeue
-- refund (requeue_count - 1, floored at requeue_episode_baseline) fires ONLY when stale_requeue_generation = claim_generation
-- (D2: the stale requeue charged THIS exact generation); a NULL/mismatched provenance never refunds.
-- stale_requeue_generation is cleared after. claim_released_at IS NULL is #1247's fence (a run the
-- credential switch released must not be revived). Held-state content columns are UNTOUCHED here.
WITH candidates AS MATERIALIZED (
    SELECT r.id, r.kind FROM runs r, worker_active_runs a
    WHERE a.worker_id = @worker_id AND a.run_id = r.id AND a.terminal_pending = false
  AND r.worker_id = @worker_id
  AND r.status = 'queued'
  AND (r.kind <> 'cross_check' OR r.claim_generation > 0)
  AND r.kind <> 'chat'
  AND r.claim_generation = a.claim_generation
  AND r.claim_released_at IS NULL

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
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_checks AS MATERIALIZED (
    SELECT cc.* FROM cross_checks cc
    JOIN locked_parents lead ON lead.id = cc.lead_run_id
    CROSS JOIN parent_lock_set locks
    WHERE cc.checker_run_id IN (SELECT id FROM candidates WHERE kind = 'cross_check')
      AND cc.lead_run_id = ANY(locks.ids)
      AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
      AND cc.deadline_at > now()
      AND lead.status IN ('claimed', 'running')
      AND lead.claim_released_at IS NULL
      AND lead.claim_generation = cc.lead_claim_generation
      AND EXISTS (
          SELECT 1 FROM runs r WHERE r.id = cc.checker_run_id
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
    ORDER BY cc.lead_run_id
    FOR UPDATE OF cc
), checker_lock_set AS MATERIALIZED (
    SELECT array_agg(checker_run_id) AS ids FROM locked_checks
), eligible_candidates AS MATERIALIZED (
    SELECT DISTINCT mapping.run_id
    FROM parent_mapping mapping CROSS JOIN parent_lock_set locks
    WHERE (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
)
UPDATE runs r SET
    status = a.phase,
    status_since = now(),
    health = 'ok', health_reason = NULL, health_since = NULL,
    budget_paused_seconds = r.budget_paused_seconds
        + CASE WHEN a.phase IN ('awaiting_approval','awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - r.status_since))::int)
               ELSE 0 END,
    requeue_count = CASE WHEN r.stale_requeue_generation = r.claim_generation
                         THEN GREATEST(r.requeue_count - 1, r.requeue_episode_baseline) ELSE r.requeue_count END,
    stale_requeue_generation = NULL,
    updated_at = now()
FROM worker_active_runs a
WHERE a.worker_id = @worker_id AND a.run_id = r.id AND a.terminal_pending = false
  AND r.worker_id = @worker_id
  AND r.status = 'queued'
  AND (r.kind <> 'cross_check' OR r.claim_generation > 0)
  AND r.kind <> 'chat'
  AND r.claim_generation = a.claim_generation
  AND r.claim_released_at IS NULL
  AND r.id IN (SELECT run_id FROM eligible_candidates)
  AND (r.kind <> 'cross_check' OR EXISTS (
      SELECT 1 FROM cross_checks cc
      JOIN locked_parents lead ON lead.id = cc.lead_run_id
      CROSS JOIN checker_lock_set checks
      WHERE cc.checker_run_id = r.id AND r.id = ANY(checks.ids)
        AND cc.lead_run_id = r.target_run_id
        AND lead.user_id = r.user_id
        AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
        AND cc.deadline_at > now()
        AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
        AND lead.claim_generation = cc.lead_claim_generation
  ))
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
RETURNING r.id, r.user_id, r.status;

-- name: frozenFailRunsMissingFromSnapshotLocked :many
-- PRD #1390 M2b (SC2, over cap): a run-lane `running` run this worker OWNS but no longer lists (its
-- execution is lost) — past the fence, and out of episode re-queue budget — gets the #2394
-- evidence-based disposition: an owner hold (recovery_wait, worker_requeue_exhausted) when recovery
-- evidence or unresolved custody is recorded, else FAILED worker_lost (fail-first with the requeue
-- twin below). The failure path SET list mirrors FailRunsOfStaleWorkersOverCap (fail_origin='worker_lost',
-- the pause/switch/milestone clears, health reset, move_pending_since for the reconcile origin
-- restore). Held states are never targeted (status = 'running' only). Chat is a target restriction
-- (kind <> 'chat', D10) — these writers only ever touch run-lane runs. @missing_cutoff is the stale
-- window plus one heartbeat interval (D4); @max_requeues is RUN_MAX_REQUEUES.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending' AND NOT cc.wait_credited), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_targets AS MATERIALIZED (
    SELECT runs.* FROM runs
    JOIN parent_mapping mapping ON mapping.run_id = runs.id
    CROSS JOIN parent_lock_set locks
    WHERE runs.id IN (SELECT id FROM candidates)
      AND (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      AND runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending' AND NOT cc.wait_credited), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
    ORDER BY runs.id
    FOR UPDATE OF runs
), target_lock_set AS MATERIALIZED (
    SELECT array_agg(id) AS ids FROM locked_targets
), final_targets AS MATERIALIZED (
    -- Recheck the refreshed locked rows, not initial candidate membership.
    SELECT runs.* FROM locked_targets runs CROSS JOIN target_lock_set locks
    WHERE runs.id = ANY(locks.ids)
      AND runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending' AND NOT cc.wait_credited), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
), eligible_parent_exits AS MATERIALIZED (
    -- Shared by parent writes and suppression; 00312 owns settlement and cancellation
    -- for an unreleased active lead with the latest eligible pending plan round.
    SELECT id, status, claim_released_at FROM final_targets WHERE kind <> 'cross_check'
), eligible_candidates AS MATERIALIZED (
    SELECT target.id AS run_id FROM final_targets target
    WHERE (target.kind = 'cross_check'
           OR target.id IN (SELECT id FROM eligible_parent_exits))
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN eligible_parent_exits exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = target.id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND exiting.status IN ('claimed', 'running')
            AND exiting.claim_released_at IS NULL
      )
)
UPDATE runs SET plan_cross_check_gate_reason = NULL,
    status = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN 'recovery_wait' ELSE 'failed' END, status_since = sqlc.arg('now')::timestamptz, failure_reason = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN NULL ELSE CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = runs.id AND h.user_id = runs.user_id AND h.original_worker_id = runs.worker_id AND h.generation = runs.claim_generation AND h.terminal_record_rejection = 'mac_failure') THEN 'terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody' ELSE @failure_reason END END,
    fail_origin = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN NULL ELSE 'worker_lost' END,
    move_pending_since = CASE WHEN NOT COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) AND runs.issue_iid IS NOT NULL THEN now() END, finished_at = CASE WHEN NOT COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN now() END,
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    recovery_wait_cause = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN 'worker_requeue_exhausted' END,
    -- Bank the old waiting interval before replacing status_since, only for a park.
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true)
                    AND runs.status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    worker_recovery_evidence = @exhaustion_evidence::jsonb->runs.id::text->'evidence',
    recovery_retry_not_before = NULL, stale_requeue_generation = NULL,
    claim_released_at = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN now() ELSE runs.claim_released_at END,
    released_worker_id = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN runs.worker_id ELSE runs.released_worker_id END,
    released_worker_nonce = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN (SELECT snapshot_register_nonce FROM workers WHERE id = runs.worker_id) ELSE runs.released_worker_nonce END,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND runs.kind <> 'chat'                                   -- D10 (run-lane only; chat has its own sweeps)
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL                        -- #1247 fence
  AND runs.status_since < @missing_cutoff                   -- fence: stale window + one heartbeat interval, D4
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending' AND NOT cc.wait_credited), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- ABSENT (or a different generation) from the snapshot
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- D11 terminal-pending lease (worker-scoped, defense-in-depth)
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w                   -- D11 pending_overflow closure (worker-level, ESSENTIAL)
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())
  AND runs.id IN (SELECT run_id FROM eligible_candidates)
  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
RETURNING runs.id, runs.user_id, runs.status;

-- name: FrozenRequeueRunsMissingFromSnapshot :many
-- PRD #1390 M2b (SC2, under cap): the requeue twin of FailRunsMissingFromSnapshot — a `running`
-- run-lane run this worker OWNS but no longer lists, past the fence and within budget, is REQUEUED
-- through the existing requeue path. Its SET list mirrors RequeueRunsOfStaleWorkers (health reset,
-- park-time bank for approval/input — a no-op here since only status='running' is targeted, codex
-- cap revocation, requeue_count++). It does NOT set stale_requeue_generation: a genuine loss is
-- never refunded (D2). Chat is a target restriction (kind <> 'chat', D10).
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) < @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending' AND NOT cc.wait_credited), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_checks AS MATERIALIZED (
    SELECT cc.* FROM cross_checks cc
    JOIN locked_parents lead ON lead.id = cc.lead_run_id
    CROSS JOIN parent_lock_set locks
    WHERE cc.checker_run_id IN (SELECT id FROM candidates WHERE kind = 'cross_check')
      AND cc.lead_run_id = ANY(locks.ids)
      AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
      AND cc.deadline_at > now()
      AND lead.status IN ('claimed', 'running')
      AND lead.claim_released_at IS NULL
      AND lead.claim_generation = cc.lead_claim_generation
      AND EXISTS (
          SELECT 1 FROM runs r WHERE r.id = cc.checker_run_id
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
    ORDER BY cc.lead_run_id
    FOR UPDATE OF cc
), checker_lock_set AS MATERIALIZED (
    SELECT array_agg(checker_run_id) AS ids FROM locked_checks
), eligible_candidates AS MATERIALIZED (
    SELECT DISTINCT mapping.run_id
    FROM parent_mapping mapping CROSS JOIN parent_lock_set locks
    WHERE (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      -- The lead-exit trigger owns cancellation of a same-batch checker.
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN candidates exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = mapping.run_id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND EXISTS (
                SELECT 1 FROM locked_parents runs
                WHERE runs.id = exiting.id
                  AND runs.status IN ('claimed', 'running')
                  AND runs.claim_released_at IS NULL
                  AND (runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) < @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending' AND NOT cc.wait_credited), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now()))
            )
      )
)
UPDATE runs SET status = 'queued', status_since = sqlc.arg('now')::timestamptz, requeue_count = runs.requeue_count + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN runs.status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND runs.kind <> 'chat'                                   -- D10 (run-lane only; chat has its own sweeps)
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL                        -- #1247 fence
  AND runs.status_since < @missing_cutoff                   -- fence: stale window + one heartbeat interval, D4
  AND (runs.requeue_count - runs.requeue_episode_baseline) < @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending' AND NOT cc.wait_credited), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- ABSENT (or a different generation) from the snapshot
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- D11 terminal-pending lease (worker-scoped, defense-in-depth)
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w                   -- D11 pending_overflow closure (worker-level, ESSENTIAL)
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())
  AND runs.id IN (SELECT run_id FROM eligible_candidates)
  AND (runs.kind <> 'cross_check' OR EXISTS (
      SELECT 1 FROM cross_checks cc
      JOIN locked_parents lead ON lead.id = cc.lead_run_id
      CROSS JOIN checker_lock_set checks
      WHERE cc.checker_run_id = runs.id AND runs.id = ANY(checks.ids)
        AND cc.lead_run_id = runs.target_run_id AND lead.user_id = runs.user_id
        AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
        AND cc.deadline_at > now()
        AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
        AND lead.claim_generation = cc.lead_claim_generation
  ))
  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
RETURNING runs.id, runs.user_id, runs.status;

-- name: frozenFailWorkerRunsOverCapLocked :many
-- On register a worker declares a fresh start, so any run it still holds is
-- orphaned (its execution is gone). Over its episode re-queue budget the #2394 disposition holds
-- it for the owner when recovery evidence or unresolved custody is recorded; otherwise this
-- writer fails it. failed →
-- origin restore, applied by the reconcile loop (register does no forge I/O), so
-- it stamps move_pending_since. RETURNING id so the caller can funnel these
-- committed-terminal (worker-lost) runs into the judge (PRD #46 Decision 2), exactly
-- as the sweeper's FailRunsOfStaleWorkersOverCap does.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
      AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Collect the actual locked IDs completely before any checker can be mutated.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_targets AS MATERIALIZED (
    SELECT runs.* FROM runs
    JOIN parent_mapping mapping ON mapping.run_id = runs.id
    CROSS JOIN parent_lock_set locks
    WHERE runs.id IN (SELECT id FROM candidates)
      AND (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      AND runs.worker_id = @worker_id
      AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
    ORDER BY runs.id
    FOR UPDATE OF runs
), target_lock_set AS MATERIALIZED (
    SELECT array_agg(id) AS ids FROM locked_targets
), final_targets AS MATERIALIZED (
    -- Recheck the refreshed locked rows, not initial candidate membership.
    SELECT runs.* FROM locked_targets runs CROSS JOIN target_lock_set locks
    WHERE runs.id = ANY(locks.ids)
      AND runs.worker_id = @worker_id
      AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
), eligible_parent_exits AS MATERIALIZED (
    -- Shared by parent writes and suppression; 00312 owns settlement and cancellation
    -- for an unreleased active lead with the latest eligible pending plan round.
    SELECT id, status, claim_released_at FROM final_targets WHERE kind <> 'cross_check'
), eligible_candidates AS MATERIALIZED (
    SELECT target.id AS run_id FROM final_targets target
    WHERE (target.kind = 'cross_check'
           OR target.id IN (SELECT id FROM eligible_parent_exits))
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN eligible_parent_exits exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = target.id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND exiting.status IN ('claimed', 'running')
            AND exiting.claim_released_at IS NULL
      )
)
UPDATE runs SET plan_cross_check_gate_reason = NULL,
    status = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN 'recovery_wait' ELSE 'failed' END, status_since = now(), failure_reason = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN NULL ELSE CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = runs.id AND h.user_id = runs.user_id AND h.original_worker_id = runs.worker_id AND h.generation = runs.claim_generation AND h.terminal_record_rejection = 'mac_failure') THEN 'terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody' ELSE @failure_reason END END,
    -- PRD #69 M7a: the trusted failure class for an orphaned run whose worker is gone.
    fail_origin = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN NULL ELSE 'worker_lost' END,
    move_pending_since = CASE WHEN NOT COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) AND runs.issue_iid IS NOT NULL THEN now() END, finished_at = CASE WHEN NOT COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN now() END,
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    recovery_wait_cause = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN 'worker_requeue_exhausted' END,
    -- Bank the old waiting interval before replacing status_since, only for a park.
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true)
                    AND runs.status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    worker_recovery_evidence = @exhaustion_evidence::jsonb->runs.id::text->'evidence',
    recovery_retry_not_before = NULL, stale_requeue_generation = NULL,
    claim_released_at = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN now() ELSE runs.claim_released_at END,
    released_worker_id = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN runs.worker_id ELSE runs.released_worker_id END,
    released_worker_nonce = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN
        CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'release_nonce_captured')::boolean, false)
             THEN @exhaustion_evidence::jsonb->runs.id::text->>'released_worker_nonce'
             ELSE (SELECT snapshot_register_nonce FROM workers WHERE id = runs.worker_id) END
        ELSE runs.released_worker_nonce END,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    updated_at = now()
FROM eligible_candidates candidate
WHERE runs.claim_released_at IS NULL AND runs.id = candidate.run_id
  AND runs.worker_id = @worker_id
  AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  -- PRD #1390 D11: register's orphan fail honours the terminal-pending lease + pending_overflow
  -- closure exactly as the stale-worker passes do (chat-exempt, D10) — a fresh worker process
  -- must not fail its own run whose outcome is journaled and about to be replayed (#1391).
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
RETURNING runs.id, runs.status;

-- name: FrozenRequeueWorkerRuns :many
-- Within budget → re-queued to this same worker (affinity), which then re-claims
-- and resumes from the persisted session (handles docker compose down && up).
--
-- PRD #1390 M2a: RETURNING id so Register can publish each requeue transition post-commit
-- (via publishSwept) exactly as the sweeper's RequeueRunsOfStaleWorkers twin already does —
-- closing the gap where a register-time requeue reached no live channel.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
      AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) < @max_requeues
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Collect the actual locked IDs completely before any checker can be mutated.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_checks AS MATERIALIZED (
    SELECT cc.* FROM cross_checks cc
    JOIN locked_parents lead ON lead.id = cc.lead_run_id
    CROSS JOIN parent_lock_set locks
    WHERE cc.checker_run_id IN (SELECT id FROM candidates WHERE kind = 'cross_check')
      AND cc.lead_run_id = ANY(locks.ids)
      AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
      AND cc.deadline_at > now()
      AND lead.status IN ('claimed', 'running')
      AND lead.claim_released_at IS NULL
      AND lead.claim_generation = cc.lead_claim_generation
      AND EXISTS (
          SELECT 1 FROM runs r WHERE r.id = cc.checker_run_id
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
    ORDER BY cc.lead_run_id
    FOR UPDATE OF cc
), checker_lock_set AS MATERIALIZED (
    SELECT array_agg(checker_run_id) AS ids FROM locked_checks
), eligible_candidates AS MATERIALIZED (
    SELECT DISTINCT mapping.run_id
    FROM parent_mapping mapping CROSS JOIN parent_lock_set locks
    WHERE (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      -- A same-batch lead exit owns its checker cancellation through the trigger.
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN candidates exiting ON exiting.id = cc.lead_run_id
          JOIN locked_parents lead ON lead.id = exiting.id
          WHERE cc.checker_run_id = mapping.run_id
            AND lead.worker_id = @worker_id
              AND lead.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
              AND (lead.requeue_count - lead.requeue_episode_baseline) < @max_requeues
              AND (lead.kind = 'chat'
                     OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                                     WHERE a.run_id = lead.id AND a.worker_id = lead.worker_id AND a.terminal_pending
                                       AND a.terminal_pending_until > now()
                                       AND a.claim_generation = lead.claim_generation)
                         AND NOT EXISTS (SELECT 1 FROM workers w
                                         WHERE w.id = lead.worker_id AND w.pending_overflow_until > now())))
      )
)
UPDATE runs SET status = 'queued', status_since = now(), requeue_count = runs.requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's status_since.
    health = 'ok', health_reason = NULL, health_since = NULL,
    -- Issue #783: bank park time before a worker-death requeue -> queued, since started_at
    -- survives the requeue and the later claimed->running resume would not see the park.
    -- awaiting_followup is intentionally excluded: interactive runs are exempt from
    -- SweepRunningTimeout entirely (interactive = false), so they have no wall deadline.
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN runs.status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    -- Codex claim-capability revocation (PRD #1147 M2): re-queuing a worker's runs
    -- revokes their per-claim capabilities immediately — clear the hash and bump the
    -- epoch. A harmless no-op for a non-codex run, whose codex_cap_hash is already NULL.
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    updated_at = now()
FROM eligible_candidates candidate
WHERE runs.id = candidate.run_id
  AND runs.worker_id = @worker_id
  AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND (runs.requeue_count - runs.requeue_episode_baseline) < @max_requeues
  -- PRD #1390 D11: register's orphan requeue honours the terminal-pending lease + pending_overflow
  -- closure exactly as the stale-worker passes do (chat-exempt, D10) — a fresh worker process
  -- must not requeue its own run whose outcome is journaled and about to be replayed (#1391).
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
  AND (runs.kind <> 'cross_check' OR EXISTS (
      SELECT 1 FROM cross_checks cc
      JOIN locked_parents lead ON lead.id = cc.lead_run_id
      CROSS JOIN checker_lock_set checks
      WHERE cc.checker_run_id = runs.id AND runs.id = ANY(checks.ids)
        AND cc.lead_run_id = runs.target_run_id AND lead.user_id = runs.user_id
        AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
        AND cc.deadline_at > now()
        AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
        AND lead.claim_generation = cc.lead_claim_generation
  ))
  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
RETURNING runs.id;

-- name: frozenFailAttestedFinalizeRunsOverCapLocked :many
-- An attested run that is over budget and not eligible for the one-shot allowance (allowance
-- already used, or RUN_MAX_REQUEUES = 0) gets the same evidence-based disposition as any other
-- exhausted run: an owner hold when recovery evidence or unresolved custody is recorded, else failed
-- as FailWorkerRunsOverCap fails it.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (@max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL)

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_targets AS MATERIALIZED (
    SELECT runs.* FROM runs
    JOIN parent_mapping mapping ON mapping.run_id = runs.id
    CROSS JOIN parent_lock_set locks
    WHERE runs.id IN (SELECT id FROM candidates)
      AND (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      AND runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (@max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL)

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
    ORDER BY runs.id
    FOR UPDATE OF runs
), target_lock_set AS MATERIALIZED (
    SELECT array_agg(id) AS ids FROM locked_targets
), final_targets AS MATERIALIZED (
    -- Recheck the refreshed locked rows, not initial candidate membership.
    SELECT runs.* FROM locked_targets runs CROSS JOIN target_lock_set locks
    WHERE runs.id = ANY(locks.ids)
      AND runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (@max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL)

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
), eligible_parent_exits AS MATERIALIZED (
    -- Shared by parent writes and suppression; 00312 owns settlement and cancellation
    -- for an unreleased active lead with the latest eligible pending plan round.
    SELECT id, status, claim_released_at FROM final_targets WHERE kind <> 'cross_check'
), eligible_candidates AS MATERIALIZED (
    SELECT target.id AS run_id FROM final_targets target
    WHERE (target.kind = 'cross_check'
           OR target.id IN (SELECT id FROM eligible_parent_exits))
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN eligible_parent_exits exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = target.id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND exiting.status IN ('claimed', 'running')
            AND exiting.claim_released_at IS NULL
      )
)
UPDATE runs SET plan_cross_check_gate_reason = NULL,
    status = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN 'recovery_wait' ELSE 'failed' END, status_since = now(), failure_reason = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN NULL ELSE CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = runs.id AND h.user_id = runs.user_id AND h.original_worker_id = runs.worker_id AND h.generation = runs.claim_generation AND h.terminal_record_rejection = 'mac_failure') THEN 'terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody' ELSE @failure_reason END END,
    fail_origin = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN NULL ELSE 'worker_lost' END,
    move_pending_since = CASE WHEN NOT COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) AND runs.issue_iid IS NOT NULL THEN now() END, finished_at = CASE WHEN NOT COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN now() END,
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    recovery_wait_cause = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN 'worker_requeue_exhausted' END,
    -- Bank the old waiting interval before replacing status_since, only for a park.
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true)
                    AND runs.status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    worker_recovery_evidence = @exhaustion_evidence::jsonb->runs.id::text->'evidence',
    recovery_retry_not_before = NULL, stale_requeue_generation = NULL,
    claim_released_at = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN now() ELSE runs.claim_released_at END,
    released_worker_id = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN runs.worker_id ELSE runs.released_worker_id END,
    released_worker_nonce = CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'park')::boolean, true) THEN
        CASE WHEN COALESCE((@exhaustion_evidence::jsonb->runs.id::text->>'release_nonce_captured')::boolean, false)
             THEN @exhaustion_evidence::jsonb->runs.id::text->>'released_worker_nonce'
             ELSE (SELECT snapshot_register_nonce FROM workers WHERE id = runs.worker_id) END
        ELSE runs.released_worker_nonce END,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  -- Positional pairing of the two parallel arrays (run ids are unique: Register validates the
  -- list). array_position is NULL for an unlisted run, so the equality is then never true.
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (@max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL)
  AND runs.id IN (SELECT run_id FROM eligible_candidates)
  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
RETURNING runs.id, runs.status;

-- name: FrozenRequeueAttestedFinalizeRuns :many
-- Under budget: an ordinary requeue (the allowance mark is NOT set). Over budget: the one-shot
-- allowance (@max_requeues > 0 and finalize_resume_generation still NULL) requeues once more and
-- stamps finalize_resume_generation = claim_generation. Never decrements requeue_count.
-- RETURNING allowance_used tells the caller which case fired (the mark equals the generation the
-- run held when re-queued; claim_generation is unchanged until the next ClaimRun).
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND ((runs.requeue_count - runs.requeue_episode_baseline) < @max_requeues
       OR ((runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues AND @max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_checks AS MATERIALIZED (
    SELECT cc.* FROM cross_checks cc
    JOIN locked_parents lead ON lead.id = cc.lead_run_id
    CROSS JOIN parent_lock_set locks
    WHERE cc.checker_run_id IN (SELECT id FROM candidates WHERE kind = 'cross_check')
      AND cc.lead_run_id = ANY(locks.ids)
      AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
      AND cc.deadline_at > now()
      AND lead.status IN ('claimed', 'running')
      AND lead.claim_released_at IS NULL
      AND lead.claim_generation = cc.lead_claim_generation
      AND EXISTS (
          SELECT 1 FROM runs r WHERE r.id = cc.checker_run_id
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
    ORDER BY cc.lead_run_id
    FOR UPDATE OF cc
), checker_lock_set AS MATERIALIZED (
    SELECT array_agg(checker_run_id) AS ids FROM locked_checks
), eligible_candidates AS MATERIALIZED (
    SELECT DISTINCT mapping.run_id
    FROM parent_mapping mapping CROSS JOIN parent_lock_set locks
    WHERE (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      -- The lead-exit trigger owns cancellation of a same-batch checker.
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN candidates exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = mapping.run_id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND EXISTS (
                SELECT 1 FROM locked_parents runs
                WHERE runs.id = exiting.id
                  AND runs.status IN ('claimed', 'running')
                  AND runs.claim_released_at IS NULL
                  AND (runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND ((requeue_count - requeue_episode_baseline) < @max_requeues
       OR ((requeue_count - requeue_episode_baseline) >= @max_requeues AND @max_requeues > 0 AND worker_recovery_episode = 0 AND finalize_resume_generation IS NULL)))
            )
      )
)
UPDATE runs SET status = 'queued', status_since = now(), requeue_count = runs.requeue_count + 1,
    finalize_resume_generation = CASE WHEN (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues AND @max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL
                                      THEN runs.claim_generation
                                      ELSE runs.finalize_resume_generation END,
    health = 'ok', health_reason = NULL, health_since = NULL,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  -- Positional pairing of the two parallel arrays (run ids are unique: Register validates the
  -- list). array_position is NULL for an unlisted run, so the equality is then never true.
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND ((runs.requeue_count - runs.requeue_episode_baseline) < @max_requeues
       OR ((runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues AND @max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL))
  AND runs.id IN (SELECT run_id FROM eligible_candidates)
  AND (runs.kind <> 'cross_check' OR EXISTS (
      SELECT 1 FROM cross_checks cc
      JOIN locked_parents lead ON lead.id = cc.lead_run_id
      CROSS JOIN checker_lock_set checks
      WHERE cc.checker_run_id = runs.id AND runs.id = ANY(checks.ids)
        AND cc.lead_run_id = runs.target_run_id AND lead.user_id = runs.user_id
        AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
        AND cc.deadline_at > now()
        AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
        AND lead.claim_generation = cc.lead_claim_generation
  ))
  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
RETURNING runs.id, (runs.worker_recovery_episode = 0 AND @max_requeues > 0 AND (runs.requeue_count - runs.requeue_episode_baseline) > @max_requeues AND runs.finalize_resume_generation IS NOT NULL AND runs.finalize_resume_generation = runs.claim_generation)::boolean AS allowance_used;
-- name: LockFrozenFailRunsMissingFromSnapshot :many
-- PRD #1390 M2b (SC2, over cap): a run-lane `running` run this worker OWNS but no longer lists (its
-- execution is lost) — past the fence, and out of episode re-queue budget — gets the #2394
-- evidence-based disposition: an owner hold (recovery_wait, worker_requeue_exhausted) when recovery
-- evidence or unresolved custody is recorded, else FAILED worker_lost (fail-first with the requeue
-- twin below). The failure path SET list mirrors FailRunsOfStaleWorkersOverCap (fail_origin='worker_lost',
-- the pause/switch/milestone clears, health reset, move_pending_since for the reconcile origin
-- restore). Held states are never targeted (status = 'running' only). Chat is a target restriction
-- (kind <> 'chat', D10) — these writers only ever touch run-lane runs. @missing_cutoff is the stale
-- window plus one heartbeat interval (D4); @max_requeues is RUN_MAX_REQUEUES.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.verdict = 'pending'), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_targets AS MATERIALIZED (
    SELECT runs.* FROM runs
    JOIN parent_mapping mapping ON mapping.run_id = runs.id
    CROSS JOIN parent_lock_set locks
    WHERE runs.id IN (SELECT id FROM candidates)
      AND (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      AND runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.verdict = 'pending'), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
    ORDER BY runs.id
    FOR UPDATE OF runs
), target_lock_set AS MATERIALIZED (
    SELECT array_agg(id) AS ids FROM locked_targets
), final_targets AS MATERIALIZED (
    -- Recheck the refreshed locked rows, not initial candidate membership.
    SELECT runs.* FROM locked_targets runs CROSS JOIN target_lock_set locks
    WHERE runs.id = ANY(locks.ids)
      AND runs.worker_id = @worker_id
  AND runs.kind <> 'chat'
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL
  AND runs.status_since < @missing_cutoff
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (runs.kind NOT IN ('chat', 'judge', 'job', 'cross_check') AND runs.interactive = false
    AND runs.completion_attempts = 0
    AND runs.started_at < (sqlc.arg('now')::timestamptz
      - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                            + runs.budget_paused_seconds + runs.budget_extension_seconds
                            + runs.budget_finalize_seconds
                            + COALESCE((SELECT GREATEST(0, CEIL(EXTRACT(EPOCH FROM
                                (LEAST(sqlc.arg('now')::timestamptz, cc.deadline_at) - cc.created_at)))::int)
                                FROM cross_checks cc WHERE cc.lead_run_id = runs.id
                                  AND cc.stage = 'plan' AND cc.verdict = 'pending'), 0))))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
), eligible_parent_exits AS MATERIALIZED (
    -- Shared by parent writes and suppression; 00312 owns settlement and cancellation
    -- for an unreleased active lead with the latest eligible pending plan round.
    SELECT id, status, claim_released_at FROM final_targets WHERE kind <> 'cross_check'
), eligible_candidates AS MATERIALIZED (
    SELECT target.id AS run_id FROM final_targets target
    WHERE (target.kind = 'cross_check'
           OR target.id IN (SELECT id FROM eligible_parent_exits))
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN eligible_parent_exits exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = target.id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND exiting.status IN ('claimed', 'running')
            AND exiting.claim_released_at IS NULL
      )
)
SELECT id FROM final_targets ORDER BY id;

-- name: LockFrozenFailWorkerRunsOverCap :many
-- On register a worker declares a fresh start, so any run it still holds is
-- orphaned (its execution is gone). Over its episode re-queue budget the #2394 disposition holds
-- it for the owner when recovery evidence or unresolved custody is recorded; otherwise this
-- writer fails it. failed →
-- origin restore, applied by the reconcile loop (register does no forge I/O), so
-- it stamps move_pending_since. RETURNING id so the caller can funnel these
-- committed-terminal (worker-lost) runs into the judge (PRD #46 Decision 2), exactly
-- as the sweeper's FailRunsOfStaleWorkersOverCap does.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
      AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Collect the actual locked IDs completely before any checker can be mutated.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_targets AS MATERIALIZED (
    SELECT runs.* FROM runs
    JOIN parent_mapping mapping ON mapping.run_id = runs.id
    CROSS JOIN parent_lock_set locks
    WHERE runs.id IN (SELECT id FROM candidates)
      AND (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      AND runs.worker_id = @worker_id
      AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
    ORDER BY runs.id
    FOR UPDATE OF runs
), target_lock_set AS MATERIALIZED (
    SELECT array_agg(id) AS ids FROM locked_targets
), final_targets AS MATERIALIZED (
    -- Recheck the refreshed locked rows, not initial candidate membership.
    SELECT runs.* FROM locked_targets runs CROSS JOIN target_lock_set locks
    WHERE runs.id = ANY(locks.ids)
      AND runs.worker_id = @worker_id
      AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
), eligible_parent_exits AS MATERIALIZED (
    -- Shared by parent writes and suppression; 00312 owns settlement and cancellation
    -- for an unreleased active lead with the latest eligible pending plan round.
    SELECT id, status, claim_released_at FROM final_targets WHERE kind <> 'cross_check'
), eligible_candidates AS MATERIALIZED (
    SELECT target.id AS run_id FROM final_targets target
    WHERE (target.kind = 'cross_check'
           OR target.id IN (SELECT id FROM eligible_parent_exits))
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN eligible_parent_exits exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = target.id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND exiting.status IN ('claimed', 'running')
            AND exiting.claim_released_at IS NULL
      )
)
SELECT id FROM final_targets ORDER BY id;

-- name: LockFrozenFailAttestedFinalizeRunsOverCap :many
-- An attested run that is over budget and not eligible for the one-shot allowance (allowance
-- already used, or RUN_MAX_REQUEUES = 0) gets the same evidence-based disposition as any other
-- exhausted run: an owner hold when recovery evidence or unresolved custody is recorded, else failed
-- as FailWorkerRunsOverCap fails it.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (@max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL)

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id = ANY(@locked_parent_ids::uuid[])
      AND lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_targets AS MATERIALIZED (
    SELECT runs.* FROM runs
    JOIN parent_mapping mapping ON mapping.run_id = runs.id
    CROSS JOIN parent_lock_set locks
    WHERE runs.id IN (SELECT id FROM candidates)
      AND (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
      AND runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (@max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL)

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
    ORDER BY runs.id
    FOR UPDATE OF runs
), target_lock_set AS MATERIALIZED (
    SELECT array_agg(id) AS ids FROM locked_targets
), final_targets AS MATERIALIZED (
    -- Recheck the refreshed locked rows, not initial candidate membership.
    SELECT runs.* FROM locked_targets runs CROSS JOIN target_lock_set locks
    WHERE runs.id = ANY(locks.ids)
      AND runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.status = 'running'
  AND runs.kind <> 'chat'
  AND runs.id = ANY(@run_ids::uuid[])
  AND runs.claim_generation = (@claim_generations::bigint[])[array_position(@run_ids::uuid[], runs.id)]
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                    AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND (runs.requeue_count - runs.requeue_episode_baseline) >= @max_requeues
  AND NOT (@max_requeues > 0 AND runs.worker_recovery_episode = 0 AND runs.finalize_resume_generation IS NULL)

  AND EXISTS (
    SELECT 1
    FROM jsonb_to_recordset(
        COALESCE(NULLIF(@frozen_targets::jsonb, 'null'::jsonb), '[]'::jsonb)
    ) AS frozen(
        id uuid, kind text, target_run_id uuid, worker_id uuid,
        claim_generation bigint, cross_check_id uuid, parent_lead_id uuid
    )
    LEFT JOIN cross_checks cc ON runs.kind = 'cross_check' AND cc.checker_run_id = runs.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN runs.kind = 'cross_check'
        THEN cc.lead_run_id ELSE runs.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = runs.user_id
    WHERE frozen.id = runs.id
      AND runs.kind IS NOT DISTINCT FROM frozen.kind
      AND runs.target_run_id IS NOT DISTINCT FROM frozen.target_run_id
      AND runs.worker_id IS NOT DISTINCT FROM frozen.worker_id
      AND runs.claim_generation IS NOT DISTINCT FROM frozen.claim_generation
      AND cc.id IS NOT DISTINCT FROM frozen.cross_check_id
      AND parent.id IS NOT DISTINCT FROM frozen.parent_lead_id
      AND (
          (parent.id = ANY(@locked_parent_ids::uuid[])
           AND (runs.kind <> 'cross_check' OR
                (cc.id = frozen.cross_check_id AND cc.lead_run_id = frozen.parent_lead_id)))
          OR (runs.kind = 'cross_check' AND cc.id IS NULL
              AND frozen.cross_check_id IS NULL AND frozen.parent_lead_id IS NULL)
      )
  )
), eligible_parent_exits AS MATERIALIZED (
    -- Shared by parent writes and suppression; 00312 owns settlement and cancellation
    -- for an unreleased active lead with the latest eligible pending plan round.
    SELECT id, status, claim_released_at FROM final_targets WHERE kind <> 'cross_check'
), eligible_candidates AS MATERIALIZED (
    SELECT target.id AS run_id FROM final_targets target
    WHERE (target.kind = 'cross_check'
           OR target.id IN (SELECT id FROM eligible_parent_exits))
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN eligible_parent_exits exiting ON exiting.id = cc.lead_run_id
          WHERE cc.checker_run_id = target.id
            AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
            AND exiting.status IN ('claimed', 'running')
            AND exiting.claim_released_at IS NULL
      )
)
SELECT id FROM final_targets ORDER BY id;
