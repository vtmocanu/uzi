-- +goose NO TRANSACTION
-- +goose Up

-- PRD #4320: serves the revision-prune NOT EXISTS probe and "runs bound to provider" lookups for
-- model_provider bindings. Partial, so the many unbound runs add nothing. Built concurrently so
-- runs stays writable. A failed concurrent build can leave an invalid index; drop this owned
-- index first rather than accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_model_provider;
CREATE INDEX CONCURRENTLY idx_runs_model_provider
    ON runs (model_provider_id, model_provider_revision)
    WHERE model_provider_id IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_model_provider;
