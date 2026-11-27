-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4695: the history_retention sweeper pass trims pr_history_events with an age predicate on observed_at.
-- This btree serves the pr_history_events retention delete (DeleteExpiredPRHistoryEvents), whose age predicate is observed_at < cutoff AND NOT first_merge. The index is partial on NOT first_merge because the first_merge rows are never expired, so they never need to be in it. Measured at 1,000,000 rows, a no-op tick without the index ran about 105 ms, and the slowest batch of a compressed churn on the unindexed table was 416 ms.
-- Plan flip: no flip was observed on pr_history_events. The statement carries ORDER BY s.observed_at
-- like the other three history tables, so the planner reads the page in index order from this
-- btree and removes the oldest rows first (the flip itself was measured on codex_usage_samples
-- with autovacuum disabled, 1,107 ms against 5 ms).
-- Why btree and not BRIN: BRIN (autosummarize on) was measured at 1,000,000 rows against the
-- statement as shipped, with ORDER BY, after dropping this btree. BRIN cannot supply the ORDER BY,
-- so the read needs a Bitmap Heap Scan plus a sort over every candidate row. A warm 90% backlog
-- batch took a median 253 ms (max 275 ms, 27 MB on-disk sort) against a 300 ms statement timeout,
-- and the compressed backlog drain hit 29 statement timeouts (57014, slowest batch 306 ms). The
-- 4,400-tick churn had no timeouts (slowest batch 88 ms at 6 arrivals per tick, 215 ms at 250), as
-- did the earlier runs of the statement without ORDER BY. The backlog drain fails, so btree.
-- Build concurrently so the table stays writable. goose v3.29.0 (migration_sql.go:70-86) runs a
-- NO TRANSACTION migration's statements one by one and calls InsertVersionNoTx only after they all
-- returned, so the version is recorded only after the concurrent build returned a valid index; the
-- precedents are 00265_index_checkpoint_retention_backfill.sql and 00487_runs_hold_promoter_idx.sql.
-- Recovery: a build interrupted before the version is recorded is re-run from the drop at the next
-- boot, and a failed concurrent build can leave an invalid index, so the drop below runs first and
-- IF NOT EXISTS is deliberately not used (it would accept an invalid leftover). An orphaned backend
-- still building this index can delay that drop. This relies on a single migrator: store.Migrate
-- configures no goose session locker, so two processes migrating at once could each drop the
-- other's build in progress; the deployment is expected to run one migrating api process.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS pr_history_events_observed_at_idx;
CREATE INDEX CONCURRENTLY pr_history_events_observed_at_idx
    ON pr_history_events (observed_at) WHERE NOT first_merge;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS pr_history_events_observed_at_idx;
