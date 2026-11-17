-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4395: the owner activity anchor reads runs by user_id. Build concurrently so the runs
-- table stays writable. A failed concurrent build can leave an invalid index; drop the owned
-- index before retrying rather than silently accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS runs_user_nonterminal_idx;
CREATE INDEX CONCURRENTLY runs_user_nonterminal_idx
    ON runs (user_id)
    WHERE status NOT IN ('completed', 'failed', 'cancelled');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_user_nonterminal_idx;
