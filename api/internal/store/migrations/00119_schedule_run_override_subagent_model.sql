-- +goose Up
-- Per-schedule "apply model also to agents" opt-in (PRD #305): two boolean columns, both
-- pure ADD COLUMN with a NOT NULL DEFAULT false. run_schedules.override_subagent_model is
-- the owner-set opt-in that makes a schedule's model override every subagent's pin too
-- (false = pins win, today's behaviour); runs.override_subagent_model is the flag frozen
-- onto a run at fire time, read at claim assembly (M3) and honoured worker-side (M4).
ALTER TABLE run_schedules ADD COLUMN override_subagent_model boolean NOT NULL DEFAULT false;  -- opt-in: apply the run's model to subagents too (NULL/false = pins win), PRD #305
ALTER TABLE runs          ADD COLUMN override_subagent_model boolean NOT NULL DEFAULT false;  -- frozen onto a run at fire time, PRD #305

-- +goose Down
ALTER TABLE runs          DROP COLUMN override_subagent_model;
ALTER TABLE run_schedules DROP COLUMN override_subagent_model;
