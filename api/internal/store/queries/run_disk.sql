-- PRD #1809 M6 (D8): per-run disk size (worker_run_disk) and the park's checkpoint durability
-- flag (runs.checkpoint_contains_latest). Everything here is DISPLAY-ONLY: no claim,
-- scheduling, sweeper or custody query reads worker_run_disk or checkpoint_contains_latest.
--
-- Freshness: every reader ignores a worker_run_disk row sampled more than 15 minutes ago. A
-- heartbeat that omits run_disk (an older worker, a tick with no sample) leaves the worker's rows
-- in place, so the window is what retires a downgraded worker's last report.

-- name: ReplaceWorkerRunDisk :exec
-- Replace one worker's reported per-run sizes with the set its latest heartbeat carried, in ONE
-- statement: rows for runs no longer listed are deleted and the listed ones are upserted with a
-- fresh sampled_at. The parallel arrays are the handler's validated, de-duplicated, capped list
-- (parseWorkerStats). Only run ids that exist AND belong to the worker's own owner are kept, so a
-- garbled or hostile id is dropped here instead of violating the runs FK. The DELETE and the
-- INSERT touch disjoint rows (listed vs not listed), so the two data-modifying parts never
-- target the same row.
WITH incoming AS (
    -- Ordinality-joined single-array unnests (sqlc cannot type a multi-argument unnest), the
    -- judge_bulk_disposition.sql shape: the four arrays are parallel and equal-length.
    SELECT ids.val AS run_id, hb.val AS home_bytes, cb.val AS cache_bytes, tr.val AS truncated
    FROM unnest(@run_ids::uuid[]) WITH ORDINALITY AS ids(val, ord)
    JOIN unnest(@home_bytes::bigint[]) WITH ORDINALITY AS hb(val, ord) ON hb.ord = ids.ord
    JOIN unnest(@cache_bytes::bigint[]) WITH ORDINALITY AS cb(val, ord) ON cb.ord = ids.ord
    JOIN unnest(@truncated::boolean[]) WITH ORDINALITY AS tr(val, ord) ON tr.ord = ids.ord
    JOIN runs r ON r.id = ids.val AND r.user_id = @user_id::uuid
), deleted AS (
    DELETE FROM worker_run_disk d
    WHERE d.worker_id = @worker_id::uuid
      AND NOT EXISTS (SELECT 1 FROM incoming WHERE incoming.run_id = d.run_id)
)
INSERT INTO worker_run_disk (worker_id, run_id, home_bytes, cache_bytes, truncated, sampled_at)
SELECT @worker_id::uuid, incoming.run_id, incoming.home_bytes, incoming.cache_bytes, incoming.truncated, now()
FROM incoming
ON CONFLICT (worker_id, run_id) DO UPDATE SET
    home_bytes  = EXCLUDED.home_bytes,
    cache_bytes = EXCLUDED.cache_bytes,
    truncated   = EXCLUDED.truncated,
    sampled_at  = EXCLUDED.sampled_at;

-- name: ListLargestRunDiskForWorkers :many
-- The largest (by HOME bytes) fresh per-run rows of each listed worker, at most @per_worker
-- each: the worker DTO's run_disk overlay (top few) and the admin-health run-disk check (top
-- one). Batched over a worker-id set so a list endpoint reads every worker in one round-trip.
SELECT ranked.worker_id, ranked.run_id, ranked.home_bytes, ranked.cache_bytes, ranked.truncated, ranked.sampled_at
FROM (
    SELECT d.worker_id, d.run_id, d.home_bytes, d.cache_bytes, d.truncated, d.sampled_at,
           row_number() OVER (PARTITION BY d.worker_id ORDER BY d.home_bytes DESC, d.run_id) AS rn
    FROM worker_run_disk d
    WHERE d.worker_id = ANY(@worker_ids::uuid[])
      AND d.sampled_at > now() - interval '15 minutes'
) ranked
WHERE ranked.rn <= @per_worker::int
ORDER BY ranked.worker_id, ranked.home_bytes DESC, ranked.run_id;

-- name: GetRunDisk :one
-- One run's latest fresh size row for the run DTO: the row its current worker reported when
-- there is one, else the most recently sampled row from any worker (a parked run whose worker
-- binding moved still shows the size its last worker saw). pgx.ErrNoRows when nothing fresh.
SELECT d.worker_id, d.run_id, d.home_bytes, d.cache_bytes, d.truncated, d.sampled_at
FROM worker_run_disk d
WHERE d.run_id = @run_id
  AND d.sampled_at > now() - interval '15 minutes'
ORDER BY (d.worker_id = sqlc.narg('current_worker_id')::uuid) DESC NULLS LAST, d.sampled_at DESC
LIMIT 1;

-- name: SetRunCheckpointContainsLatest :execrows
-- Record whether the checkpoint a park published contains the run's latest committed work
-- (PRD #1809 M6, D8). Called after a park transition APPLIED, and guarded on the run still
-- being owned by the reporting worker in the status that park wrote, so a run that already moved
-- on (re-queued, resumed, cancelled) is left alone. NULL is "not reported": every park writes
-- the report as carried, including NULL from a worker that omits it, so the flag always
-- describes the latest park and never an older one. Display-only: custody ignores it.
UPDATE runs SET
    checkpoint_contains_latest = sqlc.narg('checkpoint_contains_latest')
WHERE id = @id
  AND worker_id = @worker_id
  AND status = @status;
