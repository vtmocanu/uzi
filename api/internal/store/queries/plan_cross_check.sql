-- name: LockWorkerRecoveryParents :many
-- Internal transaction seam: the caller holds the worker row before this query.
-- A non-NULL bounded_target_ids selects exclusively those non-checker targets.
-- Otherwise freeze the conservative union before snapshot replacement clears leases.
-- Phase-specific custody, lease, overflow and budget guards belong to later writes.
-- No checker or cross_checks row is locked here, and no recovery is performed.
WITH queued_continuations AS MATERIALIZED (
    -- Only the newly added queued population is bounded here. Existing active,
    -- snapshot and attested discovery retains its conservative parent union.
    SELECT r.id FROM runs r JOIN workers w ON w.id = r.worker_id AND w.user_id = r.user_id
    WHERE @bounded_target_ids::uuid[] IS NULL AND r.worker_id = @worker_id
      AND @custody_hold_limit::integer > 0
      AND r.status = 'queued' AND r.claim_generation > 0 AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework') AND r.egress_profile_id IS NULL
      AND r.requeue_count - r.requeue_episode_baseline >= @max_requeues::integer
      AND r.pause_requested_at IS NULL AND r.credential_switch_requested_at IS NULL
      AND r.stop_kind IS NULL AND r.stop_reason IS NULL AND r.hold_reason IS NULL
      AND (SELECT count(*) FROM recovery_custody_holds h
          WHERE h.run_id = r.id AND h.user_id = r.user_id AND h.state = 'open'
            AND h.generation BETWEEN 1 AND r.claim_generation) >= @custody_hold_limit
      AND (w.pending_overflow_until IS NULL OR w.pending_overflow_until <= now())
      AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
          WHERE a.worker_id = r.worker_id AND a.run_id = r.id AND a.claim_generation = r.claim_generation
            AND r.claim_released_at IS NULL
            AND (a.reported_at >= @snapshot_fresh_cutoff::timestamptz
                OR (a.terminal_pending AND a.terminal_pending_until > now())))
      AND NOT (@max_requeues > 0 AND r.worker_recovery_episode = 0
          AND r.requeue_count - r.requeue_episode_baseline > @max_requeues
          AND COALESCE(r.finalize_resume_generation = r.claim_generation, false)
          AND r.claim_released_at IS NULL)
    ORDER BY r.id LIMIT 32
), candidates AS MATERIALIZED (
    SELECT r.id, r.kind, r.target_run_id, r.worker_id, r.claim_generation, r.user_id,
           r.status, r.status_since, r.requeue_count, r.claimed_at,
           r.released_worker_id, r.released_worker_nonce, r.stop_kind,
           r.pause_mode, r.credential_switch_generation,
           r.worker_recovery_episode, r.requeue_episode_baseline, r.claim_released_at,
           r.pause_requested_at, r.credential_switch_requested_at, r.stop_reason, r.hold_reason
    FROM runs r
    JOIN workers w ON w.id = r.worker_id AND w.user_id = r.user_id
    WHERE @bounded_target_ids::uuid[] IS NOT NULL
      AND r.id = ANY(@bounded_target_ids::uuid[])
      AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework') AND r.egress_profile_id IS NULL
      AND r.worker_id = ANY(@additional_worker_ids::uuid[])
    UNION ALL
    SELECT r.id, r.kind, r.target_run_id, r.worker_id, r.claim_generation, r.user_id,
           r.status, r.status_since, r.requeue_count, r.claimed_at,
           r.released_worker_id, r.released_worker_nonce, r.stop_kind,
           r.pause_mode, r.credential_switch_generation,
           r.worker_recovery_episode, r.requeue_episode_baseline, r.claim_released_at,
           r.pause_requested_at, r.credential_switch_requested_at, r.stop_reason, r.hold_reason
    FROM runs r
    JOIN workers w ON w.id = r.worker_id AND w.user_id = r.user_id
    WHERE @bounded_target_ids::uuid[] IS NULL
      AND (r.worker_id = @worker_id OR r.worker_id = ANY(@additional_worker_ids::uuid[]))
      AND (
          r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
          OR r.id IN (SELECT id FROM queued_continuations)
          OR r.id = ANY(@snapshot_run_ids::uuid[])
          OR (r.id = ANY(@attested_run_ids::uuid[])
              AND r.claim_generation = (@attested_claim_generations::bigint[])[
                  array_position(@attested_run_ids::uuid[], r.id)])
      )
), frozen_targets AS MATERIALIZED (
    SELECT c.id, c.kind, c.target_run_id, c.worker_id, c.claim_generation, c.user_id,
           c.status, c.status_since, c.requeue_count, c.claimed_at,
           c.released_worker_id, c.released_worker_nonce, c.stop_kind,
           c.pause_mode, c.credential_switch_generation,
           c.worker_recovery_episode, c.requeue_episode_baseline, c.claim_released_at,
           c.pause_requested_at, c.credential_switch_requested_at, c.stop_reason, c.hold_reason,
           cc.id AS cross_check_id, parent.id AS parent_lead_id
    FROM candidates c
    LEFT JOIN cross_checks cc ON c.kind = 'cross_check' AND cc.checker_run_id = c.id
    -- cross_checks is authoritative even if target_run_id disagrees. Preserve both
    -- fields so a later phase can fence that disagreement or a changed association.
    LEFT JOIN runs parent ON parent.id = CASE WHEN c.kind = 'cross_check'
        THEN cc.lead_run_id ELSE c.id END
        AND parent.kind <> 'cross_check' AND parent.user_id = c.user_id
), locked_parents AS MATERIALIZED (
    SELECT parent.id FROM runs parent
    WHERE parent.id IN (SELECT parent_lead_id FROM frozen_targets)
      AND parent.kind <> 'cross_check'
    ORDER BY parent.id
    FOR UPDATE OF parent
), parent_lock_set AS MATERIALIZED (
    -- The aggregate drains the entire ordered lock CTE, including with no targets.
    SELECT COALESCE(array_agg(id ORDER BY id), ARRAY[]::uuid[])::uuid[] AS locked_parent_ids
    FROM locked_parents
)
SELECT target.id, target.kind, target.target_run_id, target.worker_id, target.user_id,
       target.status, target.status_since, target.requeue_count, target.claimed_at,
       target.released_worker_id, target.released_worker_nonce, target.stop_kind,
       target.pause_mode, target.credential_switch_generation,
       target.claim_generation, target.worker_recovery_episode, target.requeue_episode_baseline,
       target.claim_released_at, target.pause_requested_at, target.credential_switch_requested_at, target.stop_reason, target.hold_reason,
       target.cross_check_id, target.parent_lead_id,
       -- Return the parent set once, avoiding targets × parents output growth.
       CASE WHEN row_number() OVER (ORDER BY target.id, target.cross_check_id) = 1
           THEN locks.locked_parent_ids ELSE ARRAY[]::uuid[] END AS locked_parent_ids
