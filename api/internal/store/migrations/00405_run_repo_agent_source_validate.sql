-- +goose Up
ALTER TABLE runs VALIDATE CONSTRAINT runs_repo_agent_source_commit_check;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_repo_agent_source_commit_check;
ALTER TABLE runs ADD CONSTRAINT runs_repo_agent_source_commit_check
    CHECK (repo_agent_source_commit IS NULL OR repo_agent_source_commit ~ '^[0-9a-f]{40}$') NOT VALID;
