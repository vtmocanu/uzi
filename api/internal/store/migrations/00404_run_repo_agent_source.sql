-- +goose Up
ALTER TABLE runs ADD COLUMN repo_agent_source_commit text;
ALTER TABLE runs ADD CONSTRAINT runs_repo_agent_source_commit_check
    CHECK (repo_agent_source_commit IS NULL OR repo_agent_source_commit ~ '^[0-9a-f]{40}$') NOT VALID;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_repo_agent_source_commit_check;
ALTER TABLE runs DROP COLUMN IF EXISTS repo_agent_source_commit;
