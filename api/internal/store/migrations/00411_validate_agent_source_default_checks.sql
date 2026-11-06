-- +goose Up
ALTER TABLE repos VALIDATE CONSTRAINT repos_agent_source_default_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_agent_source_requested_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_agent_source_requested_basis_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_agent_source_requested_pair_check;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_agent_source_requested_pair_check;
ALTER TABLE runs ADD CONSTRAINT runs_agent_source_requested_pair_check
    CHECK ((agent_source_requested IS NULL) = (agent_source_requested_basis IS NULL)) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_agent_source_requested_basis_check;
ALTER TABLE runs ADD CONSTRAINT runs_agent_source_requested_basis_check
    CHECK (agent_source_requested_basis IN ('explicit', 'repo_default')) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_agent_source_requested_check;
ALTER TABLE runs ADD CONSTRAINT runs_agent_source_requested_check
    CHECK (agent_source_requested IN ('own', 'repo')) NOT VALID;
ALTER TABLE repos DROP CONSTRAINT repos_agent_source_default_check;
ALTER TABLE repos ADD CONSTRAINT repos_agent_source_default_check
    CHECK (agent_source_default IN ('own', 'repo')) NOT VALID;
