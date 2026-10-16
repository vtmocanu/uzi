-- name: SetRunPolicyHold :one
WITH eligible AS MATERIALIZED (
 SELECT r.id FROM runs r
 WHERE r.id = @id AND r.worker_id = @worker_id
 AND r.claim_generation = @claim_generation::bigint AND r.claim_released_at IS NULL
 AND r.harness = 'codex' AND NOT r.interactive
 AND r.kind IN ('issue', 'task', 'prompt', 'self_improve')
 AND r.stop_kind IS NULL
 AND (r.status = 'running'
   OR (r.status = 'claimed' AND (@detail::text = 'resume_rejected' OR @source_generation::bigint < r.claim_generation))
   OR (r.status = 'awaiting_approval' AND r.gate_revision = sqlc.narg('expected_gate_revision')::bigint
       AND (SELECT m.kind FROM run_messages m WHERE m.run_id = r.id
         AND m.claim_generation = r.claim_generation
         AND m.kind IN ('plan', 'plan_revising') ORDER BY m.seq DESC LIMIT 1) = 'plan_revising'))
), consumed_pause AS (
 UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
 WHERE u.run_id IN (SELECT id FROM eligible)
 AND u.kind IN ('pause', 'pause_cancel') AND u.applied_at IS NULL
)
UPDATE runs SET status = 'paused', status_since = now(),
 session_id = COALESCE(sqlc.narg('session_id'), runs.session_id),
 hold_reason = 'provider_policy_refusal', hold_captured_head = sqlc.narg('head'),
 checkpoint_contains_latest = sqlc.narg('checkpoint_contains_latest'),
 policy_hold_provenance = @provenance, policy_hold_published = @published,
 policy_hold_detail = @detail,
 policy_refusal_count = policy_refusal_count + CASE WHEN @detail = 'refused' THEN 1 ELSE 0 END,
 policy_last_tag = CASE WHEN @detail = 'refused' THEN sqlc.narg('tag') ELSE policy_last_tag END,
 policy_last_phase = CASE WHEN @detail = 'refused' THEN sqlc.narg('phase') ELSE policy_last_phase END,
 policy_last_correlation_id = CASE WHEN @detail = 'refused' THEN sqlc.narg('correlation_id') ELSE policy_last_correlation_id END,
 policy_last_refused_at = CASE WHEN @detail = 'refused' THEN now() ELSE policy_last_refused_at END,
 policy_last_outcome_id = @outcome_id, policy_last_source_generation = @source_generation,
 pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
 health = 'ok', health_reason = NULL, health_since = NULL, updated_at = now()
WHERE runs.id IN (SELECT id FROM eligible)
 AND runs.worker_id = @worker_id AND runs.claim_generation = @claim_generation
 AND runs.claim_released_at IS NULL AND runs.harness = 'codex' AND NOT runs.interactive
 AND runs.kind IN ('issue', 'task', 'prompt', 'self_improve')
RETURNING runs.*;

-- name: ResumePolicyHold :one
UPDATE runs SET status = 'queued', status_since = now(),
 budget_paused_seconds = budget_paused_seconds + GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int),
 policy_fresh_claim_generation = NULL,
 codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
 health = 'ok', health_reason = NULL, health_since = NULL, updated_at = now()
WHERE id = @id AND user_id = @user_id AND status = 'paused'
 AND hold_reason = 'provider_policy_refusal'
 AND policy_last_outcome_id = @outcome_id AND claim_generation = @claim_generation
RETURNING *;

