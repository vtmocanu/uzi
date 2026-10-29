-- Frozen pre-M3 API promoter: regression evidence only, run in a rolled-back transaction.
-- name: PromoteRecoveryWaitRunNow :execrows
WITH candidates AS MATERIALIZED (
    SELECT runs.id, runs.kind FROM runs
    WHERE runs.id = $1 AND runs.user_id = $2 AND runs.status = 'recovery_wait'
  AND (runs.recovery_wait_cause IS DISTINCT FROM 'codex_idle_stall' OR runs.recovery_retry_not_before IS NOT NULL)
  AND runs.recovery_wait_cause IS DISTINCT FROM 'codex_account_unavailable'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'worker_requeue_exhausted'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'worker_memory_pressure'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'retained_recovery_adoption_failed'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'retained_recovery_exhausted'
), parent_mapping AS MATERIALIZED (
    SELECT candidates.id AS run_id, parent.id AS parent_id
    FROM candidates
    LEFT JOIN cross_checks cc ON candidates.kind = 'cross_check'
        AND cc.checker_run_id = candidates.id
    LEFT JOIN runs parent ON parent.id = CASE WHEN candidates.kind = 'cross_check'
        THEN cc.lead_run_id ELSE candidates.id END
        AND parent.kind <> 'cross_check'
), locked_parents AS MATERIALIZED (
    SELECT lead.id, lead.user_id, lead.repo_id, lead.issue_iid, lead.issue_title, lead.issue_description, lead.status, lead.requeue_count, lead.worker_id, lead.session_id, lead.last_seq, lead.branch, lead.mr_iid, lead.failure_reason, lead.plan_md, lead.iteration_count, lead.claimed_at, lead.started_at, lead.finished_at, lead.created_at, lead.updated_at, lead.origin_column, lead.board_column, lead.move_pending_since, lead.mr_state, lead.auto_approve, lead.autopilot_commented_at, lead.kind, lead.pipeline_id, lead.pipeline_ref, lead.failure_snapshot, lead.fix_verdict, lead.stop_kind, lead.agent_source, lead.agent_exclusions, lead.repo_agents, lead.title, lead.resume_of_run_id, lead.last_activity_at, lead.health, lead.health_reason, lead.health_since, lead.health_notified_at, lead.target_run_id, lead.mr_web_url, lead.prd_done_path, lead.prd_patch_settled_at, lead.anthropic_secret_id, lead.anthropic_secret_label, lead.anthropic_select_reason, lead.anthropic_headroom_pct, lead.wait_on_limit, lead.limit_resets_at, lead.retry_not_before, lead.limit_wait_count, lead.rate_limit_type, lead.open_question_id, lead.revise_count, lead.plan_source, lead.planned_base_commit, lead.require_base_match, lead.milestones_candidate, lead.milestones_frozen, lead.milestones_completed, lead.milestones_in_progress, lead.budget_max_iterations, lead.budget_wall_seconds, lead.schedule_id, lead.limit_dead_secret_id, lead.report_only, lead.report_md, lead.ci_config_paths, lead.model, lead.override_subagent_model, lead.fail_origin, lead.priority, lead.summary_intent, lead.summary_plan, lead.summary_deltas, lead.issue_comments, lead.base_branch, lead.open_mr, lead.dispatched_at, lead.review_target_run_id, lead.review_requested, lead.then_fix_requested, lead.then_fix_of_run_id, lead.preserved_patch, lead.required_capabilities, lead.stop_reason, lead.required_tools, lead.size_class, lead.interactive, lead.open_followup_id, lead.plan_changed_files, lead.scope_ceiling, lead.status_since, lead.review_comments, lead.budget_paused_seconds, lead.mr_rework_enabled, lead.trigger_source, lead.checkpoint_tip, lead.usage_refolded, lead.codex_secret_id, lead.codex_auth_mode, lead.codex_secret_label, lead.codex_account_key, lead.codex_material_revision, lead.codex_account_revision, lead.codex_claim_epoch, lead.codex_cap_hash, lead.pause_requested_at, lead.pause_mode, lead.pause_after_count, lead.checkpoint_tip_at, lead.recovery_wait_count, lead.recovery_retry_not_before, lead.completion_contract_version, lead.contract_revision, lead.completion_contract, lead.completion_attempts, lead.latest_completion_attempt, lead.milestones_agents, lead.hold_reason, lead.hold_captured_head, lead.completion_budget_exhausted_at, lead.completion_question_at, lead.budget_extension_seconds, lead.claim_generation, lead.harness, lead.recovery_wait_cause, lead.forge_park_count, lead.credential_override_mode, lead.credential_override_secret_id, lead.claim_released_at, lead.credential_switch_requested_at, lead.credential_switch_generation, lead.stale_requeue_generation, lead.budget_finalize_seconds, lead.released_worker_id, lead.released_worker_nonce, lead.gate_revision, lead.gate_presentation_id, lead.gate_presented_payload, lead.gate_payload_digest, lead.gate_refusal_count, lead.gate_refusal_generation, lead.disk_park_count, lead.checkpoint_contains_latest, lead.egress_profile_id, lead.egress_snapshot, lead.job_type, lead.finalize_resume_generation, lead.job_protocol, lead.first_started_at, lead.plan_cross_check_required, lead.plan_cross_check_gate_reason, lead.issue_raw_digest, lead.issue_saved_body, lead.issue_input_reason, lead.auto_approve_blocked_reasons, lead.plan_cross_check_diff_refusal, lead.worker_recovery_episode, lead.requeue_episode_baseline, lead.worker_recovery_evidence, lead.cross_check_lane, lead.cross_check_lane_generation, lead.completion_final_head, lead.code_cross_check_required, lead.provider_park_count, lead.provider_park_streak_started_at, lead.ci_config_guard, lead.memory_episode, lead.memory_intervention_count, lead.memory_policy, lead.completion_non_closing_reason, lead.ci_artifact_manifest, lead.checkpoint_attested_source, lead.source_publication_intent, lead.continued_issue_iid, lead.checkpoint_coordinates, lead.retained_park_count, lead.ignored_inferred_capabilities, lead.reserve_hold_reason, lead.reserve_exempt_until, lead.reserve_exempt_granted_at, lead.reserve_exempt_credential_id, lead.reserve_exempt_window_reset, lead.reserve_exempt_prior_pct, lead.hinted_capabilities, lead.capability_inference_evidence, lead.idle_stall_park_count, lead.forge_admission FROM runs lead
    WHERE lead.id IN (SELECT parent_id FROM parent_mapping)
      AND lead.kind <> 'cross_check'
    ORDER BY lead.id
    FOR UPDATE OF lead
), parent_lock_set AS MATERIALIZED (
    -- Consume every selected parent lock before taking a checker lock.
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
    status     = 'queued',
    status_since = now(),
    started_at = NULL,
    budget_paused_seconds = 0,
    -- PRD #1733: the owner's early promote (set-token / credential switch) is the decision to try
    -- again, so it clears the open provider-park streak. The automatic promotions
    -- (PromoteRecoveryWaitRuns, PromoteVaultLockedRecoveryWaitRuns) PRESERVE it.
    provider_park_count = 0, provider_park_streak_started_at = NULL,
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = $1 AND runs.user_id = $2 AND runs.status = 'recovery_wait'
  AND (runs.recovery_wait_cause IS DISTINCT FROM 'codex_idle_stall' OR runs.recovery_retry_not_before IS NOT NULL)
  AND runs.recovery_wait_cause IS DISTINCT FROM 'codex_account_unavailable'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'worker_requeue_exhausted'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'worker_memory_pressure'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'retained_recovery_adoption_failed'
  AND runs.recovery_wait_cause IS DISTINCT FROM 'retained_recovery_exhausted'
  AND runs.id IN (SELECT run_id FROM eligible_candidates)
  AND (runs.kind <> 'cross_check' OR EXISTS (
      SELECT 1 FROM locked_checks cc
      JOIN locked_parents lead ON lead.id = cc.lead_run_id
      CROSS JOIN checker_lock_set checks
      WHERE cc.checker_run_id = runs.id AND runs.id = ANY(checks.ids)
        AND cc.lead_run_id = runs.target_run_id AND lead.user_id = runs.user_id
  ))
