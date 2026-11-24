package store_test

// Frozen c6eee1d45 text of the old-writer statements the fresh resume guard must still refuse by
// itself. The current queries carry the pending/claimed NOT EXISTS literal and match no row, so the
// trigger backstop (an old binary, or a writer outside the registry) is exercised with the base text.
const (
	base_setRunRecoveryWait = `-- name: SetRunRecoveryWait :execrows
UPDATE runs SET
    status                    = 'recovery_wait',
    status_since              = now(),
    recovery_wait_count       = recovery_wait_count + 1,
    recovery_wait_cause       = $1::text,
    idle_stall_park_count = COALESCE($2::integer, idle_stall_park_count),
    provider_park_count = CASE WHEN $3::boolean
        AND $4::bigint > 0
        AND claim_generation = $4::bigint
        THEN 0 ELSE provider_park_count END,
    provider_park_streak_started_at = CASE WHEN $3::boolean
        AND $4::bigint > 0
        AND claim_generation = $4::bigint
        THEN NULL ELSE provider_park_streak_started_at END,
    recovery_retry_not_before = $5,
    session_id                = COALESCE($6, session_id),
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at                = now()
WHERE id = $7 AND worker_id = $8
  AND status = 'running'
  AND kind NOT IN ('judge', 'job', 'cross_check')
  -- PRD #1247 M5a-1 rework (reviewer NB1): the per-query generation fence, identical to
  -- SetRunLimitWait's and the SAME nil-guarded shape as InsertRunMessage. A stale report from an
  -- OLD flight — reclaimed to a NEW generation under same-worker affinity, or against a released
  -- claim — matches 0 rows, so a fenced-out park cannot clobber the reclaiming flight's run. A
  -- legacy worker (NULL generation) parks unconditionally, unchanged. sqlc.narg, never @name (the
  -- multibyte comment blocks break the @name parser).
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND claim_released_at IS NULL
  AND ($4::bigint IS NULL
       OR claim_generation = $4::bigint)
`
	base_setRunFailed = `-- name: SetRunFailed :execrows
UPDATE runs SET
    plan_cross_check_gate_reason = NULL,
    status             = 'failed',
    status_since       = now(),
    failure_reason     = $1,
    -- PRD #69 M7a: the TRUSTED failure class, always set from Go (the worker-reported
    -- ` + "`" + `failed` + "`" + ` arm coerces req.fail_origin through the allowlist and defaults a
    -- classless failure to 'agent_failure'; the limit-opt-out non-park path stamps
    -- 'rate_limited'). Never derived from failure_reason, which is never parsed.
    fail_origin        = $2,
    -- PRD #377 M1: the agent's secret-scrubbed, size-capped branch diff, preserved on a
    -- workflow_scope_missing failure so a human can apply the work the bot PAT could not
    -- push. NULL on every other failed path (only that arm sends a non-nil value).
    preserved_patch    = $3,
    session_id         = COALESCE($4, session_id),
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    -- #3607 M2a: a terminal run carries no required-recovery identity.
    policy_required_head = NULL, policy_recovery_verified_generation = NULL, policy_recovery_verified_head = NULL,
    updated_at         = now()
WHERE NOT (status = 'recovery_wait' AND recovery_wait_cause IS NOT DISTINCT FROM 'data_volume_full' AND recovery_retry_not_before IS NULL)
  -- Owner-held Anthropic spend-limit hold (always NULL retry): the same late-report refusal.
  AND NOT (status = 'recovery_wait' AND recovery_wait_cause IS NOT DISTINCT FROM 'anthropic_spend_limit')
  AND hold_reason IS DISTINCT FROM 'codex_policy_hold'
  AND (policy_outcome_id IS NULL OR policy_resolved_at IS NOT NULL)
  AND id = $5 AND worker_id = $6 AND (kind <> 'cross_check' OR claim_generation > 0)
  AND NOT (status = 'paused' AND COALESCE(hold_reason = 'iteration_budget_exhausted', FALSE))
  AND status NOT IN ('completed', 'failed', 'cancelled')
  AND status <> 'paused'
  -- All paused runs are protected above. Retain provider protection through
  -- ResumePausedRun's queued interval; after ClaimRun, the service generation fence applies.
  AND NOT (status IN ('paused', 'queued') AND COALESCE(hold_reason IN ('provider_unavailable','codex_policy_hold'), FALSE))
  -- PRD #3252 m4: nor may one leave the phase-switch hold, which keeps this claim through ResumePausedRun's queued interval.
  AND NOT (status IN ('paused', 'queued') AND COALESCE(hold_reason = 'phase_switch_unavailable', FALSE))
  -- PRD #1247 M5a-1 rework (m6): the per-query generation fence, the SAME nil-guarded shape as
  -- UpdateRunLastSeq/InsertRunMessage. limit_wait (non-park + forge-park DEGRADED) callers skip
  -- the outer FOR UPDATE fence, so when a generation is supplied the fail applies ONLY to the
  -- still-held run at that exact generation: a late gen-G report matches 0 rows against a run
  -- released after G (claim_released_at set) or reclaimed to G+1 (generation moved on), so it
  -- cannot clobber the reclaiming flight. nil = legacy/outer-lock-fenced callers, unchanged.
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND claim_released_at IS NULL
  AND ($7::bigint IS NULL
       OR claim_generation = $7::bigint)
`
	base_parkRunRetainedRecovery = `-- name: ParkRunRetainedRecovery :one
UPDATE runs SET status='recovery_wait', status_since=now(), updated_at=now(),
 recovery_wait_cause = $1::text,
 recovery_wait_detail = CASE WHEN $1::text = 'retained_recovery_adoption_failed' THEN $2::text END,
 recovery_wait_count=recovery_wait_count+1,
 retained_park_count=retained_park_count+CASE WHEN $3::boolean THEN 1 ELSE 0 END,
 recovery_retry_not_before=$4::timestamptz,
 session_id=COALESCE($5::text, session_id),
 checkpoint_contains_latest=$6::boolean,
 claim_released_at=now(),
 released_worker_id=CASE WHEN $2::text = 'source_end_declined' THEN worker_id ELSE released_worker_id END,
 released_worker_nonce=CASE WHEN $2::text = 'source_end_declined'
     THEN (SELECT workers.snapshot_register_nonce FROM workers WHERE workers.id = runs.worker_id) ELSE released_worker_nonce END,
 health='ok', health_reason=NULL, health_since=NULL
WHERE runs.id = $7 AND runs.worker_id = $8 AND (status='running' OR (status='claimed' AND $2::text = 'source_end_declined'))
 AND claim_generation = $9::bigint AND $9::bigint > 0
 AND claim_released_at IS NULL AND COALESCE(stop_kind, '')=''
 AND kind IN ('issue','ci_fix','self_improve','prompt','task','mr_rework')
RETURNING runs.id, runs.user_id, runs.repo_id, runs.issue_iid, runs.issue_title, runs.issue_description, runs.status, runs.requeue_count, runs.worker_id, runs.session_id, runs.last_seq, runs.branch, runs.mr_iid, runs.failure_reason, runs.plan_md, runs.iteration_count, runs.claimed_at, runs.started_at, runs.finished_at, runs.created_at, runs.updated_at, runs.origin_column, runs.board_column, runs.move_pending_since, runs.mr_state, runs.auto_approve, runs.autopilot_commented_at, runs.kind, runs.pipeline_id, runs.pipeline_ref, runs.failure_snapshot, runs.fix_verdict, runs.stop_kind, runs.agent_source, runs.agent_exclusions, runs.repo_agents, runs.title, runs.resume_of_run_id, runs.last_activity_at, runs.health, runs.health_reason, runs.health_since, runs.health_notified_at, runs.target_run_id, runs.mr_web_url, runs.prd_done_path, runs.prd_patch_settled_at, runs.anthropic_secret_id, runs.anthropic_secret_label, runs.anthropic_select_reason, runs.anthropic_headroom_pct, runs.wait_on_limit, runs.limit_resets_at, runs.retry_not_before, runs.limit_wait_count, runs.rate_limit_type, runs.open_question_id, runs.revise_count, runs.plan_source, runs.planned_base_commit, runs.require_base_match, runs.milestones_candidate, runs.milestones_frozen, runs.milestones_completed, runs.milestones_in_progress, runs.budget_max_iterations, runs.budget_wall_seconds, runs.schedule_id, runs.limit_dead_secret_id, runs.report_only, runs.report_md, runs.ci_config_paths, runs.model, runs.override_subagent_model, runs.fail_origin, runs.priority, runs.summary_intent, runs.summary_plan, runs.summary_deltas, runs.issue_comments, runs.base_branch, runs.open_mr, runs.dispatched_at, runs.review_target_run_id, runs.review_requested, runs.then_fix_requested, runs.then_fix_of_run_id, runs.preserved_patch, runs.required_capabilities, runs.stop_reason, runs.required_tools, runs.size_class, runs.interactive, runs.open_followup_id, runs.plan_changed_files, runs.scope_ceiling, runs.status_since, runs.review_comments, runs.budget_paused_seconds, runs.mr_rework_enabled, runs.trigger_source, runs.checkpoint_tip, runs.usage_refolded, runs.codex_secret_id, runs.codex_auth_mode, runs.codex_secret_label, runs.codex_account_key, runs.codex_material_revision, runs.codex_account_revision, runs.codex_claim_epoch, runs.codex_cap_hash, runs.pause_requested_at, runs.pause_mode, runs.pause_after_count, runs.checkpoint_tip_at, runs.recovery_wait_count, runs.recovery_retry_not_before, runs.completion_contract_version, runs.contract_revision, runs.completion_contract, runs.completion_attempts, runs.latest_completion_attempt, runs.milestones_agents, runs.hold_reason, runs.hold_captured_head, runs.completion_budget_exhausted_at, runs.completion_question_at, runs.budget_extension_seconds, runs.claim_generation, runs.harness, runs.recovery_wait_cause, runs.forge_park_count, runs.credential_override_mode, runs.credential_override_secret_id, runs.claim_released_at, runs.credential_switch_requested_at, runs.credential_switch_generation, runs.stale_requeue_generation, runs.budget_finalize_seconds, runs.released_worker_id, runs.released_worker_nonce, runs.gate_revision, runs.gate_presentation_id, runs.gate_presented_payload, runs.gate_payload_digest, runs.gate_refusal_count, runs.gate_refusal_generation, runs.disk_park_count, runs.checkpoint_contains_latest, runs.egress_profile_id, runs.egress_snapshot, runs.job_type, runs.finalize_resume_generation, runs.job_protocol, runs.first_started_at, runs.plan_cross_check_required, runs.plan_cross_check_gate_reason, runs.issue_raw_digest, runs.issue_saved_body, runs.issue_input_reason, runs.auto_approve_blocked_reasons, runs.plan_cross_check_diff_refusal, runs.worker_recovery_episode, runs.requeue_episode_baseline, runs.worker_recovery_evidence, runs.cross_check_lane, runs.cross_check_lane_generation, runs.completion_final_head, runs.code_cross_check_required, runs.provider_park_count, runs.provider_park_streak_started_at, runs.ci_config_guard, runs.memory_episode, runs.memory_intervention_count, runs.memory_policy, runs.completion_non_closing_reason, runs.ci_artifact_manifest, runs.checkpoint_attested_source, runs.source_publication_intent, runs.continued_issue_iid, runs.checkpoint_coordinates, runs.retained_park_count, runs.ignored_inferred_capabilities, runs.reserve_hold_reason, runs.reserve_exempt_until, runs.reserve_exempt_granted_at, runs.reserve_exempt_credential_id, runs.reserve_exempt_window_reset, runs.reserve_exempt_prior_pct, runs.hinted_capabilities, runs.capability_inference_evidence, runs.idle_stall_park_count, runs.forge_admission, runs.codex_select_reason, runs.recovery_wait_phase, runs.plan_cross_check_gated, runs.policy_preserve_required, runs.policy_pending_fresh, runs.policy_outcome_id, runs.policy_continuation_mode, runs.policy_resolved_at, runs.policy_applying_worker_id, runs.policy_applying_generation, runs.policy_source_generation, runs.policy_refusal_count, runs.policy_refusal_tag, runs.policy_refusal_phase, runs.policy_refusal_correlation_id, runs.policy_hold_published, runs.harness_preferred, runs.harness_decision_reason, runs.harness_preferred_score, runs.harness_other_score, runs.harness_preferred_measured, runs.harness_other_measured, runs.harness_preferred_held, runs.harness_other_held, runs.harness_preferred_weekly_reset_at, runs.harness_other_weekly_reset_at, runs.registration_evidence_generation, runs.registration_evidence_nonce, runs.registration_evidence_epoch, runs.snapshot_departed_generation, runs.snapshot_departed_nonce, runs.snapshot_departed_epoch, runs.repo_agent_source_commit, runs.repo_agent_source_required, runs.agent_source_requested, runs.agent_source_requested_basis, runs.plan_agent_source, runs.ci_fix_mr_iid, runs.policy_required_head, runs.policy_recovery_verified_generation, runs.policy_recovery_verified_head, runs.policy_hold_cause, runs.policy_fresh_consumed_generation, runs.policy_fresh_confirmed_generation, runs.recovery_wait_detail, runs.credential_switch_op_id, runs.credential_switch_hold_op_id, runs.credential_switch_hold_generation, runs.credential_switch_hold_worker_id, runs.credential_switch_hold_register_nonce, runs.effort, runs.model_source, runs.effort_source, runs.checkpoint_withheld_since, runs.checkpoint_withheld_status_since, runs.checkpoint_withheld_worker_id, runs.checkpoint_withheld_generation, runs.checkpoint_withheld_nonce, runs.checkpoint_withheld_report_seq, runs.model_provider_id, runs.model_provider_revision, runs.model_provider_key_mode, runs.model_provider_key_secret_id, runs.model_provider_key_rev, runs.model_provider_limit_streak, runs.claim_provenance_worker_id, runs.claim_provenance_generation, runs.health_owner_hold_since, runs.followup_park_seq, runs.followup_finalize_generation, runs.codex_switch_target_secret_id, runs.codex_switch_target_generation, runs.codex_switch_target_op_id, runs.codex_switch_target_auth_mode, runs.codex_switch_target_material_revision, runs.codex_switch_target_account_key, runs.codex_switch_target_account_revision, runs.ci_autofix_enabled
`
	base_requeueRunsOfStaleWorkers = `-- name: RequeueRunsOfStaleWorkers :many
WITH locked AS (
    SELECT workers.id FROM workers
    WHERE workers.last_heartbeat_at IS NULL OR workers.last_heartbeat_at < $2
    ORDER BY workers.id
    FOR UPDATE
), candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND (runs.requeue_count - runs.requeue_episode_baseline) < $1
      AND runs.worker_id IN (SELECT id FROM locked)
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.id, lead.user_id, lead.repo_id, lead.issue_iid, lead.issue_title, lead.issue_description, lead.status, lead.requeue_count, lead.worker_id, lead.session_id, lead.last_seq, lead.branch, lead.mr_iid, lead.failure_reason, lead.plan_md, lead.iteration_count, lead.claimed_at, lead.started_at, lead.finished_at, lead.created_at, lead.updated_at, lead.origin_column, lead.board_column, lead.move_pending_since, lead.mr_state, lead.auto_approve, lead.autopilot_commented_at, lead.kind, lead.pipeline_id, lead.pipeline_ref, lead.failure_snapshot, lead.fix_verdict, lead.stop_kind, lead.agent_source, lead.agent_exclusions, lead.repo_agents, lead.title, lead.resume_of_run_id, lead.last_activity_at, lead.health, lead.health_reason, lead.health_since, lead.health_notified_at, lead.target_run_id, lead.mr_web_url, lead.prd_done_path, lead.prd_patch_settled_at, lead.anthropic_secret_id, lead.anthropic_secret_label, lead.anthropic_select_reason, lead.anthropic_headroom_pct, lead.wait_on_limit, lead.limit_resets_at, lead.retry_not_before, lead.limit_wait_count, lead.rate_limit_type, lead.open_question_id, lead.revise_count, lead.plan_source, lead.planned_base_commit, lead.require_base_match, lead.milestones_candidate, lead.milestones_frozen, lead.milestones_completed, lead.milestones_in_progress, lead.budget_max_iterations, lead.budget_wall_seconds, lead.schedule_id, lead.limit_dead_secret_id, lead.report_only, lead.report_md, lead.ci_config_paths, lead.model, lead.override_subagent_model, lead.fail_origin, lead.priority, lead.summary_intent, lead.summary_plan, lead.summary_deltas, lead.issue_comments, lead.base_branch, lead.open_mr, lead.dispatched_at, lead.review_target_run_id, lead.review_requested, lead.then_fix_requested, lead.then_fix_of_run_id, lead.preserved_patch, lead.required_capabilities, lead.stop_reason, lead.required_tools, lead.size_class, lead.interactive, lead.open_followup_id, lead.plan_changed_files, lead.scope_ceiling, lead.status_since, lead.review_comments, lead.budget_paused_seconds, lead.mr_rework_enabled, lead.trigger_source, lead.checkpoint_tip, lead.usage_refolded, lead.codex_secret_id, lead.codex_auth_mode, lead.codex_secret_label, lead.codex_account_key, lead.codex_material_revision, lead.codex_account_revision, lead.codex_claim_epoch, lead.codex_cap_hash, lead.pause_requested_at, lead.pause_mode, lead.pause_after_count, lead.checkpoint_tip_at, lead.recovery_wait_count, lead.recovery_retry_not_before, lead.completion_contract_version, lead.contract_revision, lead.completion_contract, lead.completion_attempts, lead.latest_completion_attempt, lead.milestones_agents, lead.hold_reason, lead.hold_captured_head, lead.completion_budget_exhausted_at, lead.completion_question_at, lead.budget_extension_seconds, lead.claim_generation, lead.harness, lead.recovery_wait_cause, lead.forge_park_count, lead.credential_override_mode, lead.credential_override_secret_id, lead.claim_released_at, lead.credential_switch_requested_at, lead.credential_switch_generation, lead.stale_requeue_generation, lead.budget_finalize_seconds, lead.released_worker_id, lead.released_worker_nonce, lead.gate_revision, lead.gate_presentation_id, lead.gate_presented_payload, lead.gate_payload_digest, lead.gate_refusal_count, lead.gate_refusal_generation, lead.disk_park_count, lead.checkpoint_contains_latest, lead.egress_profile_id, lead.egress_snapshot, lead.job_type, lead.finalize_resume_generation, lead.job_protocol, lead.first_started_at, lead.plan_cross_check_required, lead.plan_cross_check_gate_reason, lead.issue_raw_digest, lead.issue_saved_body, lead.issue_input_reason, lead.auto_approve_blocked_reasons, lead.plan_cross_check_diff_refusal, lead.worker_recovery_episode, lead.requeue_episode_baseline, lead.worker_recovery_evidence, lead.cross_check_lane, lead.cross_check_lane_generation, lead.completion_final_head, lead.code_cross_check_required, lead.provider_park_count, lead.provider_park_streak_started_at, lead.ci_config_guard, lead.memory_episode, lead.memory_intervention_count, lead.memory_policy, lead.completion_non_closing_reason, lead.ci_artifact_manifest, lead.checkpoint_attested_source, lead.source_publication_intent, lead.continued_issue_iid, lead.checkpoint_coordinates, lead.retained_park_count, lead.ignored_inferred_capabilities, lead.reserve_hold_reason, lead.reserve_exempt_until, lead.reserve_exempt_granted_at, lead.reserve_exempt_credential_id, lead.reserve_exempt_window_reset, lead.reserve_exempt_prior_pct, lead.hinted_capabilities, lead.capability_inference_evidence, lead.idle_stall_park_count, lead.forge_admission, lead.codex_select_reason, lead.recovery_wait_phase, lead.plan_cross_check_gated, lead.policy_preserve_required, lead.policy_pending_fresh, lead.policy_outcome_id, lead.policy_continuation_mode, lead.policy_resolved_at, lead.policy_applying_worker_id, lead.policy_applying_generation, lead.policy_source_generation, lead.policy_refusal_count, lead.policy_refusal_tag, lead.policy_refusal_phase, lead.policy_refusal_correlation_id, lead.policy_hold_published, lead.harness_preferred, lead.harness_decision_reason, lead.harness_preferred_score, lead.harness_other_score, lead.harness_preferred_measured, lead.harness_other_measured, lead.harness_preferred_held, lead.harness_other_held, lead.harness_preferred_weekly_reset_at, lead.harness_other_weekly_reset_at, lead.registration_evidence_generation, lead.registration_evidence_nonce, lead.registration_evidence_epoch, lead.snapshot_departed_generation, lead.snapshot_departed_nonce, lead.snapshot_departed_epoch, lead.repo_agent_source_commit, lead.repo_agent_source_required, lead.agent_source_requested, lead.agent_source_requested_basis, lead.plan_agent_source, lead.ci_fix_mr_iid, lead.policy_required_head, lead.policy_recovery_verified_generation, lead.policy_recovery_verified_head, lead.policy_hold_cause, lead.policy_fresh_consumed_generation, lead.policy_fresh_confirmed_generation, lead.recovery_wait_detail, lead.credential_switch_op_id, lead.credential_switch_hold_op_id, lead.credential_switch_hold_generation, lead.credential_switch_hold_worker_id, lead.credential_switch_hold_register_nonce, lead.effort, lead.model_source, lead.effort_source, lead.checkpoint_withheld_since, lead.checkpoint_withheld_status_since, lead.checkpoint_withheld_worker_id, lead.checkpoint_withheld_generation, lead.checkpoint_withheld_nonce, lead.checkpoint_withheld_report_seq, lead.model_provider_id, lead.model_provider_revision, lead.model_provider_key_mode, lead.model_provider_key_secret_id, lead.model_provider_key_rev, lead.model_provider_limit_streak, lead.claim_provenance_worker_id, lead.claim_provenance_generation, lead.health_owner_hold_since, lead.followup_park_seq, lead.followup_finalize_generation, lead.codex_switch_target_secret_id, lead.codex_switch_target_generation, lead.codex_switch_target_op_id, lead.codex_switch_target_auth_mode, lead.codex_switch_target_material_revision, lead.codex_switch_target_account_key, lead.codex_switch_target_account_revision, lead.ci_autofix_enabled FROM runs lead
    WHERE lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
    ORDER BY lead.id
    FOR UPDATE OF lead
), parent_lock_set AS MATERIALIZED (
    -- Collect the actual locked IDs completely before any checker can be mutated.
    SELECT array_agg(id) AS ids FROM locked_parents
), locked_checks AS MATERIALIZED (
    SELECT cc.id, cc.lead_run_id, cc.stage, cc.round, cc.lead_claim_generation, cc.plan_md, cc.milestones, cc.required_capabilities, cc.required_tools, cc.size_class, cc.base_commit, cc.planning_diff, cc.candidate_digest, cc.checker_run_id, cc.checker_harness, cc.checker_model, cc.checker_effort, cc.verdict, cc.reason_class, cc.findings, cc.decided_at, cc.deadline_at, cc.created_at, cc.checker_model_source, cc.checker_effort_source, cc.automatic_revision_limit, cc.automatic_rounds_enabled, cc.interrupted_at, cc.wait_credited, cc.head_commit, cc.outcome, cc.code_context, cc.guidance_snapshot, cc.guidance_text, cc.guidance_digest, cc.repo_instructions_enabled, cc.repo_instructions_text, cc.repo_instructions_digest, cc.dispositions, cc.finalized_at, cc.repo_instructions_commit, cc.coverage FROM cross_checks cc
    JOIN locked_parents lead ON lead.id = cc.lead_run_id
    CROSS JOIN parent_lock_set locks
    WHERE cc.checker_run_id IN (SELECT id FROM candidates WHERE kind = 'cross_check')
      AND cc.lead_run_id = ANY(locks.ids)
      AND ((cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending') OR (cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL))
      AND cc.deadline_at > now()
      AND (lead.status IN ('claimed', 'running') OR (cc.stage = 'code' AND lead.status = 'awaiting_followup' AND lead.kind = 'task' AND lead.interactive))
      AND (cc.stage <> 'code' OR lead.worker_id = (SELECT child.worker_id FROM runs child WHERE child.id = cc.checker_run_id))
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
      -- A same-batch lead exit owns its checker cancellation through the trigger.
      AND NOT EXISTS (
          SELECT 1 FROM cross_checks cc
          JOIN candidates exiting ON exiting.id = cc.lead_run_id
          JOIN locked_parents lead ON lead.id = exiting.id
          WHERE cc.checker_run_id = mapping.run_id
            AND lead.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
              AND (lead.requeue_count - lead.requeue_episode_baseline) < $1
              AND lead.worker_id IN (SELECT id FROM locked)
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
    -- PRD #1390 M2b (D2): record the generation this stale-worker requeue charged, so the
    -- heartbeat re-adoption can refund the requeue only when the same generation is restored
    -- (a legitimate earlier loss is never refunded). The restore and every ClaimRun clear it.
    stale_requeue_generation = runs.claim_generation,
    -- #2705 invariant: this column plus a fresh status_since (set above) is what pins the run
    -- to its stale owner in ClaimRun for WORKER_STALE_REQUEUE_GRACE. Every fresh claim clears
    -- it; park/promote paths keep it (per-episode pin, see ClaimRun's comment).
    -- Issue #783: bank park time before a worker-death requeue -> queued, since started_at
    -- survives the requeue and the later claimed->running resume would not see the park.
    -- awaiting_followup is intentionally excluded: interactive runs are exempt from
    -- SweepRunningTimeout entirely (interactive = false), so they have no wall deadline.
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN runs.status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    -- #2988: preserve the capability for provisional same-generation re-adoption.
    -- Queued suspends credential use; a successful ClaimRun supersedes the old
    -- capability atomically even when the same worker claims again.
    updated_at = now()
FROM eligible_candidates candidate
WHERE runs.id = candidate.run_id
  AND runs.hold_reason IS DISTINCT FROM 'codex_policy_hold'
  AND (runs.policy_outcome_id IS NULL OR runs.policy_resolved_at IS NOT NULL)
  AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND (runs.requeue_count - runs.requeue_episode_baseline) < $1
  AND runs.worker_id IN (SELECT id FROM locked)
  -- PRD #1390 D11: terminal-pending lease + pending_overflow closure, chat-exempt (D10).
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
  AND (runs.kind <> 'cross_check' OR EXISTS (
      SELECT 1 FROM locked_checks cc
      JOIN locked_parents lead ON lead.id = cc.lead_run_id
      CROSS JOIN checker_lock_set checks
      WHERE cc.checker_run_id = runs.id AND runs.id = ANY(checks.ids)
        AND cc.lead_run_id = runs.target_run_id AND lead.user_id = runs.user_id
  ))
RETURNING runs.id, runs.user_id, runs.status
`
	base_setRunCheckpointTip = `-- name: SetRunCheckpointTip :execrows
UPDATE runs SET checkpoint_tip = $1, checkpoint_tip_at = now(),
    checkpoint_attested_source = COALESCE($2::text, checkpoint_attested_source)
WHERE id = $3
`
)
