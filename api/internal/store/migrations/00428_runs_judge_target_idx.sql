-- +goose NO TRANSACTION
-- +goose Up

-- Issue #3257: RecordJudgeSkip asks whether ANY judge run (active or terminal) exists for a
-- target. The only existing index on runs(target_run_id) for judges is the active-only
-- partial unique index, so that NOT EXISTS would scan. This one covers every judge row.
-- Build concurrently so the runs table stays writable.
-- A failed concurrent build can leave an invalid index; drop this owned index
-- before retrying rather than silently accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS runs_judge_target_idx;
CREATE INDEX CONCURRENTLY runs_judge_target_idx
    ON runs (target_run_id)
    WHERE kind = 'judge';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_judge_target_idx;
