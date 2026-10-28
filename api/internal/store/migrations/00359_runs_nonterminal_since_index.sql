-- +goose NO TRANSACTION
-- +goose Up

-- PRD #3454: the maintenance readout lists the oldest non-terminal runs by (status_since, id).
-- runs is the largest table and almost all rows are terminal; this partial index bounds the
-- readout scans to the live rows and serves the ORDER BY. Build concurrently so runs stays
-- writable. This file holds only the index. A failed concurrent build can leave an invalid
-- index; drop this owned index before retrying rather than accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_nonterminal_since;
CREATE INDEX CONCURRENTLY idx_runs_nonterminal_since
    ON runs (status_since, id)
    WHERE status NOT IN ('completed', 'failed', 'cancelled');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_nonterminal_since;