-- name: CountOnlineWorkersSatisfyingPolicyHold :one
-- Count policy-capable candidates satisfying the run's strict static placement,
-- affinity and persistent/ephemeral binding rules. Slots are intentionally ignored
-- for the protocol diagnostic, as in CountOnlineWorkersClaimableForRun.StrictEligible.
WITH candidates AS (
SELECT w.id, rh.pacing, rh.pacing_observed_at, rh.blocking_reason, rh.phase,
       (w.last_heartbeat_at IS NOT NULL AND w.last_heartbeat_at >= @heartbeat_cutoff)::boolean AS heartbeat_recent,
       (run.egress_profile_id IS NULL)::boolean AS diagnostic_lane,
       (run.kind <> 'cross_check' OR run.worker_id IS NULL OR run.worker_id = w.id
        OR run.updated_at < @cross_check_affinity_cutoff::timestamptz)::boolean AS diagnostic_affinity,
       (w.kind = 'hosted' AND rh.pacing_observed_at <= @cross_check_evaluated_at::timestamptz
        AND rh.pacing_observed_at >= (@cross_check_evaluated_at::timestamptz) - interval '60 seconds'
        AND rh.pacing->>'state' IN ('held','active','waiting_evidence','override')
        AND NOT w.maintenance_fenced AND NOT w.maintenance_owns_drain
        AND w.maintenance_phase NOT IN ('requested','ready','stopping','recycling')
        AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.live_worker_id = w.id AND h.state = 'open')
        AND (w.draining_since IS NULL OR w.draining_reason = 'image_roll')
        AND (w.status <> 'online' OR w.last_heartbeat_at IS NULL OR w.last_heartbeat_at < @heartbeat_cutoff
             OR (w.draining_since IS NOT NULL AND w.draining_reason = 'image_roll')
             OR rh.phase IN ('rolling','stuck'))) IS TRUE AS through_roll,
       CASE WHEN w.maintenance_fenced OR w.maintenance_phase IN ('requested','ready','stopping','recycling') THEN COALESCE(w.draining_since, w.maintenance_activity_floor) ELSE w.draining_since END AS draining_since,
       (w.status = 'online') AS online,
       (CASE WHEN run.kind = 'cross_check' THEN fn_cross_check_child_eligible(w, run, true, 'any', (SELECT stage FROM cross_checks WHERE checker_run_id = run.id), @cross_check_evaluated_at::timestamptz, @cross_check_affinity_cutoff::timestamptz)
             ELSE w.max_concurrent_runs IS NULL OR wa.active < w.max_concurrent_runs END) AS free_slot,
       (run.kind = 'cross_check' OR NOT w.ephemeral OR w.ephemeral_run_id = run.id OR (fn_ephemeral_lease_admits(
              w.lease_since, w.lease_repo_id, w.lease_branch, w.draining_since IS NOT NULL,
              @ephemeral_lease::interval, now(),
              run.repo_id, run.kind, run.branch, run.pipeline_ref, run.issue_iid, run.failure_snapshot, run.egress_profile_id)
                   AND (NOT fn_ephemeral_docker_preference_applies(owner.ephemeral_docker_enabled, @worker_docker_enabled::boolean, run.repo_id, run.kind, run.egress_profile_id, @docker_repo_allowlist::uuid[]) OR COALESCE(w.docker_enabled, false)))) AS advisory_binding,
       (run.kind = 'cross_check' OR NOT w.ephemeral OR w.ephemeral_run_id = run.id OR (
           (fn_ephemeral_lease_admits(
              w.lease_since, w.lease_repo_id, w.lease_branch, w.draining_since IS NOT NULL,
              @ephemeral_lease::interval, now(),
              run.repo_id, run.kind, run.branch, run.pipeline_ref, run.issue_iid, run.failure_snapshot, run.egress_profile_id)
                   AND (NOT fn_ephemeral_docker_preference_applies(owner.ephemeral_docker_enabled, @worker_docker_enabled::boolean, run.repo_id, run.kind, run.egress_profile_id, @docker_repo_allowlist::uuid[]) OR COALESCE(w.docker_enabled, false)))
           AND NOT EXISTS (SELECT 1 FROM workers bw WHERE bw.ephemeral AND bw.ephemeral_run_id = run.id)
           AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds ch WHERE ch.live_worker_id = w.id AND ch.state = 'open')
       )) AS strict_binding,
       (run.worker_id = w.id) AS own_worker,
       (NOT w.ephemeral) AS persistent,
       (run.kind = 'cross_check' OR run.worker_id IS NULL OR run.worker_id = w.id
        OR NOT EXISTS (SELECT 1 FROM workers ow WHERE ow.id = run.worker_id
                       AND (ow.draining_since IS NOT NULL OR ow.maintenance_phase IN ('requested','ready','stopping','recycling') OR ow.maintenance_fenced OR
                            (ow.last_heartbeat_at IS NOT NULL AND ow.last_heartbeat_at >= @heartbeat_cutoff)
                            -- #2705 pin P, mirroring ClaimRun's stale-requeue grace arm.
                            OR (run.kind <> 'cross_check'
                                AND run.stale_requeue_generation = run.claim_generation
                                AND COALESCE(run.status_since > sqlc.narg('stale_requeue_cutoff')::timestamptz, false))))
        OR run.updated_at < @affinity_cutoff) AS affinity
