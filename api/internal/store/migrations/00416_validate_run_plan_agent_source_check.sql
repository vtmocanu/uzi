-- +goose Up
ALTER TABLE runs VALIDATE CONSTRAINT runs_plan_agent_source_check;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_plan_agent_source_check;
ALTER TABLE runs ADD CONSTRAINT runs_plan_agent_source_check
    CHECK (plan_agent_source IN ('own', 'repo')) NOT VALID;
