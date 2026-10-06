-- +goose Up
-- A cross-check is a repo-backed Codex child with its own clone. Its target_run_id
-- points to the lead; cross_checks.checker_run_id records the corresponding attempt.
-- The branch may start NULL: checkout identity comes from the child's run id.
ALTER TABLE runs DROP CONSTRAINT runs_trigger_source_check,
    ADD CONSTRAINT runs_trigger_source_check CHECK (trigger_source IN (
        'manual', 'autopilot', 'schedule', 'self_improve', 'ci_fix', 'mr_rework',
        'chat', 'task', 'task_review', 'then_fix', 'judge', 'judge_rerun', 'resume', 'cross_check'
    ));

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt', 'task', 'mr_rework', 'job', 'cross_check'));

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL)
 OR (kind = 'task'         AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL)
 OR (kind = 'mr_rework'    AND repo_id IS NOT NULL AND pipeline_ref IS NOT NULL AND mr_iid IS NOT NULL AND target_run_id IS NOT NULL)
 OR (kind = 'job'          AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND job_type IS NOT NULL)
 OR (kind = 'cross_check'  AND repo_id IS NOT NULL AND issue_iid IS NULL AND target_run_id IS NOT NULL AND job_type IS NULL
     AND harness = 'codex' AND report_only = true AND budget_wall_seconds IS NOT NULL));

-- +goose Down
-- cross_checks.checker_run_id is a non-cascading FK to runs.
DELETE FROM cross_checks WHERE checker_run_id IN (SELECT id FROM runs WHERE kind = 'cross_check')
    OR lead_run_id IN (SELECT id FROM runs WHERE kind = 'cross_check');
DELETE FROM runs WHERE kind = 'cross_check';

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL)
 OR (kind = 'task'         AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL)
 OR (kind = 'mr_rework'    AND repo_id IS NOT NULL AND pipeline_ref IS NOT NULL AND mr_iid IS NOT NULL AND target_run_id IS NOT NULL)
 OR (kind = 'job'          AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND job_type IS NOT NULL));

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt', 'task', 'mr_rework', 'job'));

ALTER TABLE runs DROP CONSTRAINT runs_trigger_source_check,
    ADD CONSTRAINT runs_trigger_source_check CHECK (trigger_source IN (
        'manual', 'autopilot', 'schedule', 'self_improve', 'ci_fix', 'mr_rework',
        'chat', 'task', 'task_review', 'then_fix', 'judge', 'judge_rerun', 'resume'
    ));
