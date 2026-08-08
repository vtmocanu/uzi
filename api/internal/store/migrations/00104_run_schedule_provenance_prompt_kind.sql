-- +goose Up

-- Run provenance for scheduled runs + the 'prompt' run kind (PRD #241). A run born
-- from a schedule points back at it via schedule_id (ON DELETE SET NULL — deleting a
-- schedule must not delete the runs it already spawned, only sever the link). The new
-- 'prompt' kind is repo-ful and issue-less, keyed to the schedule that made it; the
-- kind domain and per-kind shape widen the same drop/re-add way 00058 did.
ALTER TABLE runs ADD COLUMN schedule_id uuid REFERENCES run_schedules (id) ON DELETE SET NULL;

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt'));

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL AND schedule_id IS NOT NULL));

-- One non-terminal prompt run per schedule: a recurring prompt schedule must not
-- stack a second run onto a still-running one. Same at-enqueue backstop shape as
-- 00058's judge/self_improve partial indexes.
CREATE UNIQUE INDEX uq_runs_one_active_prompt_per_schedule
    ON runs (schedule_id)
    WHERE kind = 'prompt' AND status NOT IN ('completed', 'failed', 'cancelled');

-- +goose Down
DROP INDEX uq_runs_one_active_prompt_per_schedule;
ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL));
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve'));
ALTER TABLE runs DROP COLUMN schedule_id;