FROM runs run
JOIN users owner ON owner.id = run.user_id
JOIN workers w ON w.user_id = run.user_id
LEFT JOIN worker_upgrade_reports rh ON rh.worker_id = w.id
CROSS JOIN LATERAL (
    SELECT count(*) AS active FROM runs pr
    WHERE pr.worker_id = w.id
      AND pr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND pr.kind <> 'chat' AND NOT pr.cross_check_lane
) wa
WHERE run.id = @run_id
  AND (run.kind <> 'cross_check' OR fn_cross_check_child_eligible(w, run, false, 'any', (SELECT stage FROM cross_checks WHERE checker_run_id = run.id), @cross_check_evaluated_at::timestamptz, @cross_check_affinity_cutoff::timestamptz))
  AND fn_worker_can_claim(
        COALESCE(w.docker_enabled, false),
        @docker_repo_allowlist::uuid[],
        run.repo_id,
        run.kind,
        COALESCE(w.capabilities, '{}')::text[],
        run.required_capabilities,
        @capability_aware::boolean)
  AND (NOT EXISTS (SELECT 1 FROM run_milestone_reconciliations audit WHERE audit.run_id = run.id)
       OR 'milestone_reconciliation_claim_v1' = ANY(w.protocol_capabilities))
      AND ((run.status NOT IN ('completed', 'failed', 'cancelled') AND run.policy_refusal_count > 0 OR run.hold_reason = 'provider_policy_refusal') IS NOT TRUE
           OR 'provider_policy_hold_v1' = ANY(w.protocol_capabilities))
  AND (run.completion_contract_version IS NULL
       OR 'completion_interlock_v1' = ANY(w.protocol_capabilities))
  -- PRD #2865: the NON-BYPASSABLE CI-config-guard claim clause (standalone, outside
  -- fn_worker_can_claim, required_capabilities and @capability_aware). A GUARDED run
  -- (ci_config_guard) is claimable only by a worker reporting 'ci_config_guard_v1'.
  AND (NOT run.ci_config_guard OR 'ci_config_guard_v1' = ANY(w.protocol_capabilities))
  AND (NOT (run.harness = 'codex' OR run.codex_material_revision IS NOT NULL OR run.codex_secret_id IS NOT NULL)
       OR ('codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities)))
  AND (run.completion_contract_version IS NULL
       OR NOT (run.harness = 'codex' OR run.codex_material_revision IS NOT NULL OR run.codex_secret_id IS NOT NULL)
       OR 'codex_completion_interlock_v1' = ANY(w.protocol_capabilities))
      AND (NOT (run.plan_cross_check_required OR (run.kind = 'cross_check' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = run.id AND pc.stage = 'plan')))
           OR 'cross_check_v1' = ANY(w.protocol_capabilities))
      AND (NOT (run.kind = 'cross_check' AND run.harness = 'claude' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = run.id AND pc.stage = 'plan'))
           OR 'cross_check_codex_lead_v1' = ANY(w.protocol_capabilities))
      AND (run.kind <> 'cross_check' OR EXISTS (
          SELECT 1 FROM cross_checks protocol_check JOIN runs protocol_lead ON protocol_lead.id = protocol_check.lead_run_id
          WHERE protocol_check.checker_run_id = run.id
            AND protocol_check.lead_run_id = run.target_run_id AND protocol_lead.user_id = run.user_id
            AND protocol_lead.kind <> 'cross_check' AND protocol_check.stage IN ('plan','code')
            AND protocol_check.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = protocol_check.lead_run_id AND latest.stage = protocol_check.stage)
            AND ((protocol_check.stage = 'plan' AND protocol_check.verdict = 'pending') OR (protocol_check.stage = 'code' AND protocol_check.outcome = 'pending' AND protocol_check.interrupted_at IS NULL AND 'cross_check_code_v1' = ANY(w.protocol_capabilities) AND 'cross_check_lane_v1' = ANY(w.protocol_capabilities))) AND protocol_check.deadline_at > now()
            AND (protocol_lead.status IN ('claimed','running') OR (protocol_check.stage = 'code' AND protocol_lead.status = 'awaiting_followup' AND protocol_lead.kind = 'task' AND protocol_lead.interactive)) AND protocol_lead.claim_released_at IS NULL
            AND protocol_lead.claim_generation = protocol_check.lead_claim_generation
            AND (protocol_check.round = 1 OR (protocol_check.automatic_rounds_enabled
                AND protocol_check.round <= protocol_check.automatic_revision_limit + 1
                AND 'cross_check_rounds_v1' = ANY(w.protocol_capabilities)))
      ))
      AND (NOT (run.kind = 'cross_check' AND EXISTS (SELECT 1 FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = run.id WHERE pin.user_id = run.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness AND (pin.model IS NOT NULL OR pin.effort IS NOT NULL))) OR 'cross_check_pins_v1' = ANY(w.protocol_capabilities))
  -- PRD #1551 M4 (D6): MIRROR ClaimRun's non-bypassable custom-Codex-model clause, so this
  -- claimable count and the claim gate never disagree. The effective-root expression is written
  -- IDENTICALLY to ClaimRun (run.model/curated-else-lane, NULL-safe via COALESCE), reading the
  -- candidate worker's OWN protocol_capabilities.
  AND (
      NOT (
          run.harness = 'codex'
          AND run.kind NOT IN ('judge', 'chat')
          AND run.review_target_run_id IS NULL
          AND COALESCE(
              NOT ((CASE WHEN run.kind = 'cross_check' THEN COALESCE((SELECT pin.model FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = run.id WHERE pin.user_id = run.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness), (SELECT u.default_codex_model FROM users u WHERE u.id = run.user_id)) WHEN run.model = ANY(@codex_curated_models::text[]) THEN run.model
                         ELSE (SELECT u.default_codex_model FROM users u WHERE u.id = run.user_id) END)
                   = ANY(@codex_curated_models::text[])),
              false)
      )
      OR 'codex_custom_model_v1' = ANY(w.protocol_capabilities)
  )
  -- PRD #1906 M5: MIRROR ClaimRun's isolated-lane clause (both ways, the protocol capability,
  -- and no Codex-indicating profile-bound run), reading the candidate's OWN columns.
  AND ((run.egress_profile_id IS NOT NULL) = w.isolated_lane)
  AND (run.egress_profile_id IS NULL OR 'isolated_fetch_v1' = ANY(w.protocol_capabilities))
  AND (run.egress_profile_id IS NULL OR NOT (run.harness = 'codex' OR run.codex_material_revision IS NOT NULL OR run.codex_secret_id IS NOT NULL))
  -- PRD #1908 (D-A): MIRROR ClaimRun's job-runner clause (non-docker AND job_runner_v1).
  -- PRD #1909 M1: and the job_files_v1 arm for a stamped job (runs.job_protocol).
  AND (run.kind <> 'job'
       OR (NOT COALESCE(w.docker_enabled, false) AND 'job_runner_v1' = ANY(w.protocol_capabilities)
           AND (run.job_protocol IS NULL OR 'job_files_v1' = ANY(w.protocol_capabilities))
           -- PRD #1976 M1: and the isolated_job_v1 arm for a profile-bound job.
           AND (run.egress_profile_id IS NULL OR 'isolated_job_v1' = ANY(w.protocol_capabilities))))
  AND (run.released_worker_id IS NULL
       OR run.released_worker_id <> w.id
       OR run.released_worker_nonce IS DISTINCT FROM w.snapshot_register_nonce)
)
SELECT count(*) FILTER (WHERE heartbeat_recent AND online AND affinity AND strict_binding)::bigint
FROM candidates;
