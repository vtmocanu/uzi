-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4207: the automatic hibernation pass asks, per worker, whether any non-terminal run
-- is assigned and when the newest terminal run ended. Without these the per-worker probes
-- scan runs. Build concurrently so the runs table stays writable.
-- A failed concurrent build can leave an invalid index; drop each owned index before retrying
-- rather than silently accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS runs_worker_nonterminal_idx;
CREATE INDEX CONCURRENTLY runs_worker_nonterminal_idx
    ON runs (worker_id)
    WHERE status NOT IN ('completed', 'failed', 'cancelled');

-- The newest terminal run's end is max(COALESCE(finished_at, status_since)) over terminal
-- runs, so the index holds exactly that expression, partial on the same predicate the
-- queries in hibernation_auto.sql use.
DROP INDEX CONCURRENTLY IF EXISTS runs_worker_terminal_end_idx;
CREATE INDEX CONCURRENTLY runs_worker_terminal_end_idx
    ON runs (worker_id, (COALESCE(finished_at, status_since)))
    WHERE status IN ('completed', 'failed', 'cancelled');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_worker_terminal_end_idx;
DROP INDEX CONCURRENTLY IF EXISTS runs_worker_nonterminal_idx;
