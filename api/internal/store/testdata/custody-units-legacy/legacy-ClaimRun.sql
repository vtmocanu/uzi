-- name: ClaimRun :one
WITH checker_template_pins AS MATERIALIZED (
    SELECT owners.user_id,
           (SELECT CASE WHEN t.codex_model IN ('gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-luna', 'gpt-6-sol', 'gpt-6.1-sol') THEN t.codex_model END
            FROM agent_templates t
            LEFT JOIN agent_template_allocations uo ON uo.template_id = t.id AND uo.user_id = owners.user_id
            LEFT JOIN agent_template_allocations g ON g.template_id = t.id AND g.user_id IS NULL
            WHERE (t.scope IN ('builtin', 'global') OR (t.scope = 'user' AND t.user_id = owners.user_id))
              AND CASE WHEN uo.template_id IS NOT NULL THEN uo.enabled ELSE COALESCE(g.enabled, false) END
              AND NOT (t.scope = 'user' AND EXISTS (SELECT 1 FROM agent_templates s WHERE s.scope <> 'user' AND s.name = t.name))
              AND lower(t.name) IN ('lead', 'orchestrator')
            ORDER BY t.name LIMIT 1) AS codex_model
    FROM (
        SELECT DISTINCT r.user_id FROM runs r
        WHERE r.kind = 'cross_check' AND r.harness = 'codex'
          AND r.status = 'queued' AND r.user_id = $3
    ) owners
), claimant AS MATERIALIZED (
    SELECT id, user_id, name, token_hash, status, last_heartbeat_at, version, created_at, updated_at, template_declared, template_reported, max_concurrent_runs, stats_cpu_pct, stats_mem_bytes, stats_mem_limit_bytes, stats_source, kind, hosted_size, hosted_generation, docker_enabled, anthropic_secret_id, anthropic_bind_mode, online_since, draining_since, capabilities, ephemeral, ephemeral_run_id, stats_disk_nix_bytes, stats_disk_nix_total_bytes, stats_disk_data_bytes, stats_disk_data_total_bytes, stats_disk_pressure_streak, protocol_capabilities, snapshot_epoch, snapshot_register_nonce, pending_overflow, pending_overflow_until, stats_disk_dind_bytes, stats_disk_dind_total_bytes, stats_disk_dind_inodes, stats_disk_dind_total_inodes, stats_disk_data_inodes, stats_disk_data_total_inodes, isolated_lane, lease_since, lease_repo_id, lease_branch, dind_register_floor, dind_meter_epoch, dind_meter_at, dind_pressure_streak, dind_meter_over, dind_below_threshold, maintenance_owns_drain, nix_pressure, data_pressure, maintenance_id, maintenance_nonce, maintenance_phase, maintenance_deployment_uid, maintenance_pvc_uid, maintenance_register_nonce, maintenance_fenced, maintenance_ready_ack, maintenance_ack_at, maintenance_activity_floor, max_cross_check_slots, registered_pod_uid, roll_heartbeat_at, draining_reason, manual_cordoned_at, manual_cordoned_by, manual_cordoned_by_label, manual_cordon_reason FROM workers WHERE id = $1 FOR UPDATE
), target AS (
    SELECT r.id, r.user_id, r.repo_id, r.kind, r.claim_generation, r.egress_profile_id,
           r.policy_pending_fresh,
           fo.connection_id AS forge_connection_id, fo.origin AS forge_origin, fc.incarnation AS forge_incarnation,
           fc.observation_version AS forge_observation_version,
           fo.api_origin AS forge_api_origin, fca.incarnation AS forge_api_incarnation,
           fca.observation_version AS forge_api_observation_version
    FROM runs r
    LEFT JOIN authorized_forge_origins fo
      ON fo.repo_id = r.repo_id AND fo.user_id = r.user_id
        AND r.egress_profile_id IS NULL
        -- A code checker never contacts the forge, so it takes no origin reservation.
        AND NOT (r.kind = 'cross_check' AND EXISTS (
            SELECT 1 FROM cross_checks cc
            WHERE cc.lead_run_id = r.target_run_id AND cc.stage = 'code' AND cc.checker_run_id = r.id))
    LEFT JOIN forge_origin_cooldowns fc
      ON fc.user_id = r.user_id AND fc.worker_id = $1 AND fc.origin = fo.origin
    -- Issue #1324 D9: the server-derived API origin is a second admission key only when it
    -- differs from the Git origin (equal origins share fc's row). NULL is ignored.
    LEFT JOIN forge_origin_cooldowns fca
      ON fca.user_id = r.user_id AND fca.worker_id = $1 AND fca.origin = fo.api_origin
        AND fo.api_origin <> fo.origin
    WHERE r.user_id = $3
      -- Hard admission even for a lone worker, an old run, or its own resume.
      AND EXISTS (SELECT 1 FROM claimant c WHERE fn_forge_origin_eligible(c, r, now()))
      -- Admission is a snapshot read, before generation advances or custody opens.
      -- NULL is unrestricted; the deadline includes the account's recovery spread.
      AND COALESCE(codex_admission_run_eligible_at(r.id) <= now(), true)
      AND EXISTS (SELECT 1 FROM claimant c WHERE NOT c.maintenance_fenced
        AND (fn_worker_accepts_new_intake(c)
             OR r.worker_id = c.id))
      AND r.kind <> 'chat' AND NOT r.cross_check_lane
      AND ((COALESCE($2::text, '') IN ('', 'run', 'ordinary') AND r.kind <> 'cross_check')
           OR (r.kind = 'cross_check' AND EXISTS (
               SELECT 1 FROM claimant c WHERE fn_cross_check_child_eligible(c, r, true,
                   CASE WHEN COALESCE($2::text, '') = '' THEN 'run' ELSE $2::text END,
                   (SELECT stage FROM cross_checks WHERE checker_run_id = r.id), $4::timestamptz, $5::timestamptz))))
      -- PRD #400 Decision 6: a task run is claimable ONLY after the CLI has seeded its
      -- uzi/task/<id> branch and stamped dispatched_at — otherwise a worker could claim
      -- it before the branch exists (the claim-before-seed race). Every non-task kind is
      -- unaffected (dispatched_at is only ever set on a task run).
      AND (r.kind <> 'task' OR r.dispatched_at IS NOT NULL)
      AND r.status = 'queued'
      -- PRD #3454 D2/D4: during an instance intake pause only admitted runs (claimed at
      -- least once, or cross_check children) may claim; a new root stays queued.
      AND (NOT (SELECT fn_instance_intake_paused()) OR fn_run_admitted_during_intake_pause(r))
      -- Lock only this checker's parent, then its pending attempt, before the target
      -- row and custody side effects. A changed verdict is rechecked on the locked cc.
      AND (r.kind <> 'cross_check' OR EXISTS (
          SELECT 1 FROM (
              SELECT cc.id, cc.lead_run_id, cc.stage, cc.round, cc.lead_claim_generation, cc.plan_md, cc.milestones, cc.required_capabilities, cc.required_tools, cc.size_class, cc.base_commit, cc.planning_diff, cc.candidate_digest, cc.checker_run_id, cc.checker_harness, cc.checker_model, cc.checker_effort, cc.verdict, cc.reason_class, cc.findings, cc.decided_at, cc.deadline_at, cc.created_at, cc.checker_model_source, cc.checker_effort_source, cc.automatic_revision_limit, cc.automatic_rounds_enabled, cc.interrupted_at, cc.wait_credited, cc.head_commit, cc.outcome, cc.code_context, cc.guidance_snapshot, cc.guidance_text, cc.guidance_digest, cc.repo_instructions_enabled, cc.repo_instructions_text, cc.repo_instructions_digest, cc.dispositions, cc.finalized_at, cc.repo_instructions_commit, cc.coverage FROM cross_checks cc
              JOIN (
                  SELECT lead.id, lead.user_id, lead.repo_id, lead.issue_iid, lead.issue_title, lead.issue_description, lead.status, lead.requeue_count, lead.worker_id, lead.session_id, lead.last_seq, lead.branch, lead.mr_iid, lead.failure_reason, lead.plan_md, lead.iteration_count, lead.claimed_at, lead.started_at, lead.finished_at, lead.created_at, lead.updated_at, lead.origin_column, lead.board_column, lead.move_pending_since, lead.mr_state, lead.auto_approve, lead.autopilot_commented_at, lead.kind, lead.pipeline_id, lead.pipeline_ref, lead.failure_snapshot, lead.fix_verdict, lead.stop_kind, lead.agent_source, lead.agent_exclusions, lead.repo_agents, lead.title, lead.resume_of_run_id, lead.last_activity_at, lead.health, lead.health_reason, lead.health_since, lead.health_notified_at, lead.target_run_id, lead.mr_web_url, lead.prd_done_path, lead.prd_patch_settled_at, lead.anthropic_secret_id, lead.anthropic_secret_label, lead.anthropic_select_reason, lead.anthropic_headroom_pct, lead.wait_on_limit, lead.limit_resets_at, lead.retry_not_before, lead.limit_wait_count, lead.rate_limit_type, lead.open_question_id, lead.revise_count, lead.plan_source, lead.planned_base_commit, lead.require_base_match, lead.milestones_candidate, lead.milestones_frozen, lead.milestones_completed, lead.milestones_in_progress, lead.budget_max_iterations, lead.budget_wall_seconds, lead.schedule_id, lead.limit_dead_secret_id, lead.report_only, lead.report_md, lead.ci_config_paths, lead.model, lead.override_subagent_model, lead.fail_origin, lead.priority, lead.summary_intent, lead.summary_plan, lead.summary_deltas, lead.issue_comments, lead.base_branch, lead.open_mr, lead.dispatched_at, lead.review_target_run_id, lead.review_requested, lead.then_fix_requested, lead.then_fix_of_run_id, lead.preserved_patch, lead.required_capabilities, lead.stop_reason, lead.required_tools, lead.size_class, lead.interactive, lead.open_followup_id, lead.plan_changed_files, lead.scope_ceiling, lead.status_since, lead.review_comments, lead.budget_paused_seconds, lead.mr_rework_enabled, lead.trigger_source, lead.checkpoint_tip, lead.usage_refolded, lead.codex_secret_id, lead.codex_auth_mode, lead.codex_secret_label, lead.codex_account_key, lead.codex_material_revision, lead.codex_account_revision, lead.codex_claim_epoch, lead.codex_cap_hash, lead.pause_requested_at, lead.pause_mode, lead.pause_after_count, lead.checkpoint_tip_at, lead.recovery_wait_count, lead.recovery_retry_not_before, lead.completion_contract_version, lead.contract_revision, lead.completion_contract, lead.completion_attempts, lead.latest_completion_attempt, lead.milestones_agents, lead.hold_reason, lead.hold_captured_head, lead.completion_budget_exhausted_at, lead.completion_question_at, lead.budget_extension_seconds, lead.claim_generation, lead.harness, lead.recovery_wait_cause, lead.forge_park_count, lead.credential_override_mode, lead.credential_override_secret_id, lead.claim_released_at, lead.credential_switch_requested_at, lead.credential_switch_generation, lead.stale_requeue_generation, lead.budget_finalize_seconds, lead.released_worker_id, lead.released_worker_nonce, lead.gate_revision, lead.gate_presentation_id, lead.gate_presented_payload, lead.gate_payload_digest, lead.gate_refusal_count, lead.gate_refusal_generation, lead.disk_park_count, lead.checkpoint_contains_latest, lead.egress_profile_id, lead.egress_snapshot, lead.job_type, lead.finalize_resume_generation, lead.job_protocol, lead.first_started_at, lead.plan_cross_check_required, lead.plan_cross_check_gate_reason, lead.issue_raw_digest, lead.issue_saved_body, lead.issue_input_reason, lead.auto_approve_blocked_reasons, lead.plan_cross_check_diff_refusal, lead.worker_recovery_episode, lead.requeue_episode_baseline, lead.worker_recovery_evidence, lead.cross_check_lane, lead.cross_check_lane_generation, lead.completion_final_head, lead.code_cross_check_required, lead.provider_park_count, lead.provider_park_streak_started_at, lead.ci_config_guard, lead.memory_episode, lead.memory_intervention_count, lead.memory_policy, lead.completion_non_closing_reason, lead.ci_artifact_manifest, lead.checkpoint_attested_source, lead.source_publication_intent, lead.continued_issue_iid, lead.checkpoint_coordinates, lead.retained_park_count, lead.ignored_inferred_capabilities, lead.reserve_hold_reason, lead.reserve_exempt_until, lead.reserve_exempt_granted_at, lead.reserve_exempt_credential_id, lead.reserve_exempt_window_reset, lead.reserve_exempt_prior_pct, lead.hinted_capabilities, lead.capability_inference_evidence, lead.idle_stall_park_count, lead.forge_admission, lead.codex_select_reason, lead.recovery_wait_phase, lead.plan_cross_check_gated, lead.policy_preserve_required, lead.policy_pending_fresh, lead.policy_outcome_id, lead.policy_continuation_mode, lead.policy_resolved_at, lead.policy_applying_worker_id, lead.policy_applying_generation, lead.policy_source_generation, lead.policy_refusal_count, lead.policy_refusal_tag, lead.policy_refusal_phase, lead.policy_refusal_correlation_id, lead.policy_hold_published, lead.harness_preferred, lead.harness_decision_reason, lead.harness_preferred_score, lead.harness_other_score, lead.harness_preferred_measured, lead.harness_other_measured, lead.harness_preferred_held, lead.harness_other_held, lead.harness_preferred_weekly_reset_at, lead.harness_other_weekly_reset_at, lead.registration_evidence_generation, lead.registration_evidence_nonce, lead.registration_evidence_epoch, lead.snapshot_departed_generation, lead.snapshot_departed_nonce, lead.snapshot_departed_epoch, lead.repo_agent_source_commit, lead.repo_agent_source_required, lead.agent_source_requested, lead.agent_source_requested_basis, lead.plan_agent_source, lead.ci_fix_mr_iid, lead.policy_required_head, lead.policy_recovery_verified_generation, lead.policy_recovery_verified_head, lead.policy_hold_cause, lead.policy_fresh_consumed_generation, lead.policy_fresh_confirmed_generation FROM runs lead
                  WHERE lead.id = r.target_run_id AND lead.user_id = r.user_id
                    AND lead.kind <> 'cross_check'
                  ORDER BY lead.id
                  FOR UPDATE OF lead SKIP LOCKED
              ) lead ON lead.id = cc.lead_run_id
              WHERE cc.checker_run_id = r.id AND cc.lead_run_id = r.target_run_id
                AND ((cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending') OR (cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL))
                AND cc.deadline_at > now()
                AND (lead.status IN ('claimed', 'running') OR (cc.stage = 'code' AND lead.status = 'awaiting_followup' AND lead.kind = 'task' AND lead.interactive))
                AND lead.claim_released_at IS NULL
                AND lead.claim_generation = cc.lead_claim_generation
              ORDER BY cc.lead_run_id
              FOR UPDATE OF cc SKIP LOCKED
          ) locked_checks
      ))
      AND (r.kind = 'cross_check' OR r.worker_id IS NULL
           OR r.worker_id = $1
           -- Hold the pin whenever the run's OWN worker ROW still exists AND it can
           -- still resume the run — i.e. it is either DRAINING (cordoned for an image
           -- roll: same worker row, same PVC, new pod on the far side) OR its heartbeat
           -- is fresh. Fall open ONLY when the row is GONE (teardown — DeleteWorkerForUser
           -- / ReapEphemeralWorkers delete the row API-side BEFORE the controller's kube
           -- teardown, so `teardown ⟺ row absent`) or the owner is heartbeat-STALE AND
           -- NOT draining (death/hang — ADR-628 D3a's protected case). This is PRD #1030
           -- M2: the roll-vs-teardown discriminator with no new column. The old rule
           -- required `draining_since IS NULL` in the EXISTS, so a roll-cordoned owner
           -- (draining) failed the test → NOT EXISTS became TRUE → a parked run fell open
           -- and a live peer stole it cold across the roll (run #1009). We deliberately
           -- hold on `draining regardless of heartbeat` because the ~2 min pod-swap edge
           -- exceeds the 45s @heartbeat_cutoff, so a heartbeat-gated hold would wrongly
           -- fall open mid-swap (accepted tradeoff D7: a draining+dead owner then holds
           -- until the @affinity_cutoff ceiling).
           -- Forge failure releases this pin early only with complete guarded
           -- source-history proof; an empty current hold is not absence proof.
           OR NOT EXISTS (
               SELECT 1 FROM workers ow
               WHERE ow.id = r.worker_id
                 AND fn_forge_origin_owner_pins(ow, r, now())
                 AND (ow.draining_since IS NOT NULL OR ow.maintenance_phase IN ('requested','ready','stopping','recycling') OR ow.maintenance_fenced
                      OR (ow.last_heartbeat_at IS NOT NULL
                          AND ow.last_heartbeat_at >= $6)
                      -- #2705 pin P: a run the stale sweeper requeued (stale_requeue_generation
                      -- = claim_generation, set only by RequeueRunsOfStaleWorkers, cleared by
                      -- every fresh claim) stays pinned to its stale owner for
                      -- WORKER_STALE_REQUEUE_GRACE from status_since, so a returning worker
                      -- (a roll, a network blip) resumes it. NULL cutoff (grace 0) never pins.
                      -- Per-episode only: PromoteCodexAccountWaitRun resets status_since AND
                      -- updated_at, so Codex account park/promote cycles renew both this grace
                      -- and the ceiling below; there is NO global bound across cycles.
                      -- stale_requeue_generation is deliberately kept through the park for
                      -- #1390 readoption/refund provenance.
                      OR (r.kind <> 'cross_check'
                          AND r.stale_requeue_generation = r.claim_generation
                          AND COALESCE(r.status_since > $7::timestamptz, false))))
           -- Generous ceiling bounding the live-but-can't-serve case; @affinity_cutoff is
           -- now now() - WORKER_AFFINITY_CEILING (default 2h), NOT the 2-min grace.
           OR r.updated_at < $8)
      -- PRD #1030 M2 / PRD #422 Decision 7: a DRAINING claimant claims NOTHING NEW. It
      -- reaches ClaimRun now (the workersvc early return that made it claim nothing was
      -- removed so it can re-claim its OWN promoted run through a roll), but it must be
      -- scoped to runs it already owns — never a new/unclaimed/fallen-open run. When the
      -- claimant is not draining this is a no-op; when it is, only its own runs qualify.
      AND (NOT $9::boolean OR r.worker_id = $1)
      -- PRD #216 D5: claiming-worker eligibility via the shared expression.
      -- PRD #84 M2 extends it: the run's required_capabilities must be a subset of the
      -- claiming worker's effective caps (@worker_caps ∪ docker), gated by @capability_aware.
      AND fn_worker_can_claim($10::boolean, $11::uuid[], r.repo_id, r.kind, $12::text[], r.required_capabilities, $13::boolean)
      -- PRD #1296 M1 (D2/D4): custody-admission. Block the claim when the run's OWNER
      -- already holds >= @custody_hold_limit admission-counted open holds (ADR-2445) — the
      -- owner has too much unpublished work awaiting recovery disposition. A gated claim
      -- matches no row, so the run stays queued (the health resolver surfaces a distinct
      -- custody-limit reason against this SAME predicate in a later milestone). This is an
      -- OWNER-scoped admission gate (D4: the instance byte ceiling must never become a
      -- global stop-claiming switch), placed alongside the existing eligibility checks. A
      -- non-positive @custody_hold_limit DISABLES the gate (limit "<= 0" means unlimited),
      -- so it never blocks an ordinary claim; production passes the custody_hold_limit
      -- admin setting (1..1000), except the cross_check claim lane, which passes an inert 0
      -- because every row that lane can select is exempt (kind = 'cross_check').
      --
      -- Issue #1751 / ADR-1751: CONTINUATION EXEMPTION. A requeued run that was already
      -- claimed before (claim_generation >= 1) AND still holds its OWN open custody hold
      -- (same owner, same run_id) is exempt from the custody-count admission check: it is
      -- continuing work the owner already has in custody, not new work. The continuation may
      -- create another generation hold (the hold CTE below opens one per claim), but is exempt
      -- from custody-count admission. A fresh run (generation 0), or one whose own holds are
      -- all released/discarded, still faces the cap. The #1318 overshoot under concurrent
      -- claims still stands: this is an admission gate, not a strict ceiling.
      --
      -- The exemption is BOUNDED PER RUN: it holds only while the run's OWN open-hold count
      -- is below @custody_hold_limit (and at least one). Without the bound a claimed run that never
      -- reports running is swept back to queued by SweepClaimedNeverStarted (no requeue_count
      -- bump, no hold release), reclaims under the exemption, opens another generation hold,
      -- and loops forever; the owner cap used to stop that loop. Issue #3859 charges a
      -- WITNESSED departure of such a continuation (SweepClaimedNeverStartedCohort), so this
      -- bound remains the backstop for unwitnessed ones (e.g. older agents). Once a single run holds
      -- @custody_hold_limit open holds of its own, it faces the owner cap like new work. The
      -- same exemption expression is mirrored by GetCustodyAdmissionForRun (health reason) and
      -- GetCustodyAggregateForOwner.blocked_runs, so the pill, the aggregate and the claim
      -- agree.
      -- Report-only checkers open no custody hold; their exemption is independent
      -- of the bounded continuation exemption for code-publishing runs above.
      AND ($14::int <= 0
           OR r.kind = 'cross_check'
           OR (r.claim_generation >= 1
               AND EXISTS (SELECT 1 FROM recovery_custody_holds oh
                             WHERE oh.user_id = r.user_id AND oh.run_id = r.id AND oh.state = 'open')
               AND (SELECT count(*) FROM recovery_custody_holds oh2
                      WHERE oh2.user_id = r.user_id AND oh2.run_id = r.id AND oh2.state = 'open') < $14::int)
           OR (SELECT fn_custody_admission_count($3::uuid, $6::timestamptz)) < $14::int)
      -- PRD #3522 M3: the weekly usage reserve admission gate. @reserve_on is true only when the run
      -- owner has a threshold set (users.weekly_reserve_pct); every other @reserve_* param is then
      -- precomputed in Go (workersvc/usage_reserve.go, reserveSnapshot) from the owner's fresh
      -- readings. A held run matches no row and stays queued. The exemption arms mirror Go's
      -- reserveEnforced (chat is already excluded above): an active manual-resume override
      -- (@reserve_override_ended lists run ids whose override ended early; empty until the reset
      -- observation lands) and a previously started run while the pause toggle is off
      -- (@reserve_pause_on). The credential arm mirrors Go's reserveHeld, in claimSecretID order:
      --   Codex run        -> its frozen alias is blocked (api_key aliases are never in the list);
      --   judge/self_improve -> the owner's judge-lane binding;
      --   per-run override -> pinned id / default / auto pool (a pinned override with a NULL id
      --                       falls through to the worker lane);
      --   otherwise        -> the CLAIMING worker's lane (@reserve_worker_mode, static verdict
      --                       @reserve_worker_static_blocked for pinned/default).
      -- The auto arms hold only when something pooled WAS spendable but every such token is
      -- blocked: an empty pool (or only the run's active dead-credential exclusion) keeps its
      -- existing pool_wait behaviour. A pooled id is skippable-by-exclusion when the run's
      -- limit_dead_secret_id names it while retry_not_before is still in the future (claimExcludeFor).
      -- A reserve-marked queued run sorts after the owner's unmarked ones in the ORDER BY below.
      AND (NOT $15::boolean
           OR (COALESCE(r.reserve_exempt_until > $16::timestamptz, false) AND NOT (r.id = ANY($17::uuid[])))
           OR (r.first_started_at IS NOT NULL AND NOT $18::boolean)
           OR NOT (
               CASE
                   WHEN r.harness = 'codex'
                       THEN COALESCE(r.codex_secret_id = ANY($19::uuid[]), false)
                   WHEN r.kind IN ('judge', 'self_improve')
                       THEN CASE $20::text
                           WHEN 'auto' THEN
                               CASE
                                   WHEN EXISTS (SELECT 1 FROM unnest($21::uuid[]) pp
                                                WHERE NOT COALESCE(r.retry_not_before > $16::timestamptz AND pp = r.limit_dead_secret_id, false))
                                       THEN false
                                   WHEN EXISTS (SELECT 1 FROM unnest($22::uuid[]) pp
                                                WHERE NOT COALESCE(r.retry_not_before > $16::timestamptz AND pp = r.limit_dead_secret_id, false))
                                       THEN true
                                   ELSE $23::boolean
                               END
                           ELSE $24::boolean
                       END
                   WHEN r.credential_override_mode = 'pinned' AND r.credential_override_secret_id IS NOT NULL
                       THEN COALESCE(r.credential_override_secret_id = ANY($25::uuid[]), false)
                   WHEN r.credential_override_mode = 'default'
                       THEN $23::boolean
                   WHEN r.credential_override_mode = 'auto' OR $26::text = 'auto'
                       THEN (EXISTS (SELECT 1 FROM unnest($22::uuid[]) pp
                                     WHERE NOT COALESCE(r.retry_not_before > $16::timestamptz AND pp = r.limit_dead_secret_id, false))
                             AND NOT EXISTS (SELECT 1 FROM unnest($21::uuid[]) pp
                                             WHERE NOT COALESCE(r.retry_not_before > $16::timestamptz AND pp = r.limit_dead_secret_id, false)))
                   ELSE $27::boolean
               END
           ))
      -- PRD #1226 M1 (D2): the NON-BYPASSABLE completion-protocol claim clause. An
      -- INTERLOCKED run (completion_contract_version IS NOT NULL) may be claimed ONLY by a
      -- worker whose SELF-REPORTED protocol_capabilities contain 'completion_interlock_v1';
      -- a LEGACY run (version IS NULL) is unaffected and claimable by any worker. This clause
      -- is a DEDICATED, standalone predicate INTENTIONALLY OUTSIDE fn_worker_can_claim,
      -- required_capabilities, ClearRunRequiredCapabilities and the @capability_aware
      -- kill-switch: an owner capability override or the kill-switch can neutralize the
      -- ordinary required-capability match, but NONE of them may authorize a worker that does
      -- not implement the completion protocol. @worker_protocol_caps is the claiming worker's
      -- stored workers.protocol_capabilities column, passed by the Go caller.
      -- Reconciled runs require generation-stamped checkpoint, input and memory requests.
      -- credential_switch_v1 predates those fields and is insufficient. This fence applies
      -- even when capability scheduling is disabled or the owner clears required_capabilities.
      AND (NOT EXISTS (SELECT 1 FROM run_milestone_reconciliations audit WHERE audit.run_id = r.id)
           OR 'milestone_reconciliation_claim_v1' = ANY($28::text[]))
      AND (r.checkpoint_coordinates IS NULL OR 'task_checkpoint_coordinates_v1' = ANY($28::text[]))
      AND (NOT EXISTS (SELECT 1 FROM run_publication_resume pr WHERE pr.run_id = r.id AND pr.state = 'active') OR 'publication_resume_v1' = ANY($28::text[]))
      -- Issue #1324 D5: an enrolled run with no handoff is claimable only by its custodian, the side row's worker_id (not runs.worker_id, which resume writers clear); a NULL side worker_id is held. No affinity-ceiling release.
      AND NOT EXISTS (SELECT 1 FROM run_publication_resume pc WHERE pc.run_id = r.id AND pc.state = 'active' AND pc.handoff IS NULL AND pc.worker_id IS DISTINCT FROM $1)
      -- A claim outside a transaction cannot set the publication-resume opt-in the trigger requires.
      AND (NOT $29::boolean OR NOT EXISTS (SELECT 1 FROM run_publication_resume pr WHERE pr.run_id = r.id AND pr.state = 'active'))
      AND (r.completion_contract_version IS NULL
           OR 'completion_interlock_v1' = ANY($28::text[]))
      -- PRD #2865: the NON-BYPASSABLE CI-config-guard claim clause (standalone, outside
      -- fn_worker_can_claim, required_capabilities and @capability_aware). A GUARDED run
      -- (ci_config_guard) is claimable only by a worker reporting 'ci_config_guard_v1'.
      AND (NOT r.ci_config_guard OR 'ci_config_guard_v1' = ANY($28::text[]))
      AND ((NOT r.repo_agent_source_required AND r.repo_agent_source_commit IS NULL) OR 'repo_agent_source_v1' = ANY($28::text[]))
      AND (r.agent_source_requested IS NULL OR 'agent_source_request_v1' = ANY($28::text[]))
      AND (NOT (r.policy_preserve_required OR r.policy_pending_fresh OR (r.policy_outcome_id IS NOT NULL AND r.policy_resolved_at IS NULL) OR r.policy_required_head IS NOT NULL) OR 'provider_policy_hold_v1' = ANY($28::text[]))
      AND (NOT (r.kind = 'prompt' AND r.ci_config_guard AND EXISTS (SELECT 1 FROM job_files af WHERE af.run_id = r.id AND af.direction = 'input' AND af.state = 'attached')) OR 'ci_artifacts_v1' = ANY($28::text[]))
      -- PRD #1332 M5A (D3): the NON-BYPASSABLE Codex-harness claim clause, MIRRORING the
      -- completion-protocol clause directly above. A CODEX-INDICATING run may be claimed ONLY by a
      -- worker whose SELF-REPORTED protocol_capabilities contain 'codex_harness_v1' (advertised only
      -- after a successful runtime-receipt probe); a NON-Codex run is unaffected. "Codex-indicating"
      -- checks ALL THREE binding facts — harness='codex' OR codex_material_revision IS NOT NULL OR
      -- codex_secret_id IS NOT NULL — so the gate FAILS CLOSED on inconsistent or legacy data: even
      -- if C1's coherence CHECK normally implies harness='codex', a row whose harness were somehow
      -- unset while a binding sentinel survives (e.g. the alias FK nulls codex_secret_id but leaves
      -- codex_material_revision) is still gated, never silently shipping its Codex credential block to
      -- an old worker that ignores the unknown JSON and runs Claude. Like the completion clause this is
      -- a DEDICATED, standalone predicate INTENTIONALLY OUTSIDE fn_worker_can_claim,
      -- required_capabilities, ClearRunRequiredCapabilities and the @capability_aware kill-switch:
      -- none of those may authorize a worker that cannot run Codex. Reuses the SAME
      -- @worker_protocol_caps param the completion clause reads (the claimant's stored
      -- workers.protocol_capabilities, passed by the Go caller).
      AND (NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
           OR ('codex_harness_v1' = ANY($28::text[]) AND 'codex_runtime_v2' = ANY($28::text[])))
      -- Interlocked Codex turns require the newer completion loop, independently of overrides.
      AND (r.completion_contract_version IS NULL
           OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
           OR 'codex_completion_interlock_v1' = ANY($28::text[]))
      AND (NOT (r.plan_cross_check_required OR (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan')))
           OR 'cross_check_v1' = ANY($28::text[]))
      AND (NOT (r.kind = 'cross_check' AND r.harness = 'claude' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
           OR 'cross_check_codex_lead_v1' = ANY($28::text[]))
      AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM semantic_planning sp WHERE sp.run_id = r.target_run_id) AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
           OR 'cross_check_semantic_plan_v1' = ANY($28::text[]))
      AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM cross_checks pc JOIN cross_check_owner_steering s ON s.cross_check_id = pc.id WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
           OR 'cross_check_owner_steering_v1' = ANY($28::text[]))
      AND (r.kind = 'cross_check' OR NOT EXISTS (SELECT 1 FROM semantic_planning sp WHERE sp.run_id = r.id)
           OR 'completion_semantic_publication_v1' = ANY($28::text[]))
      AND (r.kind <> 'cross_check' OR EXISTS (
          SELECT 1 FROM cross_checks protocol_check JOIN runs protocol_lead ON protocol_lead.id = protocol_check.lead_run_id
          WHERE protocol_check.checker_run_id = r.id
            AND protocol_check.lead_run_id = r.target_run_id AND protocol_lead.user_id = r.user_id
            AND protocol_lead.kind <> 'cross_check' AND protocol_check.stage IN ('plan','code')
            AND protocol_check.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = protocol_check.lead_run_id AND latest.stage = protocol_check.stage)
            AND ((protocol_check.stage = 'plan' AND protocol_check.verdict = 'pending') OR (protocol_check.stage = 'code' AND protocol_check.outcome = 'pending' AND protocol_check.interrupted_at IS NULL AND 'cross_check_code_v1' = ANY($28::text[]) AND 'cross_check_lane_v1' = ANY($28::text[]))) AND protocol_check.deadline_at > now()
            AND (protocol_lead.status IN ('claimed','running') OR (protocol_check.stage = 'code' AND protocol_lead.status = 'awaiting_followup' AND protocol_lead.kind = 'task' AND protocol_lead.interactive)) AND protocol_lead.claim_released_at IS NULL
            AND protocol_lead.claim_generation = protocol_check.lead_claim_generation
            AND (protocol_check.round = 1 OR (protocol_check.automatic_rounds_enabled
                AND protocol_check.round <= protocol_check.automatic_revision_limit + 1
                AND 'cross_check_rounds_v1' = ANY($28::text[])))
      ))
      AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = r.id WHERE pin.user_id = r.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness AND (pin.model IS NOT NULL OR pin.effort IS NOT NULL))) OR 'cross_check_pins_v1' = ANY($28::text[]))
      -- PRD #1551 M4 (D6): the NON-BYPASSABLE custom-Codex-model claim clause, a SIBLING of the
      -- codex-harness clause directly above. A Codex run whose EFFECTIVE worker-root model is a
      -- CUSTOM (non-curated) id may be claimed ONLY by a worker whose protocol_capabilities contain
      -- 'codex_custom_model_v1'. Cross-check roots follow ResolveCrossCheck: checker model pin,
      -- owner default_codex_model, then the first delivered lead template's valid Codex role pin.
      -- A null/invalid template implies the curated fallback, so NULL is not custom here. Never
      -- skip a first lead whose pin is NULL/invalid in favor of a later lead. Ordinary runs keep
      -- D4 precedence: a frozen curated r.model wins, otherwise the owner's Codex default lane.
      -- Task-review (review_target_run_id IS NOT NULL), judge and
      -- chat are EXEMPT — assembly reads no Codex lane for them — so the exemption is keyed on
      -- review_target_run_id/kind, NEVER on all of kind='task'. NULL-safe via COALESCE: a NULL
      -- r.model AND NULL lane make the effective root NULL, so the inner NOT(... = ANY ...) is NULL
      -- and COALESCE(...,false) is false ⇒ not custom ⇒ no new requirement. Same standalone posture
      -- as the harness clause (OUTSIDE fn_worker_can_claim, required_capabilities and the
      -- capability_aware kill-switch); the effective-root expression is written identically here,
      -- in the peer mirror below, and in CountOnlineWorkersClaimableForRun.
      AND (
          NOT (
              r.harness = 'codex'
              AND r.kind NOT IN ('judge', 'chat')
              AND r.review_target_run_id IS NULL
              AND COALESCE(
                  NOT ((CASE WHEN r.kind = 'cross_check' THEN COALESCE((SELECT pin.model FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = r.id WHERE pin.user_id = r.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness), (SELECT u.default_codex_model FROM users u WHERE u.id = r.user_id),
                      (SELECT template.codex_model FROM checker_template_pins template WHERE template.user_id = r.user_id)) WHEN r.model = ANY($30::text[]) THEN r.model
                             ELSE (SELECT u.default_codex_model FROM users u WHERE u.id = r.user_id) END)
                       = ANY($30::text[])),
                  false)
          )
          OR 'codex_custom_model_v1' = ANY($28::text[])
      )
      -- PRD #1908 (D-A): the NON-BYPASSABLE job-runner claim clause, a standalone sibling of the
      -- Codex clauses above. A repo-less 'job' run may be claimed ONLY by a NON-docker worker whose
      -- SELF-REPORTED protocol_capabilities contain 'job_runner_v1'. Every other kind is unaffected.
      -- OUTSIDE fn_worker_can_claim, required_capabilities, ClearRunRequiredCapabilities and the
      -- @capability_aware kill-switch: an old-image worker would route a job to the issue executor,
      -- and a docker worker never serves a repo-less non-judge run. @is_docker_worker is the
      -- claimant's docker flag, @worker_protocol_caps its stored protocol_capabilities.
      -- PRD #1909 M1 (the job_files_v1 ROLLOUT GATE): a job stamped with runs.job_protocol (every job
      -- created since the file protocol landed) additionally needs 'job_files_v1'; a pre-change job
      -- (job_protocol NULL) stays claimable by any job_runner_v1 worker. Same OUTSIDE-the-bypasses rule.
      AND (r.kind <> 'job'
           OR (NOT $10::boolean AND 'job_runner_v1' = ANY($28::text[])
               AND (r.job_protocol IS NULL OR 'job_files_v1' = ANY($28::text[]))
               -- PRD #1976 M1 (the isolated_job_v1 ROLLOUT GATE): a profile-bound job (runs.egress_profile_id)
               -- additionally needs 'isolated_job_v1', so a lane worker released before profile-bound jobs
               -- (isolated_fetch_v1 + job_runner_v1 only) never claims one. Same OUTSIDE-the-bypasses rule;
               -- the lane clause below (isolated_lane + isolated_fetch_v1) is unchanged.
               AND (r.egress_profile_id IS NULL OR 'isolated_job_v1' = ANY($28::text[]))))
      -- PRD #1590 M2 (D2, amendment A1): keep a Codex subscription run queued while
      -- its SAME alias's account authority is on hold (D1's hold class):
      --   (1) quarantine: the linked account is quarantined and is still the run's
      --       frozen identity at its frozen credential_revision. A run with no frozen
      --       identity (codex_account_key NULL: its alias was unlinked at create, and
      --       nothing freezes it later) fails evalCodexReleasePredicate with
      --       ErrCodexAccountKeyUnfrozen whatever the account does, so it is not held
      --       (D1 holds only what can resume); like a different identity or a bumped
      --       revision, the claim proceeds and fails in assembly as before;
      --   (2) re-login in flight: past first link, a newer login on the alias is
      --       staging/failed (the PATCH cleared the link, so (1) cannot see it);
      --   (3) A1: past first link, that newer login already linked back to the
      --       frozen identity at the frozen credential_revision (D5 re-admits it;
      --       until then the run's material_revision is stale).
      -- This is the SQL twin of classifyCodexClaimAuthority; a shared fixture table
      -- pins the two against each other. The frozen identity is stored only as the
      -- Go-encoded JSON array codex_account_key (00202, codexAccountKey), and no SQL
      -- encoder exists, so it is DECODED with a guarded ::jsonb cast and compared
      -- structurally to the account's tuple; an undecodable key never matches. The
      -- gate is run-level, independent of worker capabilities, and precedes the
      -- custody-opening hold CTE. The LEFT JOIN preserves an unlinked alias
      -- (including a first login); every lookup stays scoped to this run's owner.
      -- The SAME predicate text appears in the peer mirror below, in
      -- ParkQueuedCodexAccountUnavailablePage and in ListActiveRunsForHealth's
      -- codex_account_gated; a store test pins the copies byte-identical after
      -- whitespace normalisation.
      AND NOT (
          r.harness = 'codex'
          AND r.codex_auth_mode = 'subscription'
          AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework', 'cross_check')
          AND EXISTS (
              SELECT 1 FROM codex_credential_state ccs
              LEFT JOIN codex_provider_account cpa
                  ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
              WHERE ccs.user_secret_id = r.codex_secret_id
                AND ccs.user_id = r.user_id
                AND (
                    (cpa.coord_state = 'quarantined'
                     AND cpa.credential_revision = r.codex_account_revision
                     AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                              THEN r.codex_account_key::jsonb
                                   = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                              ELSE false END)
                    OR (r.codex_account_key IS NOT NULL
                        AND ccs.material_revision > r.codex_material_revision
                        AND (ccs.status IN ('staging', 'failed')
                             OR (ccs.status = 'linked'
                                 AND cpa.credential_revision = r.codex_account_revision
                                 AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                          THEN r.codex_account_key::jsonb
                                               = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                          ELSE false END)))
                )
          )
      )
      -- PRD #1906 M5 (Decision 9): the NON-BYPASSABLE isolated-lane claim clause, TWO-WAY. A
      -- profile-bound run (egress_profile_id IS NOT NULL) may be claimed ONLY by a lane worker
      -- (@worker_isolated_lane: workers.isolated_lane, set by the api at provisioning and never
      -- from a worker report), and a lane worker claims ONLY profile-bound runs. The lane worker
      -- must ALSO advertise 'isolated_fetch_v1' (it runs the isolated research runner), and a
      -- Codex-indicating run is never claimable as a profile-bound run (Decision 10; its web search
      -- is server-side), checked on all three Codex facts like the harness clause above. Like the
      -- completion and Codex clauses it is a DEDICATED, standalone predicate INTENTIONALLY OUTSIDE
      -- fn_worker_can_claim, required_capabilities, ClearRunRequiredCapabilities and the
      -- @capability_aware kill-switch: none of them can put a profile-bound run on a worker outside
      -- the lane. The same three lines are mirrored for the spread peer below, in
      -- CountOnlineWorkersClaimableForRun and (as the lane half) in the CountOnlineWorkersSatisfying*
      -- rungs.
      AND ((r.egress_profile_id IS NOT NULL) = $31::boolean)
      AND (r.egress_profile_id IS NULL OR 'isolated_fetch_v1' = ANY($28::text[]))
      AND (r.egress_profile_id IS NULL OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL))
      -- PRD #529 Decision 4: an ephemeral worker exists to serve exactly one run and
      -- must never take foreign work — otherwise it could hold a non-owning run when
      -- its bound run terminates, blocking the busy-guarded teardown (M4). So an
      -- ephemeral claimant (@is_ephemeral) matches ONLY its bound run
      -- (@ephemeral_run_id); a non-ephemeral worker short-circuits true and the
      -- (NULL) run id is never compared.
      --
      -- PRD #2006: the LEASE arm. A leased ephemeral worker (it finished its bound run and kept
      -- its pod for a bounded lease) may ALSO claim a queued run of the same owner (the target's
      -- own r.user_id = @user_id), the same repository and the same effective branch identity as
      -- its last bound run: fn_ephemeral_lease_admits decides, on the claimant's OWN lease columns
      -- (read under its row lock by the caller) at @lease_at, a clock reading the caller takes
      -- AFTER every lock and guard it waits on (a stale transaction-start now() could admit
      -- through a lease that expired while the claim waited). The arm is closed when another
      -- ephemeral worker is already bound to r (one worker per bound run, uq_workers_ephemeral_run)
      -- or when the claimant still holds an open custody hold. A NULL @ephemeral_lease or an
      -- absent lease admits nothing, so lease 0 / a non-leased worker is exactly the bound-run-only
      -- clause above. The caller rebinds the worker to the claimed run in the same transaction.
      AND (NOT $32::boolean
           OR (r.kind = 'cross_check' AND r.target_run_id = $33::uuid)
           OR r.id = $33::uuid
           OR ((fn_ephemeral_lease_admits(
                   $34::timestamptz,
                   $35::uuid,
                   $36::text,
                   $9::boolean,
                   $37::interval,
                   $38::timestamptz,
                   r.repo_id, r.kind, r.branch, r.pipeline_ref, r.issue_iid, r.failure_snapshot, r.egress_profile_id)
                   AND (NOT fn_ephemeral_docker_preference_applies((SELECT owner.ephemeral_docker_enabled FROM users owner WHERE owner.id = r.user_id), $39::boolean, r.repo_id, r.kind, r.egress_profile_id, $11::uuid[]) OR COALESCE($10::boolean, false)))
               AND NOT EXISTS (
                   SELECT 1 FROM workers bw
                   WHERE bw.ephemeral AND bw.ephemeral_run_id = r.id)
               AND NOT EXISTS (
                   SELECT 1 FROM recovery_custody_holds ch
                   WHERE ch.live_worker_id = $1 AND ch.state = 'open')))
      -- PRD #216 fleet-aware spread (D3/D4/D7/D8/R3). Defer this run to a peer
      -- ONLY when a strictly-better peer exists. Resume affinity (worker_id = me)
      -- and a run older than @spread_cutoff both BYPASS the spread, so the spread
      -- can never make a run unclaimable (D7 fail-open). NOT EXISTS is two-valued
      -- by construction (no COALESCE to forget). A peer qualifies as strictly
      -- better iff it is live (D6 heartbeat), eligible via the SAME expression
      -- (D5), has an advertised cap (D8: NULL cap is not a deferral target), has a
      -- free slot (D8), and is strictly less loaded by integer cross-multiplication
      -- (R3: peer.active * my.cap < my.active * peer.cap — exact, no float ties).
      -- The peer/my active counts use the SAME definition as the UI's active_runs
      -- (:93-98) so a placement is never contradicted by the displayed load. My cap
      -- and my active count are read from the same snapshot (not racy params); a
      -- NULL my.cap makes the product NULL -> row excluded -> I claim (fail-open);
      -- a 0 active count on me makes the RHS 0 -> no peer qualifies -> I always
      -- claim (a minimum-loaded worker never defers, guaranteeing claimability).
      -- Dedicated cross-check claims use affinity instead of spread (PRD #2169);
      -- legacy run-lane children retain the ordinary spread rules.
      AND (
          (r.kind = 'cross_check' AND COALESCE($2::text, 'run') = 'cross_check')
          OR r.worker_id = $1
          OR r.updated_at < $40
          OR NOT EXISTS (
              SELECT 1
              FROM workers p
              CROSS JOIN LATERAL (
                  SELECT count(*) AS active
                  FROM runs pr
                  WHERE pr.worker_id = p.id
                    AND pr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
                    AND pr.kind <> 'chat' AND NOT pr.cross_check_lane
              ) pa
              WHERE p.user_id = $3
                AND p.id <> $1
                AND p.last_heartbeat_at IS NOT NULL
                AND p.last_heartbeat_at >= $6
                -- A draining peer claims nothing (PRD #422 Decision 7), so never DEFER a
                -- run to it — it would never pick the run up.
                AND fn_worker_accepts_new_intake(p)
                AND fn_forge_origin_eligible(p, r, now())
                -- PRD #529 Decision 4: an ephemeral peer claims ONLY its own bound run
                -- (ClaimRun's claimant clause above), so it would never pick up a
                -- FOREIGN run — same failure mode as the draining-peer guard. Deferring
                -- a foreign run to it would make the run unclaimable. But when r IS the
                -- ephemeral peer's own bound run, that peer is a valid deferral target,
                -- so a busy claimant correctly defers to it — hence the full predicate,
                -- not a bare AND NOT p.ephemeral.
                -- PRD #2006: ...or a LEASED ephemeral peer that may claim r through its lease
                -- (the claimant clause's lease arm), advisory like this whole mirror, so now().
                AND (NOT p.ephemeral OR p.ephemeral_run_id = r.id
                     OR (fn_ephemeral_lease_admits(
                            p.lease_since, p.lease_repo_id, p.lease_branch, NOT fn_worker_accepts_new_intake(p),
                            $37::interval, now(),
                            r.repo_id, r.kind, r.branch, r.pipeline_ref, r.issue_iid, r.failure_snapshot, r.egress_profile_id)
                   AND (NOT fn_ephemeral_docker_preference_applies((SELECT owner.ephemeral_docker_enabled FROM users owner WHERE owner.id = r.user_id), $39::boolean, r.repo_id, r.kind, r.egress_profile_id, $11::uuid[]) OR COALESCE(p.docker_enabled, false))))
                AND p.max_concurrent_runs IS NOT NULL
                -- A legacy child can defer only to a peer eligible on the run lane;
                -- explicit zero or dedicated cross-check capacity cannot use fallback.
                -- Keep the protocol, pins and custom-model mirrors below as well.
                AND (r.kind <> 'cross_check' OR fn_cross_check_child_eligible(p, r, true,
                     'run', (SELECT stage FROM cross_checks WHERE checker_run_id = r.id), $4::timestamptz, $5::timestamptz))
                AND fn_worker_can_claim(COALESCE(p.docker_enabled, false), $11::uuid[], r.repo_id, r.kind, p.capabilities, r.required_capabilities, $13::boolean)
                -- PRD #1226 M1 (D2): MIRROR the non-bypassable completion-protocol clause for
                -- the peer, or fleet-spread could DEFER an interlocked run to an INCAPABLE peer
                -- that would never be able to claim it — making the run unclaimable. Reads the
                -- peer's OWN workers.protocol_capabilities column directly (no Go param, unlike
                -- the claimant's @worker_protocol_caps above).
                AND (NOT EXISTS (SELECT 1 FROM run_milestone_reconciliations audit WHERE audit.run_id = r.id)
                     OR 'milestone_reconciliation_claim_v1' = ANY(p.protocol_capabilities))
      AND (r.checkpoint_coordinates IS NULL OR 'task_checkpoint_coordinates_v1' = ANY(p.protocol_capabilities))
      AND (NOT EXISTS (SELECT 1 FROM run_publication_resume pr WHERE pr.run_id = r.id AND pr.state = 'active') OR 'publication_resume_v1' = ANY(p.protocol_capabilities))
      -- Issue #1324 D5: an enrolled run with no handoff is claimable only by its custodian, the side row's worker_id (not runs.worker_id, which resume writers clear); a NULL side worker_id is held. No affinity-ceiling release.
      AND NOT EXISTS (SELECT 1 FROM run_publication_resume pc WHERE pc.run_id = r.id AND pc.state = 'active' AND pc.handoff IS NULL AND pc.worker_id IS DISTINCT FROM p.id)
                AND (r.completion_contract_version IS NULL
                     OR 'completion_interlock_v1' = ANY(p.protocol_capabilities))
                -- PRD #2865: the NON-BYPASSABLE CI-config-guard claim clause (standalone, outside
                -- fn_worker_can_claim, required_capabilities and @capability_aware). A GUARDED run
                -- (ci_config_guard) is claimable only by a worker reporting 'ci_config_guard_v1'.
                AND (NOT r.ci_config_guard OR 'ci_config_guard_v1' = ANY(p.protocol_capabilities))
                AND ((NOT r.repo_agent_source_required AND r.repo_agent_source_commit IS NULL) OR 'repo_agent_source_v1' = ANY(p.protocol_capabilities))
                AND (r.agent_source_requested IS NULL OR 'agent_source_request_v1' = ANY(p.protocol_capabilities))
      AND (NOT (r.policy_preserve_required OR r.policy_pending_fresh OR (r.policy_outcome_id IS NOT NULL AND r.policy_resolved_at IS NULL) OR r.policy_required_head IS NOT NULL) OR 'provider_policy_hold_v1' = ANY(p.protocol_capabilities))
                AND (NOT (r.kind = 'prompt' AND r.ci_config_guard AND EXISTS (SELECT 1 FROM job_files af WHERE af.run_id = r.id AND af.direction = 'input' AND af.state = 'attached')) OR 'ci_artifacts_v1' = ANY(p.protocol_capabilities))
                -- PRD #1332 M5A (D3): MIRROR the non-bypassable Codex-harness clause for the peer, or
                -- fleet-spread could DEFER a CODEX-INDICATING run to an INCAPABLE peer that could never
                -- claim it (its OWN Codex claim clause above blocks it) — making the run permanently
                -- unclaimable by being preferred. Same all-three "Codex-indicating" test on r, reading
                -- the peer's OWN workers.protocol_capabilities column directly (no Go param, unlike the
                -- claimant's @worker_protocol_caps).
                AND (NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                     OR ('codex_harness_v1' = ANY(p.protocol_capabilities) AND 'codex_runtime_v2' = ANY(p.protocol_capabilities)))
                AND (r.completion_contract_version IS NULL
                     OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                     OR 'codex_completion_interlock_v1' = ANY(p.protocol_capabilities))
      AND (NOT (r.plan_cross_check_required OR (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan')))
           OR 'cross_check_v1' = ANY(p.protocol_capabilities))
      AND (NOT (r.kind = 'cross_check' AND r.harness = 'claude' AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
           OR 'cross_check_codex_lead_v1' = ANY(p.protocol_capabilities))
      AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM semantic_planning sp WHERE sp.run_id = r.target_run_id) AND EXISTS (SELECT 1 FROM cross_checks pc WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
           OR 'cross_check_semantic_plan_v1' = ANY(p.protocol_capabilities))
      AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM cross_checks pc JOIN cross_check_owner_steering s ON s.cross_check_id = pc.id WHERE pc.checker_run_id = r.id AND pc.stage = 'plan'))
           OR 'cross_check_owner_steering_v1' = ANY(p.protocol_capabilities))
      AND (r.kind = 'cross_check' OR NOT EXISTS (SELECT 1 FROM semantic_planning sp WHERE sp.run_id = r.id)
           OR 'completion_semantic_publication_v1' = ANY(p.protocol_capabilities))
      AND (r.kind <> 'cross_check' OR EXISTS (
          SELECT 1 FROM cross_checks protocol_check JOIN runs protocol_lead ON protocol_lead.id = protocol_check.lead_run_id
          WHERE protocol_check.checker_run_id = r.id
            AND protocol_check.lead_run_id = r.target_run_id AND protocol_lead.user_id = r.user_id
            AND protocol_lead.kind <> 'cross_check' AND protocol_check.stage IN ('plan','code')
            AND protocol_check.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = protocol_check.lead_run_id AND latest.stage = protocol_check.stage)
            AND ((protocol_check.stage = 'plan' AND protocol_check.verdict = 'pending') OR (protocol_check.stage = 'code' AND protocol_check.outcome = 'pending' AND protocol_check.interrupted_at IS NULL AND 'cross_check_code_v1' = ANY(p.protocol_capabilities) AND 'cross_check_lane_v1' = ANY(p.protocol_capabilities))) AND protocol_check.deadline_at > now()
            AND (protocol_lead.status IN ('claimed','running') OR (protocol_check.stage = 'code' AND protocol_lead.status = 'awaiting_followup' AND protocol_lead.kind = 'task' AND protocol_lead.interactive)) AND protocol_lead.claim_released_at IS NULL
            AND protocol_lead.claim_generation = protocol_check.lead_claim_generation
            AND (protocol_check.round = 1 OR (protocol_check.automatic_rounds_enabled
                AND protocol_check.round <= protocol_check.automatic_revision_limit + 1
                AND 'cross_check_rounds_v1' = ANY(p.protocol_capabilities)))
      ))
      AND (NOT (r.kind = 'cross_check' AND EXISTS (SELECT 1 FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = r.id WHERE pin.user_id = r.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness AND (pin.model IS NOT NULL OR pin.effort IS NOT NULL))) OR 'cross_check_pins_v1' = ANY(p.protocol_capabilities))
                -- PRD #1551 M4 (D6): MIRROR the non-bypassable custom-Codex-model clause for the peer, or
                -- fleet-spread could DEFER a CUSTOM-root Codex run to an INCAPABLE peer that could never
                -- claim it (its OWN custom-model clause above blocks it) — making the run permanently
                -- unclaimable by being preferred. The effective-root expression is written IDENTICALLY to
                -- the claimant clause, reading the peer's OWN workers.protocol_capabilities (no Go param).
                AND (
                    NOT (
                        r.harness = 'codex'
                        AND r.kind NOT IN ('judge', 'chat')
                        AND r.review_target_run_id IS NULL
                        AND COALESCE(
                            NOT ((CASE WHEN r.kind = 'cross_check' THEN COALESCE((SELECT pin.model FROM user_cross_check_pins pin JOIN cross_checks cc ON cc.checker_run_id = r.id WHERE pin.user_id = r.user_id AND pin.stage = cc.stage AND pin.harness = cc.checker_harness), (SELECT u.default_codex_model FROM users u WHERE u.id = r.user_id),
                                (SELECT template.codex_model FROM checker_template_pins template WHERE template.user_id = r.user_id)) WHEN r.model = ANY($30::text[]) THEN r.model
                                       ELSE (SELECT u.default_codex_model FROM users u WHERE u.id = r.user_id) END)
                                 = ANY($30::text[])),
                            false)
                    )
                    OR 'codex_custom_model_v1' = ANY(p.protocol_capabilities)
                )
                -- PRD #1908 (D-A): MIRROR the job-runner claim clause for the peer, or fleet-spread could
                -- DEFER a job to a docker or old-image peer that could never claim it. Reads the peer's
                -- OWN docker flag and workers.protocol_capabilities.
                -- PRD #1909 M1: and the job_files_v1 arm for a stamped job (runs.job_protocol).
                AND (r.kind <> 'job'
                     OR (NOT COALESCE(p.docker_enabled, false) AND 'job_runner_v1' = ANY(p.protocol_capabilities)
                         AND (r.job_protocol IS NULL OR 'job_files_v1' = ANY(p.protocol_capabilities))
                         -- PRD #1976 M1: and the isolated_job_v1 arm for a profile-bound job.
                         AND (r.egress_profile_id IS NULL OR 'isolated_job_v1' = ANY(p.protocol_capabilities))))
                AND COALESCE(codex_admission_run_eligible_at(r.id) <= now(), true)
                -- PRD #1590 M2 (D2, A1): mirror the claimant's account gate so a
                -- busy worker never defers this run to a peer that cannot claim it.
                AND NOT (
                    r.harness = 'codex'
                    AND r.codex_auth_mode = 'subscription'
                    AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework', 'cross_check')
                    AND EXISTS (
                        SELECT 1 FROM codex_credential_state ccs
                        LEFT JOIN codex_provider_account cpa
                            ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
                        WHERE ccs.user_secret_id = r.codex_secret_id
                          AND ccs.user_id = r.user_id
                          AND (
                              (cpa.coord_state = 'quarantined'
                               AND cpa.credential_revision = r.codex_account_revision
                               AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                        THEN r.codex_account_key::jsonb
                                             = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                        ELSE false END)
                              OR (r.codex_account_key IS NOT NULL
                                  AND ccs.material_revision > r.codex_material_revision
                                  AND (ccs.status IN ('staging', 'failed')
                                       OR (ccs.status = 'linked'
                                           AND cpa.credential_revision = r.codex_account_revision
                                           AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                                    THEN r.codex_account_key::jsonb
                                                         = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                                    ELSE false END)))
                          )
                    )
                )
                -- PRD #1906 M5: MIRROR the isolated-lane clause for the peer, or fleet-spread could
                -- DEFER a run to a peer on the wrong side of the lane that could never claim it.
                AND ((r.egress_profile_id IS NOT NULL) = p.isolated_lane)
                AND (r.egress_profile_id IS NULL OR 'isolated_fetch_v1' = ANY(p.protocol_capabilities))
                AND (r.egress_profile_id IS NULL OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL))
                -- PRD #3522 M3: never DEFER a worker-lane run to a peer whose own Anthropic lane the
                -- weekly reserve blocks (@reserve_blocked_worker_ids, the owner's workers whose bind
                -- mode resolves to a blocked token or an all-blocked auto pool): that peer's
                -- ClaimRun would hold the run, and deferring to it would leave the run unclaimed
                -- until @spread_cutoff. Only a run whose credential is chosen by the claiming
                -- WORKER is affected: Codex, judge/self_improve and per-run override runs resolve
                -- their credential independently of the peer, and an exempt run is not gated. The
                -- list is computed without the run's dead-credential exclusion; the spread is advisory
                -- and fails open at @spread_cutoff, so that approximation can delay a deferral but
                -- never strand a run.
                AND NOT (
                    $15::boolean
                    AND NOT (COALESCE(r.reserve_exempt_until > $16::timestamptz, false) AND NOT (r.id = ANY($17::uuid[])))
                    AND NOT (r.first_started_at IS NOT NULL AND NOT $18::boolean)
                    AND r.harness IS DISTINCT FROM 'codex'
                    AND r.kind NOT IN ('judge', 'self_improve')
                    AND NOT (r.credential_override_mode = 'pinned' AND r.credential_override_secret_id IS NOT NULL)
                    AND COALESCE(r.credential_override_mode, '') NOT IN ('default', 'auto')
                    AND p.id = ANY($41::uuid[])
                )
                AND pa.active < p.max_concurrent_runs
                AND pa.active * (SELECT w.max_concurrent_runs FROM workers w WHERE w.id = $1)
                    < (SELECT count(*) FROM runs mr
                        WHERE mr.worker_id = $1
                          AND mr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
                          AND mr.kind <> 'chat' AND NOT mr.cross_check_lane)
                      * p.max_concurrent_runs
          )
      )
      -- PRD #1390 M3 (D3, D8): global, pre-claim snapshot dedupe — three exclusions that run
      -- INSIDE candidate selection, BEFORE the generation increment and before the `hold` CTE
      -- opens custody, so a run a fresh snapshot lists is never returned to any claimant (including
      -- a sibling past the affinity ceiling) and no side effect fires for it.
      --
      -- (1) Fresh-snapshot / terminal-pending exclusion: never claim a run a FRESH snapshot lists
      -- as active at its CURRENT generation. Freshness is the snapshot's OWN reported_at (a worker
      -- whose snapshots fail validation must not keep stale rows protected by its liveness), within
      -- @snapshot_fresh_cutoff = now() - (WORKER_HEARTBEAT_STALE + WORKER_HEARTBEAT_INTERVAL). A
      -- terminal-pending lease (a.terminal_pending AND still unexpired) excludes regardless of
      -- freshness (#1391's journaled outcome outlives the heartbeat, D11). Worker-scoped
      -- (a.worker_id = r.worker_id) for consistency with the sweep predicates, and sound because a
      -- run's snapshot row is only ever its owner's.
      AND NOT EXISTS (
          SELECT 1 FROM worker_active_runs a
          WHERE a.run_id = r.id AND a.worker_id = r.worker_id
            AND a.claim_generation = r.claim_generation
            -- PRD #1497 M1 (D16): a RELEASED flight's fresh snapshot must NOT block the reclaim of a
            -- server-parked run — the release is exactly the signal the old flight is over.
            AND r.claim_released_at IS NULL
            AND (a.reported_at >= $42
                 OR (a.terminal_pending AND a.terminal_pending_until > now())))
      -- (2) Request-array exclusion (fact 7): the claimant's OWN request snapshot excludes its
      -- listed runs at the CURRENT generation, so the exclusion holds BEFORE the first heartbeat
      -- persists the rows exclusion (1) reads. @request_active_ids / @request_active_gens are
      -- index-aligned pairs; the two-array unnest is spelled as a WITH ORDINALITY zip because
      -- sqlc's analyzer cannot type a multi-argument unnest(a, b) (see judge_bulk_disposition.sql).
      -- Empty arrays (no request snapshot, or the no-snapshot path) match nothing → no exclusion.
      AND NOT EXISTS (
          SELECT 1
          FROM unnest($43::uuid[]) WITH ORDINALITY AS req_id(id, ord)
          JOIN unnest($44::bigint[]) WITH ORDINALITY AS req_gen(gen, ord)
               ON req_gen.ord = req_id.ord
          WHERE req_id.id = r.id AND req_gen.gen = r.claim_generation)
      -- (3) Overflow closure (D11): never claim a run whose OWNER is under an unexpired
      -- pending_overflow closure — an outcome the worker could not list has no row of its own to
      -- lease, so the worker-level closure stands in for the row-level lease. An unassigned run
      -- (r.worker_id IS NULL) has no owner row, so it is never closed here; the claimant-side half
      -- (a flagged worker refused every claim, unassigned included) is the service-level guard.
      AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = r.worker_id AND w.pending_overflow_until > now())
      -- PRD #1497 M1 (D19): a server-side wall park records the incarnation it released
      -- (released_worker_id + released_worker_nonce, captured under the park's lock). That EXACT
      -- incarnation cannot reclaim its own parked run — otherwise a dead/incapable/unresponsive
      -- worker that came back would win the run it could not park. A DIFFERENT worker, or the SAME
      -- worker RE-REGISTERED (registration rotates snapshot_register_nonce, so it is a new safe
      -- incarnation), is not excluded, which is what keeps a single-worker deployment live. Rows with
      -- no released pair (released_worker_id IS NULL) are always claimable — the leading IS NULL arm
      -- is REQUIRED because `NULL = @worker_id` is unknown, not false, so a bare NOT(...) would
      -- wrongly exclude every ordinary run for a claimant whose nonce is also NULL.
      AND (r.released_worker_id IS NULL
           OR r.released_worker_id <> $1
           OR r.released_worker_nonce IS DISTINCT FROM
                (SELECT ow.snapshot_register_nonce FROM workers ow WHERE ow.id = $1))
      -- Issue #3142: claim-time memory-authority gate. @memory_authority_lost is true when the
      -- claimant's request says (or, for a capable worker, omits) that it no longer holds memory
      -- authority; it then claims only the kinds that never reach a memory runtime. This mirrors
      -- agent/src/worker.ts dispatch, where only the final `this.runner.execute(claim)` arm
      -- reaches RunRunner (the only runner with a memory runtime); the exempt arms are
      -- cross_check, judge, job, review_target_run_id and an egress_profile_id (isolated_fetch)
      -- run. A new kind defaults to guard-bound (fail closed). Deliberately NOT mirrored into the
      -- spread-peer clause, CountOnlineWorkersClaimableForRun or the ephemeral provisioner
      -- queries, because the state is per-request, not a property of the worker row.
      AND (NOT $45::boolean
           OR r.kind IN ('judge', 'job', 'cross_check')
           OR r.review_target_run_id IS NOT NULL
           OR r.egress_profile_id IS NOT NULL)
    -- Three-level sort (PRD #320 D3): (1) resume affinity — a re-queued run
    -- prefers its prior worker, exactly as before; (2) priority rank —
    -- fn_run_priority slots BETWEEN affinity and FIFO, so an interactive run
    -- (rank 1) beats an earlier-created background judge/self_improve run
    -- (rank 0) and an expedited run (rank 2) beats both; (3) FIFO within a
    -- level. @background_grace_cutoff (now − RUN_BACKGROUND_GRACE) is the D4
    -- fail-open: a demoted run created before it reads as stale, so
    -- fn_run_priority returns normal and background work never starves.
    --
    -- PRD #3522 M3: a queued run carrying a weekly-reserve hold marker (reserve_hold_reason, written by
    -- the sweeper's markReserveHolds pass) sorts AFTER the owner's unmarked runs, right after the
    -- affinity key. Held runs are filtered out of `target` above, so the marker only matters where a
    -- stale marker outlives the reading that set it: the run is still claimable, it just does not
    -- starve unmarked siblings while the sweeper catches up.
    ORDER BY COALESCE(r.worker_id = $1, false) DESC,
             (r.reserve_hold_reason IS NOT NULL) ASC,
             fn_run_priority(r.kind, r.priority, r.created_at < $46) DESC,
             r.created_at ASC
    FOR UPDATE OF r SKIP LOCKED
    LIMIT 1
),
forge_locked AS MATERIALIZED (
    -- Issue #1324 D9: a run can need two reservations (Git and API origin) and must take both or
    -- neither. Lock the target's cooldown rows first, in a fixed order, after claimant and target;
    -- under READ COMMITTED a row changed since the statement snapshot is re-read at its latest
    -- version, so the all-or-nothing decision below sees committed state and a stale snapshot loses.
    SELECT c.origin, c.incarnation, c.observation_version, c.cooled_until, c.reservation_run_id
    FROM forge_origin_cooldowns c JOIN target t
      ON c.user_id = t.user_id AND c.worker_id = $1
        AND c.origin IN (t.forge_origin, t.forge_api_origin)
    ORDER BY c.origin
    FOR UPDATE OF c
),
forge_ok AS MATERIALIZED (
    -- A missing snapshot row stays advisory (no origin or synthetic lock is taken for absence).
    SELECT t.id FROM target t
    WHERE (t.forge_incarnation IS NULL OR EXISTS (
              SELECT 1 FROM forge_locked l
              WHERE l.origin = t.forge_origin AND l.incarnation = t.forge_incarnation
                AND l.observation_version = t.forge_observation_version
                AND l.cooled_until <= now() AND l.reservation_run_id IS NULL))
      AND (t.forge_api_incarnation IS NULL OR EXISTS (
              SELECT 1 FROM forge_locked l
              WHERE l.origin = t.forge_api_origin AND l.incarnation = t.forge_api_incarnation
                AND l.observation_version = t.forge_api_observation_version
                AND l.cooled_until <= now() AND l.reservation_run_id IS NULL))
),
forge_reservation AS (
    -- One conditional write, no retry. The existing row is locked only after
    -- claimant and target. A stale snapshot loses here without opening custody
    -- or advancing G. A concurrent first observation on the no-row path remains
    -- advisory: do not serialize insertion of a new origin row.
    UPDATE forge_origin_cooldowns c SET
        reservation_run_id = t.id,
        reservation_generation = t.claim_generation + 1,
        reservation_incarnation = c.incarnation,
        reservation_version = c.observation_version,
        reservation_observation_version = c.observation_version
    FROM target t
    WHERE c.user_id = t.user_id AND c.worker_id = $1 AND c.origin = t.forge_origin
      AND c.incarnation = t.forge_incarnation
      AND c.observation_version = t.forge_observation_version
      AND c.cooled_until <= now() AND c.reservation_run_id IS NULL
      AND EXISTS (SELECT 1 FROM forge_ok ok WHERE ok.id = t.id)
    RETURNING c.reservation_run_id AS run_id
),
forge_api_reservation AS (
    -- The API origin's half-open trial, reserved in the same statement under the same CAS.
    UPDATE forge_origin_cooldowns c SET
        reservation_run_id = t.id,
        reservation_generation = t.claim_generation + 1,
        reservation_incarnation = c.incarnation,
        reservation_version = c.observation_version,
        reservation_observation_version = c.observation_version
    FROM target t
    WHERE c.user_id = t.user_id AND c.worker_id = $1 AND c.origin = t.forge_api_origin
      AND t.forge_api_incarnation IS NOT NULL
      AND c.incarnation = t.forge_api_incarnation
      AND c.observation_version = t.forge_api_observation_version
      AND c.cooled_until <= now() AND c.reservation_run_id IS NULL
      AND EXISTS (SELECT 1 FROM forge_ok ok WHERE ok.id = t.id)
    RETURNING c.reservation_run_id AS run_id
),
forge_admitted AS MATERIALIZED (
    SELECT t.id, t.user_id, t.repo_id, t.kind, t.claim_generation, t.egress_profile_id, t.policy_pending_fresh, t.forge_connection_id, t.forge_origin, t.forge_incarnation, t.forge_observation_version, t.forge_api_origin, t.forge_api_incarnation, t.forge_api_observation_version FROM target t
    WHERE (t.forge_incarnation IS NULL
           OR EXISTS (SELECT 1 FROM forge_reservation fr WHERE fr.run_id = t.id))
      AND (t.forge_api_incarnation IS NULL
           OR EXISTS (SELECT 1 FROM forge_api_reservation fa WHERE fa.run_id = t.id))
),
hold AS (
    -- PRD #1296 M1 (D2/D3): open the H-free custody hold atomically with the claim, for a
    -- RECOVERY-CAPABLE worker (@recovery_capable, derived from the worker's advertised
    -- recovery_archive_v1, recovery_archive_v2 or recovery_inventory_v1 protocol capability) on one of the six code-publishing profiles.
    -- Reads FROM forge_admitted, so a failed reservation cannot open a hold.
    -- An admission-gated or idle claim also produces no hold. Both live
    -- FKs point at the claimed run + claiming worker (ON DELETE RESTRICT while open), and the
    -- immutable provenance columns record the taking worker's identity for a later
    -- AAD-authenticated post-terminal recovery retry. t.claim_generation + 1 is the SAME
    -- value the UPDATE below sets (same locked row, same snapshot).
    INSERT INTO recovery_custody_holds
        (id, user_id, repo_id, run_id, generation, state,
         original_worker_id, original_worker_identity, live_worker_id, live_run_id,
         created_at, updated_at, inventory_guarded)
    SELECT gen_random_uuid(), t.user_id, t.repo_id, t.id, t.claim_generation + 1, 'open',
           $1, $47::text, $1, t.id, now(), now(),
           ('recovery_inventory_v1' = ANY($28::text[]))
    FROM forge_admitted t
    WHERE $48::boolean
      AND t.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
      -- PRD #1906 M5: a profile-bound run publishes no code (no repo, no forge credential, no
      -- push), so it never opens a custody hold, whatever the claiming worker advertises.
      AND t.egress_profile_id IS NULL
    RETURNING 1
), fresh_consumed AS (
    -- Compatible-lane one-claim Fresh (#3607 Mf): the consumption audit commits in the SAME statement
    -- as the claim UPDATE below, keyed on the locked target carrying the pending authorization.
    -- 'resume' is a server-only input kind that is never delivered to a worker.
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT t.id, 'resume',
           '{"policy_fresh":"consumed","generation":' || (t.claim_generation + 1)::text || '}'
    FROM forge_admitted t
    WHERE t.policy_pending_fresh
    RETURNING 1
)
UPDATE runs SET
    forge_admission = jsonb_build_object('generation', runs.claim_generation + 1, 'worker_id', $1::uuid,
        'connection_id', (SELECT forge_connection_id FROM forge_admitted), 'origin', (SELECT forge_origin FROM forge_admitted),
        'api_origin', (SELECT forge_api_origin FROM forge_admitted)),
    cross_check_lane = COALESCE($2::text = 'cross_check', false),
    status     = 'claimed',
    status_since = now(),
    worker_id  = $1,
    claimed_at = now(),
    updated_at = now(),
    -- PRD #1296 M1 (D2): the general claim-lane counter, incremented once per successful
    -- claim. Returned in the claim payload; the hold above binds the identical value.
    claim_generation = runs.claim_generation + 1,
    -- Compatible-lane one-claim Fresh (#3607 Mf): a pending authorization is consumed by exactly this
    -- claim, which records the generation it spent it on; RETURNING projects it for this claim only.
    -- Every right-hand side reads the PRE-update row.
    policy_fresh_consumed_generation = CASE WHEN runs.policy_pending_fresh
        THEN runs.claim_generation + 1 ELSE runs.policy_fresh_consumed_generation END,
    policy_pending_fresh = false,
    -- #2988 reverses PRD #1147's provisional-requeue revocation: every successful
    -- fresh claim supersedes the old capability atomically, even for the same worker.
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    -- PRD #1247 M5 (D3): CLOSE the released-claim fence window. A held-state credential
    -- switch requeues the run with claim_released_at set (ReleaseCredentialSwitch), which
    -- makes the generation fence reject every report from the OLD flight. Reclaiming the
    -- run opens a fresh generation, so the fence must be cleared in the SAME atomic UPDATE
    -- that bumps claim_generation — otherwise a reclaim would immediately reject the NEW
    -- flight's own first report. A run that was never released has claim_released_at NULL
    -- already, so this is a harmless no-op on the ordinary claim path.
    claim_released_at = NULL,
    -- PRD #1497 M1 (D19): a successful claim clears the released-incarnation marker a server-side
    -- wall park recorded — the run now has a fresh, live owner, so the exclusion has done its job.
    -- (The excluded incarnation itself never reaches this UPDATE: the target predicate above bars it.)
    released_worker_id = NULL,
    released_worker_nonce = NULL,
    -- PRD #1390 M3 (D2 hygiene): clear the stale-requeue provenance on every fresh claim, alongside
    -- the generation bump. The refund in ReadoptRunsFromSnapshot fires only when
    -- stale_requeue_generation = claim_generation, so leaving a stale value here could refund a
    -- requeue_count charged against a generation this claim has already replaced.
    stale_requeue_generation = NULL,
    -- Historical exhaustion evidence describes the released flight, not this fresh claim.
    worker_recovery_evidence = NULL,
    -- PRD #1809 M6 (D8): a claim starts a new flight, so the previous park's checkpoint-durability
    -- report no longer describes this run. Cleared here AND on every running report
    -- (SetRunRunning) so a later park the worker does not report on (a server-side
    -- credential_disabled park of this claimed run, a sweeper wall park, a completion hold) shows
    -- "not reported" instead of an older park's value. Display-only.
    checkpoint_contains_latest = NULL,
    -- PRD #3522 M3: leaving 'queued' drops the weekly-reserve hold marker; the visible reason only
    -- describes a queued run.
    reserve_hold_reason = NULL,
    -- Exit contract (PRD #47 Decision 3): leaving 'queued' clears any health flag
    -- the detector raised (e.g. "no worker online"). health_notified_at is NOT reset.
    health = 'ok', health_reason = NULL, health_since = NULL
WHERE runs.id = (SELECT id FROM forge_admitted)
  AND runs.status = 'queued'
  AND runs.claim_generation = (SELECT claim_generation FROM forge_admitted)
RETURNING runs.id, runs.user_id, runs.repo_id, runs.issue_iid, runs.issue_title, runs.issue_description, runs.status, runs.requeue_count, runs.worker_id, runs.session_id, runs.last_seq, runs.branch, runs.mr_iid, runs.failure_reason, runs.plan_md, runs.iteration_count, runs.claimed_at, runs.started_at, runs.finished_at, runs.created_at, runs.updated_at, runs.origin_column, runs.board_column, runs.move_pending_since, runs.mr_state, runs.auto_approve, runs.autopilot_commented_at, runs.kind, runs.pipeline_id, runs.pipeline_ref, runs.failure_snapshot, runs.fix_verdict, runs.stop_kind, runs.agent_source, runs.agent_exclusions, runs.repo_agents, runs.title, runs.resume_of_run_id, runs.last_activity_at, runs.health, runs.health_reason, runs.health_since, runs.health_notified_at, runs.target_run_id, runs.mr_web_url, runs.prd_done_path, runs.prd_patch_settled_at, runs.anthropic_secret_id, runs.anthropic_secret_label, runs.anthropic_select_reason, runs.anthropic_headroom_pct, runs.wait_on_limit, runs.limit_resets_at, runs.retry_not_before, runs.limit_wait_count, runs.rate_limit_type, runs.open_question_id, runs.revise_count, runs.plan_source, runs.planned_base_commit, runs.require_base_match, runs.milestones_candidate, runs.milestones_frozen, runs.milestones_completed, runs.milestones_in_progress, runs.budget_max_iterations, runs.budget_wall_seconds, runs.schedule_id, runs.limit_dead_secret_id, runs.report_only, runs.report_md, runs.ci_config_paths, runs.model, runs.override_subagent_model, runs.fail_origin, runs.priority, runs.summary_intent, runs.summary_plan, runs.summary_deltas, runs.issue_comments, runs.base_branch, runs.open_mr, runs.dispatched_at, runs.review_target_run_id, runs.review_requested, runs.then_fix_requested, runs.then_fix_of_run_id, runs.preserved_patch, runs.required_capabilities, runs.stop_reason, runs.required_tools, runs.size_class, runs.interactive, runs.open_followup_id, runs.plan_changed_files, runs.scope_ceiling, runs.status_since, runs.review_comments, runs.budget_paused_seconds, runs.mr_rework_enabled, runs.trigger_source, runs.checkpoint_tip, runs.usage_refolded, runs.codex_secret_id, runs.codex_auth_mode, runs.codex_secret_label, runs.codex_account_key, runs.codex_material_revision, runs.codex_account_revision, runs.codex_claim_epoch, runs.codex_cap_hash, runs.pause_requested_at, runs.pause_mode, runs.pause_after_count, runs.checkpoint_tip_at, runs.recovery_wait_count, runs.recovery_retry_not_before, runs.completion_contract_version, runs.contract_revision, runs.completion_contract, runs.completion_attempts, runs.latest_completion_attempt, runs.milestones_agents, runs.hold_reason, runs.hold_captured_head, runs.completion_budget_exhausted_at, runs.completion_question_at, runs.budget_extension_seconds, runs.claim_generation, runs.harness, runs.recovery_wait_cause, runs.forge_park_count, runs.credential_override_mode, runs.credential_override_secret_id, runs.claim_released_at, runs.credential_switch_requested_at, runs.credential_switch_generation, runs.stale_requeue_generation, runs.budget_finalize_seconds, runs.released_worker_id, runs.released_worker_nonce, runs.gate_revision, runs.gate_presentation_id, runs.gate_presented_payload, runs.gate_payload_digest, runs.gate_refusal_count, runs.gate_refusal_generation, runs.disk_park_count, runs.checkpoint_contains_latest, runs.egress_profile_id, runs.egress_snapshot, runs.job_type, runs.finalize_resume_generation, runs.job_protocol, runs.first_started_at, runs.plan_cross_check_required, runs.plan_cross_check_gate_reason, runs.issue_raw_digest, runs.issue_saved_body, runs.issue_input_reason, runs.auto_approve_blocked_reasons, runs.plan_cross_check_diff_refusal, runs.worker_recovery_episode, runs.requeue_episode_baseline, runs.worker_recovery_evidence, runs.cross_check_lane, runs.cross_check_lane_generation, runs.completion_final_head, runs.code_cross_check_required, runs.provider_park_count, runs.provider_park_streak_started_at, runs.ci_config_guard, runs.memory_episode, runs.memory_intervention_count, runs.memory_policy, runs.completion_non_closing_reason, runs.ci_artifact_manifest, runs.checkpoint_attested_source, runs.source_publication_intent, runs.continued_issue_iid, runs.checkpoint_coordinates, runs.retained_park_count, runs.ignored_inferred_capabilities, runs.reserve_hold_reason, runs.reserve_exempt_until, runs.reserve_exempt_granted_at, runs.reserve_exempt_credential_id, runs.reserve_exempt_window_reset, runs.reserve_exempt_prior_pct, runs.hinted_capabilities, runs.capability_inference_evidence, runs.idle_stall_park_count, runs.forge_admission, runs.codex_select_reason, runs.recovery_wait_phase, runs.plan_cross_check_gated, runs.policy_preserve_required, runs.policy_pending_fresh, runs.policy_outcome_id, runs.policy_continuation_mode, runs.policy_resolved_at, runs.policy_applying_worker_id, runs.policy_applying_generation, runs.policy_source_generation, runs.policy_refusal_count, runs.policy_refusal_tag, runs.policy_refusal_phase, runs.policy_refusal_correlation_id, runs.policy_hold_published, runs.harness_preferred, runs.harness_decision_reason, runs.harness_preferred_score, runs.harness_other_score, runs.harness_preferred_measured, runs.harness_other_measured, runs.harness_preferred_held, runs.harness_other_held, runs.harness_preferred_weekly_reset_at, runs.harness_other_weekly_reset_at, runs.registration_evidence_generation, runs.registration_evidence_nonce, runs.registration_evidence_epoch, runs.snapshot_departed_generation, runs.snapshot_departed_nonce, runs.snapshot_departed_epoch, runs.repo_agent_source_commit, runs.repo_agent_source_required, runs.agent_source_requested, runs.agent_source_requested_basis, runs.plan_agent_source, runs.ci_fix_mr_iid, runs.policy_required_head, runs.policy_recovery_verified_generation, runs.policy_recovery_verified_head, runs.policy_hold_cause, runs.policy_fresh_consumed_generation, runs.policy_fresh_confirmed_generation
