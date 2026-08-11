-- +goose Up
-- Per-schedule model override (PRD #300): two nullable text columns, both pure ADD
-- COLUMN with no CHECK change. run_schedules.model is the owner-set model a schedule
-- fires runs on (NULL = inherit the owner's per-user Worker model); runs.model is the
-- model frozen onto a run at fire time, read at claim assembly to override the per-user
-- default (NULL = inherit = today's behavior).
ALTER TABLE run_schedules ADD COLUMN model text;  -- per-schedule model override (NULL = inherit), PRD #300
ALTER TABLE runs          ADD COLUMN model text;  -- model frozen onto a run at fire time (NULL = inherit), PRD #300

-- +goose Down
ALTER TABLE runs          DROP COLUMN model;
ALTER TABLE run_schedules DROP COLUMN model;
