-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4695: the history_retention sweeper pass trims pr_history_events with an age predicate on observed_at.
-- This btree serves the pr_history_events retention delete (DeleteExpiredPRHistoryEvents), whose age predicate is observed_at < cutoff AND NOT first_merge. The index is partial on NOT first_merge because the first_merge rows are never expired, so they never need to be in it. Measured at 1,000,000 rows, a no-op tick without the index ran about 105 ms after churn (about 30 ms after backlog), and the slowest batch of a compressed churn on the unindexed table was 416 ms.
-- Plan flip: a stale-statistics Seq Scan was also seen on pr_history_events with a btree on
-- observed_at present (about 30 ms, no timeout; estimated 731,064 rows, actual 0). The statement therefore
-- carries ORDER BY s.observed_at like the other three history tables, so the planner reads the
-- page in index order from this btree and removes the oldest rows first. The largest flip was
-- measured on codex_usage_samples at the maximum jsonb size with autovacuum disabled: a Seq Scan
-- at 1,088 ms reading 512,478 blocks.
-- Why btree and not BRIN, in order. BRIN (autosummarize on) with the statement WITHOUT ORDER BY
-- passed the backlog drain and the 250-arrival churn (4,400 ticks, no timeouts). The shipped
-- statement needs ORDER BY to keep the planner on the index once statistics go stale. BRIN cannot
-- supply that order, so the read needs a Bitmap Heap Scan plus a sort over every candidate row.
-- Against the shipped statement, at 1,000,000 rows after dropping this btree, a warm 90% backlog
-- batch took a median 253 ms (max 275 ms, 27 MB on-disk sort) against a 300 ms statement timeout,
-- and the compressed backlog drain failed with 29 statement timeouts (57014, slowest batch
-- 306 ms). After churn its no-op tick reads most of the heap (about 17,300 blocks of a heap of
-- about 19,600 to 20,200). So btree is chosen for the shared statement. BRIN's 4,400-tick churn
-- with the shipped statement had no timeouts (slowest batch 88 ms at 6 arrivals per tick, 215 ms
-- at 250), so the churn alone does not decide it; the backlog drain does.
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
