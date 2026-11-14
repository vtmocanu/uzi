-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4207: the automatic hibernation pass asks, per worker, whether any non-terminal run
-- is assigned and when the newest terminal run finished. Without these the per-worker probes
-- scan runs. Build concurrently so the runs table stays writable.
-- A failed concurrent build can leave an invalid index; drop each owned index before retrying
-- rather than silently accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS runs_worker_nonterminal_idx;
CREATE INDEX CONCURRENTLY runs_worker_nonterminal_idx
    ON runs (worker_id)
    WHERE status NOT IN ('completed', 'failed', 'cancelled');

DROP INDEX CONCURRENTLY IF EXISTS runs_worker_finished_idx;
CREATE INDEX CONCURRENTLY runs_worker_finished_idx
    ON runs (worker_id, finished_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_worker_finished_idx;
DROP INDEX CONCURRENTLY IF EXISTS runs_worker_nonterminal_idx;
