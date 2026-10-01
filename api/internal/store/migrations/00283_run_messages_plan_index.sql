-- +goose NO TRANSACTION
-- +goose Up

-- Issue #2041: ListPlanRevisionStateForRuns needs only plan and plan_revising
-- frames, not every message of the page's runs. This partial index bounds the
-- scan to those frames while preserving the query's (run_id, seq) ordering.
-- Build concurrently so the growing run_messages table stays writable.
-- A failed concurrent build can leave an invalid index; drop this owned index
-- before retrying rather than silently accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_run_messages_plan_seq;
CREATE INDEX CONCURRENTLY idx_run_messages_plan_seq
    ON run_messages (run_id, seq)
    WHERE kind IN ('plan', 'plan_revising');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_run_messages_plan_seq;
