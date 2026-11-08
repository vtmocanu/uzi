-- +goose Up
ALTER TABLE runs ADD COLUMN plan_agent_source text;
ALTER TABLE runs ADD CONSTRAINT runs_plan_agent_source_check
    CHECK (plan_agent_source IN ('own', 'repo')) NOT VALID;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_plan_agent_source_check;
ALTER TABLE runs DROP COLUMN IF EXISTS plan_agent_source;
