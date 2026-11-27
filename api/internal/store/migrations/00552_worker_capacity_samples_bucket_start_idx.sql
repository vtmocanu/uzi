-- +goose NO TRANSACTION
-- +goose Up

-- Issue #4695: the history_retention sweeper pass trims worker_capacity_samples with an age predicate on bucket_start.
-- This btree serves the worker_capacity_samples retention delete (DeleteExpiredWorkerCapacitySamples), whose age predicate is bucket_start < cutoff.
-- Plan-flip evidence: the delete's inner page read, once the table statistics went stale after a
-- drain or churn, switched to a Seq Scan (1,107 ms against 5 ms with an Index Scan), measured on
-- codex_usage_samples with autovacuum disabled; similar whole-heap near-empty reads were seen on
-- anthropic_usage_samples and worker_capacity_samples. The btree was present in that case, so the
-- statement also carries ORDER BY s.bucket_start to make the planner read the page in index order.
-- Why btree and not BRIN: BRIN (autosummarize on) was measured at 1,000,000 rows against the
-- statement as shipped, with ORDER BY, after dropping this btree. BRIN cannot supply the ORDER BY,
-- so the read needs a Bitmap Heap Scan plus a sort over every candidate row. A warm 90% backlog
-- batch took a median 207 ms (max 221 ms, 27 MB on-disk sort) against a 300 ms statement timeout,
-- and the compressed backlog drain hit 1 statement timeout (57014, slowest batch 350 ms). The
-- 4,400-tick churn had no timeouts (slowest batch 205 ms), and a near-empty tick after it read
-- about 18,800 blocks in 164 ms. The earlier runs of the statement without ORDER BY reached about
-- 3,660 of 4,400 churn ticks before the wall cap, with no timeouts. The backlog drain fails, so btree.
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
DROP INDEX CONCURRENTLY IF EXISTS worker_capacity_samples_bucket_start_idx;
CREATE INDEX CONCURRENTLY worker_capacity_samples_bucket_start_idx
    ON worker_capacity_samples (bucket_start);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS worker_capacity_samples_bucket_start_idx;
