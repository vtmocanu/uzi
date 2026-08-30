-- +goose Up

-- issue #857: first-class trigger provenance for a run — what/how/who started it.
-- Stamped by the app at each create entrypoint; DEFAULT 'manual' is a safety net for
-- any insert that forgets to stamp. The backfill below is best-effort inference from
-- the same column combination that was previously the only way to derive the trigger.
ALTER TABLE runs
    ADD COLUMN trigger_source text NOT NULL DEFAULT 'manual'
        CHECK (trigger_source IN (
            'manual', 'autopilot', 'schedule', 'self_improve', 'ci_fix', 'mr_rework',
            'chat', 'task', 'task_review', 'then_fix', 'judge', 'judge_rerun', 'resume'
        ));

-- Best-effort backfill (issue #857 item 4). Keyed on kind + the lineage/flag columns.
-- Limitations, by design: judge_rerun is not historically distinguishable from judge
-- (both are kind='judge'), so historical judge rows read 'judge'. schedule_id is
-- ON DELETE SET NULL, so an issue run whose schedule was later deleted has no schedule_id
-- and falls through to 'autopilot' (if auto_approve) or 'manual'.
UPDATE runs SET trigger_source = CASE
    WHEN kind = 'judge'                                      THEN 'judge'
    WHEN kind = 'chat' AND resume_of_run_id IS NOT NULL      THEN 'resume'
    WHEN kind = 'chat'                                       THEN 'chat'
    WHEN kind = 'ci_fix'                                     THEN 'ci_fix'
    WHEN kind = 'self_improve'                               THEN 'self_improve'
    WHEN kind = 'mr_rework'                                  THEN 'mr_rework'
    WHEN kind = 'task' AND review_target_run_id IS NOT NULL  THEN 'task_review'
    WHEN kind = 'task' AND then_fix_of_run_id IS NOT NULL    THEN 'then_fix'
    WHEN kind = 'task'                                       THEN 'task'
    WHEN kind = 'prompt'                                     THEN 'schedule'
    WHEN kind = 'issue' AND auto_approve                     THEN 'autopilot'
    WHEN kind = 'issue' AND schedule_id IS NOT NULL          THEN 'schedule'
    ELSE 'manual'
END;

-- +goose Down
ALTER TABLE runs DROP COLUMN trigger_source;
