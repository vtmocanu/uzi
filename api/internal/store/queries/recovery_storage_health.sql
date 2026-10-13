-- name: RecoveryStorageHealth :many
-- One snapshot for all accounting; owner examples are limited only after totals.
-- Custody state does not affect stored quota refusals or byte accounting.
WITH contributions AS (
  SELECT c.user_id,
    (CASE WHEN c.state = 'available' THEN 1 ELSE 0 END)::bigint AS available_count,
    (CASE WHEN c.state = 'available' THEN COALESCE(c.byte_size, 0) ELSE 0 END)::bigint AS available_bytes,
    (CASE WHEN c.state = 'preparing' THEN 1 ELSE 0 END)::bigint AS preparing_count,
    (CASE WHEN c.state = 'preparing' THEN COALESCE(c.reserved_bytes, 0) ELSE 0 END)::bigint AS preparing_bytes,
    (CASE WHEN c.state = 'uploading' THEN 1 ELSE 0 END)::bigint AS uploading_count,
    (CASE WHEN c.state = 'uploading' THEN COALESCE(c.reserved_bytes, 0) ELSE 0 END)::bigint AS uploading_bytes,
    0::bigint AS job_count, 0::bigint AS job_bytes, 0::bigint AS reclaimable_bytes,
    (CASE WHEN c.state = 'needs_action' AND c.reason = 'storage quota exceeded' THEN 1 ELSE 0 END)::bigint AS refused_count
  FROM recovery_captures c
  WHERE c.state IN ('available', 'preparing', 'uploading')
     OR (c.state = 'needs_action' AND c.reason = 'storage quota exceeded')
  UNION ALL
  SELECT f.user_id, 0, 0, 0, 0, 0, 0, 1, f.byte_size,
    CASE WHEN (f.state IN ('unattached', 'available') AND f.expires_at IS NOT NULL AND f.expires_at < @now::timestamptz)
                OR f.state = 'available' THEN f.byte_size ELSE 0 END, 0
  FROM job_files f WHERE f.state <> 'expired'
), owners AS (
  SELECT user_id,
    sum(available_count)::bigint AS available_count, sum(available_bytes)::bigint AS available_bytes,
    sum(preparing_count)::bigint AS preparing_count, sum(preparing_bytes)::bigint AS preparing_bytes,
    sum(uploading_count)::bigint AS uploading_count, sum(uploading_bytes)::bigint AS uploading_bytes,
    sum(job_count)::bigint AS job_count, sum(job_bytes)::bigint AS job_bytes,
    sum(reclaimable_bytes)::bigint AS reclaimable_bytes, sum(refused_count)::bigint AS refused_count
  FROM contributions GROUP BY user_id
), totals AS (
  SELECT count(*)::bigint AS owner_count,
    COALESCE(sum(available_count), 0)::bigint AS available_count,
    COALESCE(sum(available_bytes), 0)::bigint AS available_bytes,
    COALESCE(sum(preparing_count), 0)::bigint AS preparing_count,
    COALESCE(sum(preparing_bytes), 0)::bigint AS preparing_bytes,
    COALESCE(sum(uploading_count), 0)::bigint AS uploading_count,
    COALESCE(sum(uploading_bytes), 0)::bigint AS uploading_bytes,
    COALESCE(sum(job_count), 0)::bigint AS job_count,
    COALESCE(sum(job_bytes), 0)::bigint AS job_bytes,
    COALESCE(sum(reclaimable_bytes), 0)::bigint AS reclaimable_bytes,
    COALESCE(sum(refused_count), 0)::bigint AS refused_count
  FROM owners
), examples AS (
  SELECT * FROM owners ORDER BY refused_count DESC, user_id LIMIT @example_limit::int
)
SELECT t.*, GREATEST(t.owner_count - (SELECT count(*) FROM examples), 0)::bigint AS omitted_count,
  e.user_id AS owner_id,
  COALESCE(e.available_count, 0)::bigint AS owner_available_count,
  COALESCE(e.available_bytes, 0)::bigint AS owner_available_bytes,
  COALESCE(e.preparing_count, 0)::bigint AS owner_preparing_count,
  COALESCE(e.preparing_bytes, 0)::bigint AS owner_preparing_bytes,
  COALESCE(e.uploading_count, 0)::bigint AS owner_uploading_count,
  COALESCE(e.uploading_bytes, 0)::bigint AS owner_uploading_bytes,
  COALESCE(e.job_count, 0)::bigint AS owner_job_count,
  COALESCE(e.job_bytes, 0)::bigint AS owner_job_bytes,
  COALESCE(e.reclaimable_bytes, 0)::bigint AS owner_reclaimable_bytes,
  COALESCE(e.refused_count, 0)::bigint AS owner_refused_count
FROM totals t LEFT JOIN examples e ON true
ORDER BY e.refused_count DESC, e.user_id;
