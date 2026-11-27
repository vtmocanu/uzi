-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4695: the history_retention sweeper pass trims pr_history_events with an age predicate on observed_at.
-- This btree serves the pr_history_events retention delete (DeleteExpiredPRHistoryEvents), whose age predicate is observed_at < cutoff AND NOT first_merge. The index is partial on NOT first_merge because the first_merge rows are never expired, so they never need to be in it. Measured at 1,000,000 rows, a no-op tick without the index ran about 105 ms with one 416 ms tick.
-- Plan-flip evidence: the delete's inner page read, once the table statistics went stale after a
-- drain or churn, switched to a Seq Scan (1,107 ms against 5 ms with an Index Scan in the measured case) even with this
-- btree present; the statement therefore also carries ORDER BY s.observed_at so the planner reads the
-- page in index order. BRIN failed after churn on every table, so btree is the choice.
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
