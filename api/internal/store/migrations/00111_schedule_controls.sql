-- +goose Up
-- Scheduled-run controls (PRD #274): two nullable columns on run_schedules, both pure
-- ADD COLUMN with NO CHECK change (run_schedules_target_shape references only issue_iid
-- and prompt, so neither column is constrained by it). max_issues caps a sweep fan-out
-- (M2, NULL = unlimited, enforced as a SQL LIMIT in ListSweepCandidateIssues); guidance
-- is optional owner guidance injected into issue/sweep run instructions (M3, wired later).
ALTER TABLE run_schedules ADD COLUMN max_issues integer;   -- sweep fan-out cap (NULL = unlimited), PRD #274
ALTER TABLE run_schedules ADD COLUMN guidance   text;      -- optional owner guidance for issue/sweep runs, PRD #274

-- +goose Down
ALTER TABLE run_schedules DROP COLUMN guidance;
ALTER TABLE run_schedules DROP COLUMN max_issues;
