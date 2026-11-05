-- +goose Up
ALTER TABLE runs ADD COLUMN repo_agent_source_commit text;
ALTER TABLE runs ADD CONSTRAINT runs_repo_agent_source_commit_check
    CHECK (repo_agent_source_commit IS NULL OR repo_agent_source_commit ~ '^[0-9a-f]{40}$') NOT VALID;
ALTER TABLE runs ADD COLUMN repo_agent_source_required boolean NOT NULL DEFAULT false;
UPDATE runs SET repo_agent_source_required = true
WHERE status NOT IN ('completed', 'failed', 'cancelled')
  AND kind IN ('issue', 'ci_fix', 'mr_rework', 'prompt', 'self_improve', 'task')
  AND review_target_run_id IS NULL
  AND egress_profile_id IS NULL;

-- +goose Down
ALTER TABLE runs DROP COLUMN IF EXISTS repo_agent_source_required;
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_repo_agent_source_commit_check;
ALTER TABLE runs DROP COLUMN IF EXISTS repo_agent_source_commit;
