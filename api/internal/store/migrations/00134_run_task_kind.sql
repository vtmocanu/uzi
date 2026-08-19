-- +goose Up

-- The 'task' run kind (PRD #400): uzi handoff — an ephemeral, branch-scoped,
-- issue-LESS but repo-FUL run, the seventh runs.kind. It is modeled on 'prompt'
-- (00104): repo-ful and issue-less, reusing the existing issue_description column
-- for its inline context (inheriting the 256 KiB cap + sanitization). What makes it
-- novel is its BRANCH: a task's destination is server-named uzi/task/<run-id> and is
-- known AT CREATE TIME, so 'task' is the FIRST kind whose shape clause requires
-- `branch IS NOT NULL` (every existing kind writes branch only at completion). Two
-- new columns ride here: base_branch (the source ref the task branches from, nullable)
-- and open_mr (whether the worker opens an MR at the end — off by default; a plain
-- handoff produces commits on the branch, not an MR). The kind domain and per-kind
-- shape widen the same drop/re-add way 00104/00058 did.
ALTER TABLE runs ADD COLUMN base_branch text;
ALTER TABLE runs ADD COLUMN open_mr boolean NOT NULL DEFAULT false;

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt', 'task'));

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL)
 OR (kind = 'task'         AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL));

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL));
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt'));
ALTER TABLE runs DROP COLUMN open_mr;
ALTER TABLE runs DROP COLUMN base_branch;
