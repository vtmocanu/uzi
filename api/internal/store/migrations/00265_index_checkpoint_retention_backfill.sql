-- +goose NO TRANSACTION
-- +goose Up

-- PRD #1810 M4: keep the growing runs table writable while building the partial index behind
-- the checkpoint-retention backfill (ListCheckpointRetentionBackfill). The backfill pages
-- terminal runs that published a checkpoint, ordered by their backfill key
-- GREATEST(status_since, checkpoint_tip_at) (the later of the terminal transition and the last
-- publish, so a late first publish after the run went terminal is not keyed below the
-- watermark) from the persisted watermark (checkpoint_retention_meta.backfilled_through, 00264).
-- This expression index, on exactly the query's key and with its partial predicate, bounds that
-- scan to the terminal-with-checkpoint subset instead of the whole runs table. Kept out of 00264 so
-- that migration stays transactional (00091's rule: a concurrent index gets its own migration).
-- The number is a draft, renumbered above the live head at landing (task migration:renumber).
-- A failed concurrent build can leave an invalid index. Drop that exact owned index before
-- retrying rather than silently accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_checkpoint_backfill;
CREATE INDEX CONCURRENTLY idx_runs_checkpoint_backfill
    ON runs ((GREATEST(status_since, checkpoint_tip_at)))
    WHERE status IN ('completed', 'failed', 'cancelled') AND checkpoint_tip IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_checkpoint_backfill;
