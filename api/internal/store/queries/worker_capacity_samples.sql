-- name: SampleWorkerCapacityBatch :one
-- Issue #4693: one batch of the worker_capacity_samples sweeper pass, as ONE autocommitted
-- statement. Returns scanned (the page size) and inserted, so the caller keeps going only
-- while a page came back full.
--
-- Observation instant: obs, bucket and stale_cutoff come from the single materialized CTE `clock`;
-- statement_timestamp() is constant within a statement, so they share one instant. Production passes a
-- NULL observed_at, which means statement_timestamp(), NOT now(): now() is the transaction
-- start, so an explicit enclosing transaction would date the sample before it was taken. A
-- non-NULL observed_at is a test seam only. bucket is a five-minute date_bin on a fixed
-- origin; stale_cutoff reuses MarkStaleWorkersOffline's staleness definition (heartbeat
-- strictly older than the cutoff is stale) from the stale_after interval (the caller passes whole microseconds),
-- so a fractional-second window is not truncated.
--
-- `page` is MATERIALIZED and carries its own LIMIT, applied BEFORE any occupancy work: the
-- anti-join keeps only workers with no sample in this bucket or later (>= bucket gives at most
-- one sample per bucket AND refuses to write an earlier bucket after a clock step backwards),
-- in worker-id order, so an unsampled worker is reached on a later batch of the same bucket
-- and a worker never reached is a gap. A cost bound, not a cursor: nothing is stored between
-- batches.
--
-- Occupancy counts runs bound to the worker in a slot-holding status, the same set
-- ListWorkersByUser counts: claimed, running, awaiting_approval, awaiting_input,
-- awaiting_followup. A pool_wait, recovery_wait or affinity-only queued row holds no slot and
-- is outside the set, so it neither counts nor affects attribution. Ordinary = not chat and
-- not on the cross-check lane (a cross-check on an ordinary slot is ordinary); cross-check
-- lane = cross_check_lane; chat = kind chat. Computed once per page row by one lateral
-- aggregate (runs_worker_nonterminal_idx, 00459).
--
-- Fail-closed attribution: 'owner' only when EVERY slot-holding run on the worker has the
-- worker's owner AND a proven lane shape, i.e. a kind in the audited allowlist (all ten
-- runkind kinds; TestWorkerCapacitySampleKindAllowlistMatchesRunkind keeps it in sync, so a new
-- kind fails the build instead of silently recording unavailable) with the cross-check lane
-- only on a cross_check run. The audit of every writer that can put a run in a slot status
-- found each is an owner-gated claim, a slot move that keeps worker_id, or a snapshot readoption
-- of a run the same worker claimed at the same generation (see the PRD #4693 Decision Log); the
-- gate below re-checks owner and lane on every slot-holding run rather than relying on that. A slot-holding run is proven on the isolated lane only when its egress-profile
-- binding matches the worker's isolated_lane flag, mirroring ClaimRun's two-way clause
-- ((r.egress_profile_id IS NOT NULL) = worker_isolated_lane): a profile-bound run on a
-- non-isolated worker, or an unprofiled run on an isolated worker, records 'unavailable'. Chat
-- is never profile-bound (runs_egress_profile_not_chat), so it never matches an isolated worker.
-- Anything else records 'unavailable' with NULL counts, never a guessed number. Caps, freshness,
-- heartbeat, kind, ephemeral and isolated_lane are always recorded.
WITH clock AS MATERIALIZED (
    SELECT COALESCE(sqlc.narg('observed_at')::timestamptz, statement_timestamp()) AS obs,
           date_bin('5 minutes', COALESCE(sqlc.narg('observed_at')::timestamptz, statement_timestamp()),
                    TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket,
           COALESCE(sqlc.narg('observed_at')::timestamptz, statement_timestamp())
               - @stale_after::interval AS stale_cutoff
), page AS MATERIALIZED (
    SELECT w.id, w.user_id, w.status, w.last_heartbeat_at, w.kind, w.ephemeral, w.isolated_lane,
           w.max_concurrent_runs, w.max_cross_check_slots
    FROM workers w, clock c
    WHERE NOT EXISTS (
        SELECT 1 FROM worker_capacity_samples s
        WHERE s.worker_id = w.id AND s.bucket_start >= c.bucket)
    ORDER BY w.id
    LIMIT @batch_size::int
), occupancy AS MATERIALIZED (
    SELECT p.id, o.ordinary_active, o.cross_check_lane_active, o.chat_active, o.unproven
    FROM page p
    LEFT JOIN LATERAL (
        SELECT count(*) FILTER (WHERE r.kind <> 'chat' AND NOT r.cross_check_lane) AS ordinary_active,
               count(*) FILTER (WHERE r.cross_check_lane) AS cross_check_lane_active,
               count(*) FILTER (WHERE r.kind = 'chat') AS chat_active,
               COALESCE(bool_or(
                   r.user_id <> p.user_id
                   OR ((r.egress_profile_id IS NOT NULL) <> p.isolated_lane)
                   OR NOT (
                       (r.cross_check_lane AND r.kind = 'cross_check')
                       OR (NOT r.cross_check_lane AND r.kind IN (
                           'issue', 'ci_fix', 'chat', 'judge', 'self_improve',
                           'prompt', 'task', 'mr_rework', 'job', 'cross_check')))), false) AS unproven
        FROM runs r
        WHERE r.worker_id = p.id
          AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
    ) o ON true
), ins AS (
    INSERT INTO worker_capacity_samples (
        worker_id, bucket_start, user_id, observed_at, freshness, last_heartbeat_at,
        worker_kind, ephemeral, isolated_lane, max_concurrent_runs, max_cross_check_slots,
        attribution, ordinary_active, cross_check_lane_active, chat_active)
    SELECT p.id, c.bucket, p.user_id, c.obs,
           CASE WHEN p.status = 'online' AND p.last_heartbeat_at >= c.stale_cutoff THEN 'online'
                WHEN p.status = 'online' THEN 'stale'
                ELSE 'offline' END,
           p.last_heartbeat_at, p.kind, p.ephemeral, p.isolated_lane,
           p.max_concurrent_runs, p.max_cross_check_slots,
           CASE WHEN oc.unproven THEN 'unavailable' ELSE 'owner' END,
           CASE WHEN oc.unproven THEN NULL ELSE oc.ordinary_active END,
           CASE WHEN oc.unproven THEN NULL ELSE oc.cross_check_lane_active END,
           CASE WHEN oc.unproven THEN NULL ELSE oc.chat_active END
    FROM page p
    JOIN occupancy oc ON oc.id = p.id
    CROSS JOIN clock c
    ON CONFLICT (worker_id, bucket_start) DO NOTHING
    RETURNING 1
)
SELECT (SELECT count(*) FROM page)::bigint AS scanned,
       (SELECT count(*) FROM ins)::bigint AS inserted;
