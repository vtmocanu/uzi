-- +goose NO TRANSACTION
-- +goose Up

-- #1197, verified 2026-09-08: keep the growing runs table writable while
-- building its partial promotion index. Schema changes and rollback-sensitive
-- status narrowing remain transactional in 00206.
-- A failed concurrent build can leave an invalid index. Drop that exact owned
-- index before retrying rather than silently accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_recovery_wait_retry;
CREATE INDEX CONCURRENTLY idx_runs_recovery_wait_retry
    ON runs (recovery_retry_not_before)
    WHERE status = 'recovery_wait';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_recovery_wait_retry;
