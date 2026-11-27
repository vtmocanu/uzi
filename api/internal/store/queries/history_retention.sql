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

-- name: DeleteExpiredPRHistoryEvents :execrows
DELETE FROM pr_history_events AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM pr_history_events AS s
        WHERE s.observed_at < @cutoff::timestamptz AND NOT s.first_merge
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.observed_at < @cutoff::timestamptz
  AND NOT d.first_merge;

-- name: DeleteExpiredAnthropicUsageSamples :execrows
DELETE FROM anthropic_usage_samples AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM anthropic_usage_samples AS s
        WHERE s.bucket_start < @cutoff::timestamptz
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.bucket_start < @cutoff::timestamptz;

-- name: DeleteExpiredCodexUsageSamples :execrows
DELETE FROM codex_usage_samples AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM codex_usage_samples AS s
        WHERE s.bucket_start < @cutoff::timestamptz
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.bucket_start < @cutoff::timestamptz;

-- name: DeleteExpiredWorkerCapacitySamples :execrows
DELETE FROM worker_capacity_samples AS d
WHERE d.ctid = ANY(ARRAY(
        SELECT s.ctid FROM worker_capacity_samples AS s
        WHERE s.bucket_start < @cutoff::timestamptz
        LIMIT @batch_size::int
        FOR UPDATE SKIP LOCKED))
  AND d.bucket_start < @cutoff::timestamptz;
