-- +goose Up

-- This migration takes ACCESS EXCLUSIVE on repos and issues (new columns and CHECKs, which
-- scan every row while the lock is held) and SHARE ROW EXCLUSIVE on the other guarded tables
-- (CREATE TRIGGER). lock_timeout bounds only the wait for a lock, not how long it is held: fail
-- fast instead of queueing behind a long transaction and stalling every reader (the 00402 pattern).
SET LOCAL lock_timeout = '5s';

-- PRD #4173 M4: link a repo to an external issue tracker project, and keep that
-- link from ever being made once the repo has issue numbers of its own (D13).

-- The never-enabled marker (DL-3). A repo is "awaiting its first enable" from the
-- moment the membership listing discovers it (UpsertRepo sets it) until it is first
-- enabled. The column default is false, so every row that exists when this migration
-- runs counts as previously enabled and can never be linked.
ALTER TABLE repos ADD COLUMN awaiting_first_enable boolean NOT NULL DEFAULT false;

-- Clears the marker in the very statement that first enables the repo, on every
-- enable path (a raw UPDATE included), and refuses to set it back. No column list on
-- the trigger: it must see every UPDATE, because the refusal guards a write that
-- touches only awaiting_first_enable.
-- +goose StatementBegin
CREATE FUNCTION repos_awaiting_first_enable_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.enabled THEN
        NEW.awaiting_first_enable := false;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.awaiting_first_enable = false AND NEW.awaiting_first_enable = true THEN
        RAISE EXCEPTION 'repos.awaiting_first_enable is never set back once cleared (repo %)', NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER repos_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE ON repos
    FOR EACH ROW EXECUTE FUNCTION repos_awaiting_first_enable_guard();

-- The short name is capped at 44 characters so "<short>-<n>" always fits the 64
-- characters of issues.issue_key (64 - 1 for the dash - 19 digits of a bigint).
-- The link from a repo to one project of one issue tracker connection, plus the
-- eligibility tag that selects which issues the sync reads. All six columns are set
-- together or not at all. The connection FK keeps the default NO ACTION (checked at
-- the end of the statement) so deleting a connection that a repo still names is
-- refused, while a user-delete cascade is not refused midway.
ALTER TABLE repos
    ADD COLUMN issue_tracker_connection_id uuid REFERENCES issue_tracker_connections,
    ADD COLUMN issue_tracker_project_id    text,
    ADD COLUMN issue_tracker_project_short text,
    ADD COLUMN eligibility_tag_id          text,
    ADD COLUMN eligibility_tag_name        text,
    ADD COLUMN issue_tracker_linked_at     timestamptz;

ALTER TABLE repos ADD CONSTRAINT repos_issue_tracker_link_shape CHECK (
    (issue_tracker_connection_id IS NULL
        AND issue_tracker_project_id IS NULL
        AND issue_tracker_project_short IS NULL
        AND eligibility_tag_id IS NULL
        AND eligibility_tag_name IS NULL
        AND issue_tracker_linked_at IS NULL)
    OR
    (issue_tracker_connection_id IS NOT NULL
        AND issue_tracker_project_id IS NOT NULL
        AND issue_tracker_project_short IS NOT NULL
        AND eligibility_tag_id IS NOT NULL
        AND eligibility_tag_name IS NOT NULL
        AND issue_tracker_linked_at IS NOT NULL
        AND char_length(issue_tracker_project_id) BETWEEN 1 AND 128
        AND char_length(issue_tracker_project_short) BETWEEN 1 AND 44
        AND char_length(eligibility_tag_id) BETWEEN 1 AND 128
        AND char_length(eligibility_tag_name) BETWEEN 1 AND 256
        AND issue_tracker_project_id !~ '[[:cntrl:]]'
        AND issue_tracker_project_short !~ '[[:cntrl:]]'
        AND eligibility_tag_id !~ '[[:cntrl:]]'
        AND eligibility_tag_name !~ '[[:cntrl:]]'
        AND issue_tracker_project_short ~ '^[A-Za-z][A-Za-z0-9_]*$')
);

-- The readable id of an issue that came from an issue tracker (DL-1), for example
-- ABC-12. NULL for forge issues. The suffix must equal the row's own number, so a
-- key cannot name a different issue than the row it sits on.
ALTER TABLE issues ADD COLUMN issue_key text;
ALTER TABLE issues ADD CONSTRAINT issues_issue_key_shape CHECK (
    issue_key IS NULL OR (
        char_length(issue_key) <= 64
        AND issue_key ~ '^[A-Za-z][A-Za-z0-9_]*-[1-9][0-9]*$'
        AND issue_key LIKE '%-' || forge_issue_iid::text)
);

-- The issue-number guard (DL-4). One function for every table that stores an issue
-- number of a repo: a row with a non-NULL issue number whose repo is still awaiting
-- its first enable is refused, so no issue number can exist on a repo that could
-- still be linked. The link takes its own row lock; the marker only goes from true to
-- false, so a write that passed this guard proves the repo can never be linked later.
--
-- Trigger arguments: the repo id column name, then one or more issue number column
-- names. A NULL repo id (recommendation_filed_issues.filed_repo_id) is skipped.
-- SQLSTATE UZ413 is this guard's own code (class UZ is not a PostgreSQL class); the
-- store maps it, nothing else raises it.
-- +goose StatementBegin
CREATE FUNCTION issue_number_awaiting_first_enable_guard() RETURNS trigger AS $$
DECLARE
    j        jsonb := to_jsonb(NEW);
    repo_val text  := j ->> TG_ARGV[0];
    has_num  boolean := false;
    i        int;
