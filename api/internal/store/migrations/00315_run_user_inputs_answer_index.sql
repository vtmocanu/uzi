-- +goose NO TRANSACTION
-- +goose Up

-- Issue #2602: the run-detail blocked-by hint asks whether ANY answer for the run's
-- open question exists (RunQuestionAnswerExists). Every other run_user_inputs index is
-- partial (pending, replay), so none serves that lookup and it would scan the table.
-- This partial index bounds it to answer rows and also serves SetRunRunning's answer
-- guard. Build concurrently so the busy run_user_inputs table stays writable.
-- A failed concurrent build can leave an invalid index; drop this owned index
-- before retrying rather than silently accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_run_user_inputs_answer;
CREATE INDEX CONCURRENTLY idx_run_user_inputs_answer
    ON run_user_inputs (run_id, question_id)
    WHERE kind = 'answer';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_run_user_inputs_answer;
