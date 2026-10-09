-- +goose Up
-- PRD #2460: a Codex lead's plan is cross-checked by a read-only CLAUDE child, so a
-- cross_check run may now carry harness 'claude' as well as 'codex'. The arm below is
-- the only change; the other nine arms are copied verbatim from 00300. That the child
-- is the OPPOSITE family of its lead is a cross-table fact a CHECK cannot state, so it
-- is a guarded write (CreatePlanCrossCheckChild / InsertPlanCrossCheck in runtime.sql).
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
     AND harness IN ('claude', 'codex') AND report_only = true AND budget_wall_seconds IS NOT NULL));

-- +goose Down
-- Earlier workers cannot reconcile a Claude child. Settle each pending check on one as
-- failed/superseded and bank the lead's wait exactly once (only an uncredited attempt),
-- the pattern of 00312's Down. cross_checks.checker_run_id is ON DELETE SET NULL
-- (00301), so deleting the child below keeps the decided history.
-- +goose StatementBegin
WITH pending AS MATERIALIZED (
 SELECT cc.id, cc.wait_credited FROM cross_checks cc
 JOIN runs child ON child.id = cc.checker_run_id
 WHERE child.kind = 'cross_check' AND child.harness = 'claude' AND cc.verdict = 'pending'
 FOR UPDATE OF cc
), settled AS (
 UPDATE cross_checks cc SET verdict = 'failed', reason_class = 'superseded',
   decided_at = LEAST(now(), cc.deadline_at), wait_credited = true
 FROM pending WHERE cc.id = pending.id
 RETURNING cc.lead_run_id, cc.created_at, cc.decided_at, pending.wait_credited AS was_credited
)
UPDATE runs lead SET budget_paused_seconds = lead.budget_paused_seconds
 + GREATEST(0, CEIL(EXTRACT(EPOCH FROM (settled.decided_at - settled.created_at)))::int)
FROM settled WHERE lead.id = settled.lead_run_id AND NOT settled.was_credited;
-- +goose StatementEnd

DELETE FROM runs WHERE kind = 'cross_check' AND harness = 'claude';

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
