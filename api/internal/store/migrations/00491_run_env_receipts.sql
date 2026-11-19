-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4416 M2: runs receive the repository variables.
--
-- run_env_receipts: one row per delivered (var_id, rev) per claim generation. Append-only
-- while the run's retired versions are kept; it carries no value and no hash. There is no
-- foreign key to run_env_vars (a receipt must outlive the version it names, so a missing
-- version stays visible as missing) and none to users (a receipt dies with its run). The
-- partial index serves the retention and gate reads, which look at secret receipts only.
CREATE TABLE run_env_receipts (
    run_id                 uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    user_id                uuid        NOT NULL,
    var_id                 uuid        NOT NULL,
    rev                    integer     NOT NULL,
    scope                  text        NOT NULL CHECK (scope IN ('repo', 'user')),
    name                   text        NOT NULL,
    secret                 boolean     NOT NULL,
    first_claim_generation bigint      NOT NULL,
    last_claim_generation  bigint      NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, var_id, rev)
);
CREATE INDEX run_env_receipts_secret_idx ON run_env_receipts (var_id, rev, run_id) WHERE secret;

-- run_env_var_ids: the immutable identity of a var_id (user, repo, scope, name). A var_id is minted
-- once and never reused for another variable, even after every version of it is gone, so a
-- receipt that names it can never be satisfied by a different variable. repo_id has no foreign key:
-- the identity survives its repo.
CREATE TABLE run_env_var_ids (
    var_id  uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id uuid,
    scope   text NOT NULL,
    name    text NOT NULL
);
INSERT INTO run_env_var_ids (var_id, user_id, repo_id, scope, name)
SELECT DISTINCT ON (var_id) var_id, user_id, repo_id, CASE WHEN repo_id IS NULL THEN 'user' ELSE 'repo' END, name
FROM run_env_vars
ORDER BY var_id, rev;

-- +goose StatementBegin
CREATE FUNCTION fn_run_env_vars_identity() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
    s      text;
    stored run_env_var_ids%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.var_id IS DISTINCT FROM OLD.var_id OR NEW.user_id IS DISTINCT FROM OLD.user_id
           OR NEW.repo_id IS DISTINCT FROM OLD.repo_id OR NEW.name IS DISTINCT FROM OLD.name THEN
            RAISE EXCEPTION 'run_env_vars: the identity of a variable (var_id, user, repo, name) is immutable'
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    s := CASE WHEN NEW.repo_id IS NULL THEN 'user' ELSE 'repo' END;
    INSERT INTO run_env_var_ids (var_id, user_id, repo_id, scope, name)
    VALUES (NEW.var_id, NEW.user_id, NEW.repo_id, s, NEW.name)
    ON CONFLICT (var_id) DO NOTHING;
    SELECT * INTO stored FROM run_env_var_ids WHERE var_id = NEW.var_id;
    IF stored.user_id IS DISTINCT FROM NEW.user_id OR stored.repo_id IS DISTINCT FROM NEW.repo_id
       OR stored.scope IS DISTINCT FROM s OR stored.name IS DISTINCT FROM NEW.name THEN
        RAISE EXCEPTION 'run_env_vars: var_id is already bound to a different variable'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$fn$;
-- +goose StatementEnd

CREATE TRIGGER run_env_vars_identity
    BEFORE INSERT OR UPDATE ON run_env_vars
    FOR EACH ROW EXECUTE FUNCTION fn_run_env_vars_identity();

-- The AAD generation a row was sealed under: 1 is the M1 context (domain, scope, repo, flag, name),
-- 2 also binds var_id and rev (internal/runenv AADKindV2), so a ciphertext copied between versions
-- of one variable no longer opens. Existing rows stay 1; every new seal writes 2.
ALTER TABLE run_env_vars ADD COLUMN aad_version smallint NOT NULL DEFAULT 1 CHECK (aad_version IN (1, 2));

-- The sweeper's worklist: retired and pending-deletion versions of an owner.
CREATE INDEX run_env_vars_retained_idx ON run_env_vars (user_id)
    WHERE retired_at IS NOT NULL OR pending_delete_at IS NOT NULL;

-- The one SQL statement of which runs receive variables, mirroring Go leadcfg.LeadLane (a lead kind
-- that is not a review run and not an isolated-lane run) and the fence in InsertRunLeadConfigFenced.
-- IMMUTABLE: it reads only its arguments. Every gate site calls it; none inlines the kind list.
-- +goose StatementBegin
CREATE FUNCTION fn_run_env_lane(p_kind text, p_review_target_run_id uuid, p_egress_profile_id uuid)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $fn$
    SELECT COALESCE(p_review_target_run_id IS NULL AND p_egress_profile_id IS NULL
                    AND p_kind IN ('issue', 'ci_fix', 'prompt', 'self_improve', 'mr_rework', 'task'), false)
$fn$;
-- +goose StatementEnd