BEGIN
    -- Fail closed: a trigger argument that names no column of the row (a rename or a
    -- typo) must not turn the guard into a silent pass.
    IF NOT (j ? TG_ARGV[0]) THEN
        RAISE EXCEPTION 'issue number guard on %: no column %', TG_TABLE_NAME, TG_ARGV[0];
    END IF;
    FOR i IN 1 .. TG_NARGS - 1 LOOP
        IF NOT (j ? TG_ARGV[i]) THEN
            RAISE EXCEPTION 'issue number guard on %: no column %', TG_TABLE_NAME, TG_ARGV[i];
        END IF;
    END LOOP;
    IF repo_val IS NULL THEN
        RETURN NEW;
    END IF;
    FOR i IN 1 .. TG_NARGS - 1 LOOP
        IF j ->> TG_ARGV[i] IS NOT NULL THEN
            has_num := true;
        END IF;
    END LOOP;
    IF NOT has_num THEN
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM repos WHERE id = repo_val::uuid AND awaiting_first_enable) THEN
        RAISE EXCEPTION 'repo % has never been enabled: % may not store an issue number', repo_val, TG_TABLE_NAME
            USING ERRCODE = 'UZ413';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER issues_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, forge_issue_iid ON issues
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'forge_issue_iid');
CREATE TRIGGER runs_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, issue_iid, continued_issue_iid ON runs
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'issue_iid', 'continued_issue_iid');
CREATE TRIGGER run_schedules_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, issue_iid ON run_schedules
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'issue_iid');
CREATE TRIGGER autopilot_triggers_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, issue_iid ON autopilot_triggers
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'issue_iid');
CREATE TRIGGER github_project_items_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, forge_issue_iid ON github_project_items
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'forge_issue_iid');
CREATE TRIGGER issue_proposals_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, created_issue_iid ON issue_proposals
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'created_issue_iid');
CREATE TRIGGER recommendation_filed_issues_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF filed_repo_id, filed_issue_iid ON recommendation_filed_issues
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('filed_repo_id', 'filed_issue_iid');
CREATE TRIGGER finding_dispositions_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, filed_issue_iid ON finding_dispositions
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'filed_issue_iid');
CREATE TRIGGER finding_group_operations_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, issue_iid ON finding_group_operations
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'issue_iid');

-- The placement-eligibility function of the ephemeral capability-gap trigger and the
-- hibernation wake-demand query (00440) is the one place both read the per-worker claim
-- clauses, so the issue_key_v1 clause joins it here, exactly as ClaimRun and the other
-- claim mirrors carry it in queries/runtime.sql. CREATE OR REPLACE keeps the signature, so
-- both callers keep working without a rewrite.
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
        -- PRD #4173: a run on a LINKED repo (repos.issue_tracker_connection_id) is claimable only by a
        -- worker reporting 'issue_key_v1' (it renders the run's references by issue_key and never
        -- writes Closes #<iid>). Standalone, outside fn_worker_can_claim and the capability-aware bind;
        -- capability arm first, so an upgraded worker never probes repos.
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
DROP TRIGGER finding_group_operations_awaiting_first_enable_guard ON finding_group_operations;
DROP TRIGGER finding_dispositions_awaiting_first_enable_guard ON finding_dispositions;
DROP TRIGGER recommendation_filed_issues_awaiting_first_enable_guard ON recommendation_filed_issues;
DROP TRIGGER issue_proposals_awaiting_first_enable_guard ON issue_proposals;
DROP TRIGGER github_project_items_awaiting_first_enable_guard ON github_project_items;
DROP TRIGGER autopilot_triggers_awaiting_first_enable_guard ON autopilot_triggers;
DROP TRIGGER run_schedules_awaiting_first_enable_guard ON run_schedules;
DROP TRIGGER runs_awaiting_first_enable_guard ON runs;
DROP TRIGGER issues_awaiting_first_enable_guard ON issues;
DROP FUNCTION issue_number_awaiting_first_enable_guard();
ALTER TABLE issues DROP CONSTRAINT issues_issue_key_shape;
ALTER TABLE issues DROP COLUMN issue_key;
ALTER TABLE repos DROP CONSTRAINT repos_issue_tracker_link_shape;
ALTER TABLE repos
    DROP COLUMN issue_tracker_linked_at,
    DROP COLUMN eligibility_tag_name,
    DROP COLUMN eligibility_tag_id,
    DROP COLUMN issue_tracker_project_short,
    DROP COLUMN issue_tracker_project_id,
    DROP COLUMN issue_tracker_connection_id;
DROP TRIGGER repos_awaiting_first_enable_guard ON repos;
DROP FUNCTION repos_awaiting_first_enable_guard();
ALTER TABLE repos DROP COLUMN awaiting_first_enable;
-- Restores the 00440 body (without the issue_key_v1 clause).
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
