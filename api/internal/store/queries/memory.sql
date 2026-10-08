-- name: GetMemoryRunForUpdate :one
-- Serialize memory admission, outcome and hold after the caller locks the worker.
-- Tenant-scoped rather than worker-scoped so an old worker can read its immutable
-- intervention history after a new claim. Lock applicable parents before checkers.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.id = @id AND runs.user_id = @user_id
), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
    ORDER BY lead.id
    FOR UPDATE OF lead
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), eligible_candidates AS MATERIALIZED (
    SELECT DISTINCT mapping.run_id
    FROM parent_mapping mapping CROSS JOIN parent_lock_set locks
    WHERE (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
)
SELECT runs.* FROM runs WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.id IN (SELECT run_id FROM eligible_candidates)
FOR UPDATE OF runs;

-- name: GetMemoryIntervention :one
SELECT * FROM memory_interventions WHERE intervention_id = @intervention_id;

-- name: InsertMemoryIntervention :execrows
INSERT INTO memory_interventions
(intervention_id, run_id, user_id, worker_id, register_nonce, claim_generation,
 memory_episode, admitted, allowance_used, policy)
VALUES (@intervention_id, @run_id, @user_id, @worker_id, @register_nonce,
 @claim_generation, @memory_episode, @admitted, @allowance_used, @policy)
ON CONFLICT (intervention_id) DO NOTHING;

-- name: ChargeMemoryIntervention :one
UPDATE runs SET memory_policy=COALESCE(memory_policy, @policy::jsonb),
    memory_intervention_count=memory_intervention_count + 1, updated_at=now()
WHERE id= @id
RETURNING *;

-- name: FreezeMemoryPolicy :one
UPDATE runs SET memory_policy=COALESCE(memory_policy, @policy::jsonb)
WHERE id= @id RETURNING *;

-- name: RecordMemoryOutcome :one
UPDATE memory_interventions SET outcome=COALESCE(outcome, @outcome::text),
    outcome_at=COALESCE(outcome_at, now())
WHERE intervention_id= @intervention_id AND admitted
RETURNING *;

-- name: MemoryParentEligible :one
SELECT (EXISTS (
 SELECT 1 FROM cross_checks cc JOIN runs lead ON lead.id=cc.lead_run_id
 JOIN runs checker ON checker.id=cc.checker_run_id
 WHERE cc.checker_run_id= @id AND cc.lead_claim_generation=lead.claim_generation
 AND lead.id=checker.target_run_id AND lead.user_id=checker.user_id
 AND lead.kind <> 'cross_check'
 AND lead.claim_released_at IS NULL AND lead.status IN ('claimed','running')
 AND cc.verdict='pending' AND cc.deadline_at>now()
))::boolean;

-- name: ParkWorkerMemoryPressure :one
-- The caller holds the worker, parent and run locks. Custody and drain evidence survive.
UPDATE runs SET status='recovery_wait', status_since=now(),
    recovery_wait_cause='worker_memory_pressure', recovery_retry_not_before=NULL,
    claim_released_at=now(), released_worker_id=worker_id,
    released_worker_nonce= @register_nonce,
    codex_cap_hash=NULL, codex_claim_epoch=codex_claim_epoch+1,
    health='ok', health_reason=NULL, health_since=NULL, updated_at=now()
WHERE id= @id AND worker_id= @worker_id AND claim_generation= @claim_generation
    AND memory_episode= @memory_episode AND claim_released_at IS NULL
    AND status IN ('claimed','running')
RETURNING *;

-- name: ResumeMemoryEpisode :one
-- Explicit owner release of an exhaustion hold is the only episode-renewal writer.
-- Keep the wall budget, custody and released incarnation; bank the hold only after execution started.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.status = 'recovery_wait' AND runs.recovery_wait_cause = 'worker_memory_pressure'
  AND (runs.kind IN ('chat', 'judge') OR runs.interactive OR runs.started_at IS NULL
       OR (COALESCE(runs.budget_wall_seconds, @global_timeout_seconds::int) + runs.budget_extension_seconds + runs.budget_finalize_seconds)
          - (GREATEST(0, EXTRACT(EPOCH FROM (runs.status_since - runs.started_at))::int) - runs.budget_paused_seconds) > 0)
), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.* FROM runs lead
    WHERE lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
    ORDER BY lead.id
    FOR UPDATE OF lead
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_checks AS MATERIALIZED (
    SELECT cc.* FROM cross_checks cc
    JOIN locked_parents lead ON lead.id = cc.lead_run_id
    CROSS JOIN parent_lock_set locks
    WHERE cc.checker_run_id IN (SELECT id FROM candidates WHERE kind = 'cross_check')
      AND cc.lead_run_id = ANY(locks.ids)
      AND cc.stage = 'plan' AND cc.round = 1 AND cc.verdict = 'pending'
      AND cc.deadline_at > now()
      AND lead.status IN ('claimed', 'running')
      AND lead.claim_released_at IS NULL
      AND lead.claim_generation = cc.lead_claim_generation
    ORDER BY cc.lead_run_id
    FOR UPDATE OF cc
), checker_lock_set AS MATERIALIZED (
    SELECT array_agg(checker_run_id) AS ids FROM locked_checks
), eligible_candidates AS MATERIALIZED (
    SELECT DISTINCT mapping.run_id
    FROM parent_mapping mapping CROSS JOIN parent_lock_set locks
    WHERE (mapping.parent_id IS NULL OR mapping.parent_id = ANY(locks.ids))
)
UPDATE runs SET
    status                = 'queued',
    status_since          = now(),
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN runs.started_at IS NOT NULL
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    -- Drop obsolete affinity while preserving the incarnation captured by the park.
    worker_id = CASE WHEN runs.claim_released_at IS NOT NULL THEN NULL ELSE runs.worker_id END,
    hold_reason = NULL,
    hold_captured_head = NULL,
    memory_episode = runs.memory_episode + 1,
    memory_intervention_count = 0, memory_policy = NULL,
    recovery_wait_cause = NULL, recovery_retry_not_before = NULL,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at            = now()
WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.status = 'recovery_wait' AND runs.recovery_wait_cause = 'worker_memory_pressure'
  -- Honor the wall sweep's chat/judge/interactive exemptions; timed runs still need remaining budget.
  -- Extending does not itself release this hold; checker parent/deadline guards below still apply.
  AND (runs.kind IN ('chat', 'judge') OR runs.interactive OR runs.started_at IS NULL
       OR (COALESCE(runs.budget_wall_seconds, @global_timeout_seconds::int) + runs.budget_extension_seconds + runs.budget_finalize_seconds)
          - (GREATEST(0, EXTRACT(EPOCH FROM (runs.status_since - runs.started_at))::int) - runs.budget_paused_seconds) > 0)
  AND runs.id IN (SELECT run_id FROM eligible_candidates)
  AND (runs.kind <> 'cross_check' OR EXISTS (
      SELECT 1 FROM locked_checks cc
      JOIN locked_parents lead ON lead.id = cc.lead_run_id
      CROSS JOIN checker_lock_set checks
      WHERE cc.checker_run_id = runs.id AND runs.id = ANY(checks.ids)
        AND cc.lead_run_id = runs.target_run_id AND lead.user_id = runs.user_id
  ))
RETURNING runs.id, runs.user_id, runs.status;
