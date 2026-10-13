-- +goose NO TRANSACTION
-- +goose Up

-- PRD #2603: GetLatestProgressNote reads the newest progress_note frame of one run
-- for the active milestone. This partial index bounds the scan to those frames
-- while preserving the (run_id, seq) ordering. Build concurrently so the growing
-- run_messages table stays writable. This file holds only the index (the users
-- column is its own migration) so a failed build cannot leave a half-applied
-- column step and a retry never re-runs ADD COLUMN.
-- A failed concurrent build can leave an invalid index; drop this owned index
-- before retrying rather than silently accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_run_messages_progress_note_seq;
CREATE INDEX CONCURRENTLY idx_run_messages_progress_note_seq
    ON run_messages (run_id, seq)
    WHERE kind = 'progress_note';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_run_messages_progress_note_seq;
