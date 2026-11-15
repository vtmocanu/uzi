-- name: ResumeWorkerRecoveryEpisode :one
-- Explicit owner release of an exhaustion hold is the only episode-renewal writer.
-- Keep the wall budget, custody and released incarnation; bank the hold only after execution started.
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.status = 'recovery_wait' AND runs.recovery_wait_cause IN ('worker_requeue_exhausted', 'retained_recovery_exhausted', 'retained_recovery_adoption_failed', 'retained_continuation_unwitnessed')
  AND NOT (runs.recovery_wait_cause = 'retained_recovery_adoption_failed' AND runs.recovery_wait_detail IS NOT DISTINCT FROM 'secret_remediation_active')
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
      AND ((cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending') OR (cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL))
      AND cc.deadline_at > now()
      AND (lead.status IN ('claimed', 'running') OR (cc.stage = 'code' AND lead.status = 'awaiting_followup' AND lead.kind = 'task' AND lead.interactive))
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
    -- Retained recovery preserves affinity even though its execution claim is released.
    -- retained_continuation_unwitnessed keeps it too: the retained source it parks over lives on
    -- that worker's disk, so the resumed run must requeue back to the same worker.
    worker_id = CASE WHEN runs.claim_released_at IS NOT NULL AND runs.recovery_wait_cause IS DISTINCT FROM 'retained_recovery_exhausted' AND runs.recovery_wait_cause IS DISTINCT FROM 'retained_recovery_adoption_failed' AND runs.recovery_wait_cause IS DISTINCT FROM 'retained_continuation_unwitnessed' THEN NULL ELSE runs.worker_id END,
    hold_reason = NULL,
    hold_captured_head = NULL,
    worker_recovery_episode = runs.worker_recovery_episode + 1,
    requeue_episode_baseline = runs.requeue_count,
    stale_requeue_generation = NULL, worker_recovery_evidence = NULL,
    recovery_wait_cause = NULL, recovery_wait_detail = NULL, recovery_retry_not_before = NULL,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at            = now()
WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.status = 'recovery_wait' AND runs.recovery_wait_cause IN ('worker_requeue_exhausted', 'retained_recovery_exhausted', 'retained_recovery_adoption_failed', 'retained_continuation_unwitnessed')
  AND NOT (runs.recovery_wait_cause = 'retained_recovery_adoption_failed' AND runs.recovery_wait_detail IS NOT DISTINCT FROM 'secret_remediation_active')
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