FROM parent_lock_set locks CROSS JOIN frozen_targets target
ORDER BY target.id, target.cross_check_id;

-- name: HasLivePlanCrossCheck :one
SELECT EXISTS (
    SELECT 1 FROM cross_checks cc
    JOIN runs lead ON lead.id = cc.lead_run_id
    WHERE lead.id = @lead_run_id AND (lead.plan_cross_check_required OR lead.plan_cross_check_gated)
      AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'
      AND cc.lead_claim_generation = lead.claim_generation
      AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
      AND cc.deadline_at > now()
)::boolean AS has_live_plan_cross_check;

-- name: HasPendingPlanCrossCheck :one
-- Strict settlement proof: even expired, stale or malformed pending rows require settlement.
SELECT EXISTS (
    SELECT 1 FROM cross_checks cc
    WHERE cc.lead_run_id = @lead_run_id AND cc.stage = 'plan' AND cc.verdict = 'pending'
)::boolean AS has_pending_plan_cross_check;


-- name: GetExactPlanCrossCheck :one
SELECT * FROM cross_checks
WHERE lead_run_id = @lead_run_id AND stage = 'plan' AND round = @round FOR UPDATE;

-- name: GetPlanCrossCheckMetadata :one
SELECT round, lead_claim_generation, verdict, reason_class, deadline_at, decided_at,
       automatic_revision_limit, automatic_rounds_enabled, interrupted_at
FROM cross_checks WHERE lead_run_id = @lead_run_id AND stage = 'plan'
ORDER BY round DESC LIMIT 1;

-- name: GetCrossCheckChildProtocol :one
SELECT cc.round FROM cross_checks cc JOIN runs child ON child.id = cc.checker_run_id
JOIN runs lead ON lead.id = cc.lead_run_id
WHERE child.id = @child_id AND child.kind = 'cross_check'
 AND child.target_run_id = lead.id AND child.user_id = lead.user_id
 AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = lead.id AND latest.stage = 'plan')
 AND cc.verdict = 'pending' AND cc.deadline_at > now()
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND cc.lead_claim_generation = lead.claim_generation
 AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1));

-- name: SupersedeUnstoredPlanApproval :one
UPDATE cross_checks cc SET verdict = 'failed', reason_class = 'approved_not_stored',
 interrupted_at = COALESCE(cc.interrupted_at, lead.claim_released_at, lead.status_since)
FROM runs lead WHERE lead.id = cc.lead_run_id AND lead.id = @lead_run_id
 AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = lead.id AND latest.stage = 'plan')
 AND cc.lead_claim_generation <> lead.claim_generation AND cc.verdict = 'approve'
 AND cc.automatic_rounds_enabled
 AND lead.plan_md IS NULL AND lead.auto_approve AND lead.gate_revision = 0
RETURNING cc.*;


-- name: GetPlanCrossCheckClaimingWorkerCaps :one
SELECT worker.protocol_capabilities FROM workers worker JOIN runs lead ON lead.worker_id = worker.id
WHERE lead.id = @lead_run_id AND worker.id = @worker_id AND worker.user_id = @user_id
 AND lead.user_id = worker.user_id AND lead.claim_generation = @claim_generation
 AND lead.claim_released_at IS NULL AND lead.status IN ('claimed','running','awaiting_approval');
