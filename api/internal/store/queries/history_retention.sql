-- Age-based retention of recorded history (issue #4695), run by the history_retention sweeper
-- pass (internal/historyretention) through HistoryRetentionSession.DeleteBatch. Each statement is
-- ONE autocommitted batch delete of at most batch_size rows strictly older than cutoff.
--
-- Shape: the inner SELECT picks the page with FOR UPDATE SKIP LOCKED (a row another transaction
-- holds is skipped, never waited on) and the outer DELETE removes those rows by ctid, so the
-- delete is a Tid Scan and the page is bounded by LIMIT. The outer and inner tables carry
-- distinct aliases and every column is qualified, otherwise sqlc reports the column as
-- ambiguous. The predicate is repeated on the outer DELETE, so a row that changed between the
-- page read and the delete is not removed on a stale ctid. The comparison is strict: a row AT the
-- cutoff is kept.
--
-- pr_history_events: a first_merge row is the only record of when a PR first merged and is never
-- expired. pr_history_heads is not touched.
--
-- ORDER BY s.<expiry column> in the inner SELECT is load-bearing (issue #4695 plan-flip evidence):
-- without it, after a drain or churn leaves the table statistics stale, the planner chose a
-- Seq Scan for the page read even with the btree present. The largest case was a Seq Scan at
-- 1,088 ms reading 512,478 blocks, measured on codex_usage_samples at the maximum jsonb size with
-- autovacuum disabled. Similar whole-heap near-empty reads were seen on anthropic_usage_samples
-- and worker_capacity_samples, and the flip was also seen on pr_history_events (about 30 ms, no
-- timeout). The ORDER BY lets the planner satisfy LIMIT from the btree on the expiry column in
-- expiry order, an Index Scan, and it removes the oldest rows first. The indexes are created by
-- migrations 00549 to 00552.
--
-- Without a valid index on the expiry column the ORDER BY forces a sort of all expired rows (about
-- 27 MiB in the measured backlog), because LockRows sits between Limit and Sort and the limit
-- cannot stop the sort early. An INVALID or missing index would therefore make each batch sort
-- every expired row and likely hit the statement timeout (unmeasured). That is reachable only if
-- the boot migration left an invalid index while another process runs the pass; the
-- single-migrator prerequisite rules that out.

-- name: DeleteExpiredPRHistoryEvents :execrows
DELETE FROM pr_history_events AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM pr_history_events AS s
        WHERE s.observed_at < @cutoff::timestamptz AND NOT s.first_merge
        ORDER BY s.observed_at
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.observed_at < @cutoff::timestamptz
  AND NOT d.first_merge;

-- name: DeleteExpiredAnthropicUsageSamples :execrows
DELETE FROM anthropic_usage_samples AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM anthropic_usage_samples AS s
        WHERE s.bucket_start < @cutoff::timestamptz
        ORDER BY s.bucket_start
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.bucket_start < @cutoff::timestamptz;

-- name: DeleteExpiredCodexUsageSamples :execrows
DELETE FROM codex_usage_samples AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM codex_usage_samples AS s
        WHERE s.bucket_start < @cutoff::timestamptz
        ORDER BY s.bucket_start
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.bucket_start < @cutoff::timestamptz;

-- name: DeleteExpiredWorkerCapacitySamples :execrows
DELETE FROM worker_capacity_samples AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM worker_capacity_samples AS s
        WHERE s.bucket_start < @cutoff::timestamptz
        ORDER BY s.bucket_start
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.bucket_start < @cutoff::timestamptz;
