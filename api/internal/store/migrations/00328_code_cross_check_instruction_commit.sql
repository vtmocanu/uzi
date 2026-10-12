-- +goose Up
ALTER TABLE cross_checks ADD COLUMN repo_instructions_commit text;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_repo_instructions_commit_check CHECK (
 repo_instructions_commit IS NULL OR (
   stage = 'code' AND head_commit IS NOT NULL AND base_commit IS NOT NULL
   AND repo_instructions_commit ~ '^[0-9a-f]{40}$'
 ));
-- Historical attempts retain a null pin.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_code_cross_check_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.stage = 'code' OR NEW.stage = 'code' THEN
  IF NEW.stage IS DISTINCT FROM OLD.stage
    OR NEW.lead_run_id IS DISTINCT FROM OLD.lead_run_id
    OR NEW.lead_claim_generation IS DISTINCT FROM OLD.lead_claim_generation
    OR NEW.head_commit IS DISTINCT FROM OLD.head_commit
    OR NEW.base_commit IS DISTINCT FROM OLD.base_commit
    OR NEW.candidate_digest IS DISTINCT FROM OLD.candidate_digest
    OR NEW.code_context IS DISTINCT FROM OLD.code_context
    OR NEW.guidance_snapshot IS DISTINCT FROM OLD.guidance_snapshot
    OR NEW.guidance_text IS DISTINCT FROM OLD.guidance_text
    OR NEW.guidance_digest IS DISTINCT FROM OLD.guidance_digest
    OR NEW.repo_instructions_commit IS DISTINCT FROM OLD.repo_instructions_commit
    OR NEW.repo_instructions_enabled IS DISTINCT FROM OLD.repo_instructions_enabled
    OR NEW.repo_instructions_text IS DISTINCT FROM OLD.repo_instructions_text
    OR NEW.repo_instructions_digest IS DISTINCT FROM OLD.repo_instructions_digest
    OR NEW.plan_md IS DISTINCT FROM OLD.plan_md
    OR NEW.milestones IS DISTINCT FROM OLD.milestones
    OR NEW.required_capabilities IS DISTINCT FROM OLD.required_capabilities
    OR NEW.required_tools IS DISTINCT FROM OLD.required_tools
    OR NEW.size_class IS DISTINCT FROM OLD.size_class
    OR NEW.planning_diff IS DISTINCT FROM OLD.planning_diff
    OR NEW.created_at IS DISTINCT FROM OLD.created_at
    OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at THEN
   RAISE EXCEPTION 'immutable code cross-check snapshot';
  END IF;
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- Restore the prior trigger function before removing its referenced column.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_code_cross_check_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.stage = 'code' OR NEW.stage = 'code' THEN
  IF NEW.stage IS DISTINCT FROM OLD.stage
    OR NEW.lead_run_id IS DISTINCT FROM OLD.lead_run_id
    OR NEW.lead_claim_generation IS DISTINCT FROM OLD.lead_claim_generation
    OR NEW.head_commit IS DISTINCT FROM OLD.head_commit
    OR NEW.base_commit IS DISTINCT FROM OLD.base_commit
    OR NEW.candidate_digest IS DISTINCT FROM OLD.candidate_digest
    OR NEW.code_context IS DISTINCT FROM OLD.code_context
    OR NEW.guidance_snapshot IS DISTINCT FROM OLD.guidance_snapshot
    OR NEW.guidance_text IS DISTINCT FROM OLD.guidance_text
    OR NEW.guidance_digest IS DISTINCT FROM OLD.guidance_digest
    OR NEW.repo_instructions_enabled IS DISTINCT FROM OLD.repo_instructions_enabled
    OR NEW.repo_instructions_text IS DISTINCT FROM OLD.repo_instructions_text
    OR NEW.repo_instructions_digest IS DISTINCT FROM OLD.repo_instructions_digest
    OR NEW.plan_md IS DISTINCT FROM OLD.plan_md
    OR NEW.milestones IS DISTINCT FROM OLD.milestones
    OR NEW.required_capabilities IS DISTINCT FROM OLD.required_capabilities
    OR NEW.required_tools IS DISTINCT FROM OLD.required_tools
    OR NEW.size_class IS DISTINCT FROM OLD.size_class
    OR NEW.planning_diff IS DISTINCT FROM OLD.planning_diff
    OR NEW.created_at IS DISTINCT FROM OLD.created_at
    OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at THEN
   RAISE EXCEPTION 'immutable code cross-check snapshot';
  END IF;
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_repo_instructions_commit_check,
 DROP COLUMN repo_instructions_commit;
