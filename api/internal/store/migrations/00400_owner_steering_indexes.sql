-- +goose NO TRANSACTION
-- +goose Up

-- Issue #3841: SnapshotOwnerSteering reads the newest run_user_inputs id of a run and range-scans
-- that run's inputs while the lead row is locked, then reads the run's question messages, and
-- probes the run's completion_decision rows by question_id to exclude completion-continue answers.
-- Without these three indexes it range-scans the primary key and every message of the run.
-- Build concurrently so the growing tables stay writable.
-- A failed concurrent build can leave an invalid index; drop this owned index
-- before retrying rather than silently accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS idx_run_user_inputs_run_id;
CREATE INDEX CONCURRENTLY idx_run_user_inputs_run_id
    ON run_user_inputs (run_id, id);

DROP INDEX CONCURRENTLY IF EXISTS idx_run_messages_question_seq;
CREATE INDEX CONCURRENTLY idx_run_messages_question_seq
    ON run_messages (run_id, seq)
    WHERE kind = 'question';

-- Supports the NOT EXISTS probe on completion_decision rows in SnapshotOwnerSteering's sel; without
-- it the custom-plan cost crosses jit_above_cost at roughly 750 inputs per run.
DROP INDEX CONCURRENTLY IF EXISTS idx_run_user_inputs_completion_qid;
CREATE INDEX CONCURRENTLY idx_run_user_inputs_completion_qid
    ON run_user_inputs (run_id, question_id)
    WHERE kind = 'completion_decision';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_run_user_inputs_completion_qid;
DROP INDEX CONCURRENTLY IF EXISTS idx_run_messages_question_seq;
DROP INDEX CONCURRENTLY IF EXISTS idx_run_user_inputs_run_id;
