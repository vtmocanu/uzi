-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4205 M5: the per-worker placement-eligibility block of the ephemeral capability-gap
-- trigger (ListUnplaceableQueuedRunsForEphemeral), lifted into one inlinable set-returning
-- function so the hibernation wake-demand query evaluates the SAME predicates "as if the
-- worker were online and accepting intake" instead of carrying a second copy.
--
-- It returns the ids of the run owner's workers that clear every PLACEMENT conjunct for run r.
-- It deliberately EXCLUDES the availability conjuncts, which stay in each caller because the two
-- callers read them in opposite directions: status = 'online', fn_worker_accepts_new_intake(w),
-- NOT maintenance_fenced, hibernation_state = 'none', and the cross-check own-worker arm
-- (r.kind = 'cross_check' AND r.worker_id = w.id). The checker-template pin is the caller's
-- checker_template_pins value, joined in as an ordinary argument (p_checker_model), and the
-- Docker preference is the caller's fn_ephemeral_docker_preference_applies(...) result, so the
-- body has no CTE or user-table dependency. STABLE and a single SELECT so the planner inlines it.
-- +goose StatementBegin
CREATE FUNCTION fn_placement_admitting_workers(
    r runs,
    p_checker_model text,
    p_cross_check_evaluated_at timestamptz,
    p_cross_check_affinity_cutoff timestamptz,
    p_ephemeral_lease interval,
    p_docker_preference_applies boolean,
    p_codex_curated_models text[]
) RETURNS TABLE (worker_id uuid) LANGUAGE sql STABLE AS $$
    SELECT w.id FROM workers w
    WHERE w.user_id = r.user_id
        AND (r.kind <> 'cross_check' OR fn_cross_check_child_eligible(w, r, false, 'any', (SELECT stage FROM cross_checks WHERE checker_run_id = r.id), p_cross_check_evaluated_at, p_cross_check_affinity_cutoff))
        AND (r.kind = 'cross_check' OR NOT w.ephemeral
             -- PRD #2006: a LEASED ephemeral worker that may claim r through its lease is capable
             -- and placeable too (advisory mirror of ClaimRun's lease arm, so now()), so r is not
             -- provisioned for.
             OR (fn_ephemeral_lease_admits(
                    w.lease_since, w.lease_repo_id, w.lease_branch, NOT fn_worker_accepts_new_intake(w),
                    p_ephemeral_lease, now(),
                    r.repo_id, r.kind, r.branch, r.pipeline_ref, r.issue_iid, r.failure_snapshot, r.egress_profile_id)
                   AND (NOT p_docker_preference_applies OR COALESCE(w.docker_enabled, false))
                 -- The lease admits only runs outside the isolated lane.
                 AND NOT w.isolated_lane))
        -- Protocol requirements apply to persistent and leased workers alike.
        AND (NOT EXISTS (SELECT 1 FROM run_milestone_reconciliations audit WHERE audit.run_id = r.id)
                      OR 'milestone_reconciliation_claim_v1' = ANY(w.protocol_capabilities))
      AND (r.checkpoint_coordinates IS NULL OR 'task_checkpoint_coordinates_v1' = ANY(w.protocol_capabilities))
      AND (NOT EXISTS (SELECT 1 FROM run_publication_resume pr WHERE pr.run_id = r.id AND pr.state = 'active') OR 'publication_resume_v1' = ANY(w.protocol_capabilities))
      -- Issue #1324 D5: an enrolled run with no handoff is claimable only by its custodian, the side row's worker_id (not runs.worker_id, which resume writers clear); a NULL side worker_id is held. No affinity-ceiling release.
      AND NOT EXISTS (SELECT 1 FROM run_publication_resume pc WHERE pc.run_id = r.id AND pc.state = 'active' AND pc.handoff IS NULL AND pc.worker_id IS DISTINCT FROM w.id)
        AND (r.completion_contract_version IS NULL
                      OR 'completion_interlock_v1' = ANY(w.protocol_capabilities))
        -- PRD #2865: the NON-BYPASSABLE CI-config-guard claim clause (standalone, outside
        -- fn_worker_can_claim, required_capabilities and the capability-aware bind). A GUARDED run
        -- (ci_config_guard) is claimable only by a worker reporting 'ci_config_guard_v1'.
        AND (NOT r.ci_config_guard OR 'ci_config_guard_v1' = ANY(w.protocol_capabilities))
        AND ((NOT r.repo_agent_source_required AND r.repo_agent_source_commit IS NULL) OR 'repo_agent_source_v1' = ANY(w.protocol_capabilities))
        AND (r.agent_source_requested IS NULL OR 'agent_source_request_v1' = ANY(w.protocol_capabilities))
      AND (NOT (r.policy_preserve_required OR r.policy_pending_fresh OR (r.policy_outcome_id IS NOT NULL AND r.policy_resolved_at IS NULL) OR r.policy_required_head IS NOT NULL) OR 'provider_policy_hold_v1' = ANY(w.protocol_capabilities))
        AND (NOT (r.kind = 'prompt' AND r.ci_config_guard AND EXISTS (SELECT 1 FROM job_files af WHERE af.run_id = r.id AND af.direction = 'input' AND af.state = 'attached')) OR 'ci_artifacts_v1' = ANY(w.protocol_capabilities))
                 AND (NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                      OR ('codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities)))
                 AND (r.completion_contract_version IS NULL
                      OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                      OR 'codex_completion_interlock_v1' = ANY(w.protocol_capabilities))
                 AND (
                     NOT (
                         r.harness = 'codex'
                         AND r.kind NOT IN ('judge', 'chat')
                         AND r.review_target_run_id IS NULL
                         AND COALESCE(
                             NOT ((CASE WHEN r.kind = 'cross_check' THEN COALESCE((SELECT pin.model FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = r.id WHERE pin.user_id = r.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness), (SELECT u.default_codex_model FROM users u WHERE u.id = r.user_id),
                                 p_checker_model) WHEN r.model = ANY(p_codex_curated_models) THEN r.model
                                        ELSE (SELECT u2.default_codex_model FROM users u2 WHERE u2.id = r.user_id) END)
                                  = ANY(p_codex_curated_models)),
                             false)
                     )
                     OR 'codex_custom_model_v1' = ANY(w.protocol_capabilities)
                 )
        AND (NOT (r.plan_cross_check_required OR (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan')))
             OR 'cross_check_v1' = ANY(w.protocol_capabilities))
        AND (NOT (r.kind = 'cross_check' AND r.harness = 'claude' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
             OR 'cross_check_codex_lead_v1' = ANY(w.protocol_capabilities))
        AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM semantic_planning sp WHERE sp.run_id = r.target_run_id) AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
             OR 'cross_check_semantic_plan_v1' = ANY(w.protocol_capabilities))
        AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM cross_checks pc JOIN cross_check_owner_steering s ON s.cross_check_id = pc.id WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
             OR 'cross_check_owner_steering_v1' = ANY(w.protocol_capabilities))
        AND (r.kind = 'cross_check' OR NOT EXISTS (SELECT 1 FROM semantic_planning sp WHERE sp.run_id = r.id)
             OR 'completion_semantic_publication_v1' = ANY(w.protocol_capabilities))
      AND (r.kind <> 'cross_check' OR EXISTS (
          SELECT 1 FROM cross_checks protocol_check JOIN runs protocol_lead ON protocol_lead.id = protocol_check.lead_run_id
          WHERE protocol_check.checker_run_id = r.id
            AND protocol_check.lead_run_id = r.target_run_id AND protocol_lead.user_id = r.user_id
            AND protocol_lead.kind <> 'cross_check' AND protocol_check.stage IN ('plan','code')
            AND protocol_check.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = protocol_check.lead_run_id AND latest.stage = protocol_check.stage)
            AND ((protocol_check.stage = 'plan' AND protocol_check.verdict = 'pending') OR (protocol_check.stage = 'code' AND protocol_check.outcome = 'pending' AND protocol_check.interrupted_at IS NULL AND 'cross_check_code_v1' = ANY(w.protocol_capabilities) AND 'cross_check_lane_v1' = ANY(w.protocol_capabilities))) AND protocol_check.deadline_at > now()
            AND (protocol_lead.status IN ('claimed','running') OR (protocol_check.stage = 'code' AND protocol_lead.status = 'awaiting_followup' AND protocol_lead.kind = 'task' AND protocol_lead.interactive)) AND protocol_lead.claim_released_at IS NULL
            AND protocol_lead.claim_generation = protocol_check.lead_claim_generation
            AND (protocol_check.round = 1 OR (protocol_check.automatic_rounds_enabled
                AND protocol_check.round <= protocol_check.automatic_revision_limit + 1
                AND 'cross_check_rounds_v1' = ANY(w.protocol_capabilities)))
      ))
      AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = r.id WHERE pin.user_id = r.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness AND (pin.model IS NOT NULL OR pin.effort IS NOT NULL))) OR 'cross_check_pins_v1' = ANY(w.protocol_capabilities))
        AND r.required_capabilities <@ (COALESCE(w.capabilities, '{}') || CASE WHEN COALESCE(w.docker_enabled, false) THEN ARRAY['docker'] ELSE ARRAY[]::text[] END)
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Queries are Go constants: rolling this migration back requires the pre-M5 binary, whose
-- ListUnplaceableQueuedRunsForEphemeral carries the block inline (same as every function migration).
DROP FUNCTION fn_placement_admitting_workers(runs, text, timestamptz, timestamptz, interval, boolean, text[]);