-- The placement function gains the run-env clause (the twin of ClaimRun's). Same signature. The body
-- before this migration is the 00486 one, which the Down restores first.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_placement_admitting_workers(
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
        -- Preserve main's linked-repository issue-key gate when replacing this function.
        AND ('issue_key_v1' = ANY(w.protocol_capabilities) OR NOT EXISTS (SELECT 1 FROM repos lr WHERE lr.id = r.repo_id AND lr.issue_tracker_connection_id IS NOT NULL))
        AND ((NOT r.repo_agent_source_required AND r.repo_agent_source_commit IS NULL) OR 'repo_agent_source_v1' = ANY(w.protocol_capabilities))
        AND (r.agent_source_requested IS NULL OR 'agent_source_request_v1' = ANY(w.protocol_capabilities))
      AND (NOT (r.policy_preserve_required OR r.policy_pending_fresh OR (r.policy_outcome_id IS NOT NULL AND r.policy_resolved_at IS NULL) OR r.policy_required_head IS NOT NULL) OR 'provider_policy_hold_v1' = ANY(w.protocol_capabilities))
        AND (NOT (r.kind = 'prompt' AND r.ci_config_guard AND EXISTS (SELECT 1 FROM job_files af WHERE af.run_id = r.id AND af.direction = 'input' AND af.state = 'attached')) OR 'ci_artifacts_v1' = ANY(w.protocol_capabilities))
                 AND (NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                      OR ('codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities)))
                 AND (r.completion_contract_version IS NULL
                      OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                      OR 'codex_completion_interlock_v1' = ANY(w.protocol_capabilities))
                 -- PRD #4320 module 5: the provider claim clause (see ClaimRun's NON-BYPASSABLE provider clause): a
                 -- provider-bound run is placeable only on a worker reporting 'claude_provider_v1'.
                 AND (r.model_provider_key_mode IS NULL OR 'claude_provider_v1' = ANY(w.protocol_capabilities))
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
        -- PRD #4416 M2: the NON-BYPASSABLE run-environment claim clause, the twin of ClaimRun's. A run that
        -- receives repository variables (a lead lane, fn_run_env_lane) and has an enabled live variable on its
        -- repo or holds a secret receipt is placeable only on a worker reporting 'run_env_v1'.
        AND ('run_env_v1' = ANY(w.protocol_capabilities) OR NOT (fn_run_env_lane(r.kind, r.review_target_run_id, r.egress_profile_id) AND (EXISTS (SELECT 1 FROM run_env_vars ev WHERE ev.user_id = r.user_id AND ev.repo_id = r.repo_id AND ev.retired_at IS NULL AND ev.pending_delete_at IS NULL AND ev.disabled_at IS NULL) OR EXISTS (SELECT 1 FROM run_env_receipts rr WHERE rr.run_id = r.id AND rr.secret))))
        AND r.required_capabilities <@ (COALESCE(w.capabilities, '{}') || CASE WHEN COALESCE(w.docker_enabled, false) THEN ARRAY['docker'] ELSE ARRAY[]::text[] END)
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Restore the 00486 placement body first (it does not reference the function dropped below).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_placement_admitting_workers(
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
        -- Preserve main's linked-repository issue-key gate when replacing this function.
        AND ('issue_key_v1' = ANY(w.protocol_capabilities) OR NOT EXISTS (SELECT 1 FROM repos lr WHERE lr.id = r.repo_id AND lr.issue_tracker_connection_id IS NOT NULL))
        AND ((NOT r.repo_agent_source_required AND r.repo_agent_source_commit IS NULL) OR 'repo_agent_source_v1' = ANY(w.protocol_capabilities))
        AND (r.agent_source_requested IS NULL OR 'agent_source_request_v1' = ANY(w.protocol_capabilities))
      AND (NOT (r.policy_preserve_required OR r.policy_pending_fresh OR (r.policy_outcome_id IS NOT NULL AND r.policy_resolved_at IS NULL) OR r.policy_required_head IS NOT NULL) OR 'provider_policy_hold_v1' = ANY(w.protocol_capabilities))
        AND (NOT (r.kind = 'prompt' AND r.ci_config_guard AND EXISTS (SELECT 1 FROM job_files af WHERE af.run_id = r.id AND af.direction = 'input' AND af.state = 'attached')) OR 'ci_artifacts_v1' = ANY(w.protocol_capabilities))
                 AND (NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                      OR ('codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities)))
                 AND (r.completion_contract_version IS NULL
                      OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                      OR 'codex_completion_interlock_v1' = ANY(w.protocol_capabilities))
                 -- PRD #4320 module 5: the provider claim clause (see ClaimRun's NON-BYPASSABLE provider clause): a
                 -- provider-bound run is placeable only on a worker reporting 'claude_provider_v1'.
                 AND (r.model_provider_key_mode IS NULL OR 'claude_provider_v1' = ANY(w.protocol_capabilities))
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

DROP INDEX run_env_vars_retained_idx;
ALTER TABLE run_env_vars DROP COLUMN aad_version;
DROP TRIGGER run_env_vars_identity ON run_env_vars;
DROP FUNCTION fn_run_env_vars_identity();
DROP FUNCTION fn_run_env_lane(text, uuid, uuid);
DROP TABLE run_env_var_ids;
DROP TABLE run_env_receipts;
