-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4700 M2: the UsageFolder missing lane (ListMissingRunUsageFoldRuns) walks the terminal,
-- non-chat, usage_refolded runs newest first by a keyset over (created_at DESC, id DESC), in
-- windows of 2000 runs. runs has no index on created_at, so without this one every call would
-- scan and top-N sort the whole eligible set (O(runs) per sweeper tick). This partial index
-- holds exactly the eligible runs in keyset order, so one window is an index range scan of at
-- most 2000 entries.
-- Build concurrently so the runs table stays writable. A failed concurrent build can leave an
-- invalid index; drop this owned index before retrying rather than accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_usage_fold_missing;
CREATE INDEX CONCURRENTLY idx_runs_usage_fold_missing
    ON runs (created_at DESC, id DESC)
    WHERE status IN ('completed', 'failed', 'cancelled') AND kind <> 'chat' AND usage_refolded;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_usage_fold_missing;
