-- PRD #1809 M6 (D8): per-run disk size (worker_run_disk) and the park's checkpoint durability
-- flag (runs.checkpoint_contains_latest). Everything here is DISPLAY-ONLY: no claim,
-- scheduling, sweeper or custody query reads worker_run_disk or checkpoint_contains_latest.
--
-- Freshness: every reader ignores a worker_run_disk row whose sampled_at (the worker's
-- measurement time, not the heartbeat's arrival) is more than 25 minutes ago. A
-- heartbeat that omits run_disk (an older worker, a tick with no sample) leaves the worker's rows
-- in place, so the window is what retires a downgraded worker's last report.
--
-- The 25 minutes is coupled to the agent's sampler (agent/src/run-disk.ts, agent/src/config.ts):
-- a steady run's entry is re-measured every UZI_RUN_DISK_SAMPLE_INTERVAL (default 10m), a sample
-- may take up to its deadline (DEFAULT_SAMPLE_DEADLINE_MS, 4m) before it stamps sampled_at, and the
-- new reading reaches the api on the next heartbeat (WORKER_HEARTBEAT_INTERVAL, default 15s). A
-- live entry can therefore be about 14m15s old just before it is replaced: a 15-minute window left
-- under a minute of slack for a late heartbeat or clock skew; 25 minutes leaves about 10.
-- Raising the sample interval or deadline means widening this window in EVERY reader below
-- (ListLargestRunDiskForWorkers, GetRunDisk) together.

-- name: ReplaceWorkerRunDisk :exec
-- Replace one worker's reported per-run sizes with the set its latest heartbeat carried, in ONE
-- statement: rows for runs no longer listed are deleted and the listed ones are upserted. The
-- parallel arrays are the handler's validated, de-duplicated, capped list (parseWorkerStats).
-- Only run ids that exist AND belong to the worker's own owner are kept, so a garbled or hostile
-- id is dropped here instead of violating the runs FK. The DELETE and the INSERT touch disjoint
-- rows (listed vs not listed), so the two data-modifying parts never target the same row.
--
-- sampled_at is the MEASUREMENT time: when the worker's size walk finished, as the entry's
-- sampled_at carried it. A NULL element (an older worker that sends none) is stored as now().
-- The reported time is clamped to [now() - 24 hours, now() + 5 minutes] on the database clock
-- the freshness readers use, so a skewed or hostile worker clock can neither pin a row as fresh
-- far into the future nor store an absurd past (a row clamped to 24 hours ago is already stale).
WITH incoming AS (
    -- Ordinality-joined single-array unnests (sqlc cannot type a multi-argument unnest), the
    -- judge_bulk_disposition.sql shape: the five arrays are parallel and equal-length.
    SELECT ids.val AS run_id, hb.val AS home_bytes, cb.val AS cache_bytes, tr.val AS truncated,
           LEAST(GREATEST(COALESCE(sa.val, now()), now() - interval '24 hours'), now() + interval '5 minutes') AS sampled_at
    FROM unnest(@run_ids::uuid[]) WITH ORDINALITY AS ids(val, ord)
    JOIN unnest(@home_bytes::bigint[]) WITH ORDINALITY AS hb(val, ord) ON hb.ord = ids.ord
    JOIN unnest(@cache_bytes::bigint[]) WITH ORDINALITY AS cb(val, ord) ON cb.ord = ids.ord
    JOIN unnest(@truncated::boolean[]) WITH ORDINALITY AS tr(val, ord) ON tr.ord = ids.ord
    JOIN unnest(@sampled_at::timestamptz[]) WITH ORDINALITY AS sa(val, ord) ON sa.ord = ids.ord
    JOIN runs r ON r.id = ids.val AND r.user_id = @user_id::uuid
), deleted AS (
    DELETE FROM worker_run_disk d
    WHERE d.worker_id = @worker_id::uuid
      AND NOT EXISTS (SELECT 1 FROM incoming WHERE incoming.run_id = d.run_id)
)
INSERT INTO worker_run_disk (worker_id, run_id, home_bytes, cache_bytes, truncated, sampled_at)
SELECT @worker_id::uuid, incoming.run_id, incoming.home_bytes, incoming.cache_bytes, incoming.truncated, incoming.sampled_at
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
      AND d.sampled_at > now() - interval '25 minutes'
) ranked
WHERE ranked.rn <= @per_worker::int
ORDER BY ranked.worker_id, ranked.home_bytes DESC, ranked.run_id;

-- name: GetRunDisk :one
-- One run's fresh size row as reported by its CURRENT worker (runs.worker_id), for the run DTO.
-- A run with no current worker (NULL) or whose worker has not reported it within 25 minutes gets
-- pgx.ErrNoRows and shows no size: a row another worker reported earlier describes a HOME that
-- worker held, not necessarily where the run's work is now, so it is never substituted.
SELECT d.worker_id, d.run_id, d.home_bytes, d.cache_bytes, d.truncated, d.sampled_at
FROM worker_run_disk d
WHERE d.run_id = @run_id
  AND d.worker_id = @worker_id
  AND d.sampled_at > now() - interval '25 minutes';

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
