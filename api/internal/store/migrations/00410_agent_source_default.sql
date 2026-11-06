-- +goose Up
ALTER TABLE repos ADD COLUMN agent_source_default text;
ALTER TABLE repos ADD CONSTRAINT repos_agent_source_default_check
    CHECK (agent_source_default IN ('own', 'repo')) NOT VALID;

ALTER TABLE runs ADD COLUMN agent_source_requested text;
ALTER TABLE runs ADD COLUMN agent_source_requested_basis text;
ALTER TABLE runs ADD CONSTRAINT runs_agent_source_requested_check
    CHECK (agent_source_requested IN ('own', 'repo')) NOT VALID;
ALTER TABLE runs ADD CONSTRAINT runs_agent_source_requested_basis_check
    CHECK (agent_source_requested_basis IN ('explicit', 'repo_default')) NOT VALID;
ALTER TABLE runs ADD CONSTRAINT runs_agent_source_requested_pair_check
    CHECK ((agent_source_requested IS NULL) = (agent_source_requested_basis IS NULL)) NOT VALID;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_agent_source_requested_pair_check;
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_agent_source_requested_basis_check;
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_agent_source_requested_check;
ALTER TABLE runs DROP COLUMN IF EXISTS agent_source_requested_basis;
ALTER TABLE runs DROP COLUMN IF EXISTS agent_source_requested;
ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_agent_source_default_check;
ALTER TABLE repos DROP COLUMN IF EXISTS agent_source_default;
