-- Warm-reserve surplus hibernation (issue #4395, PRD warm reserve). The pass in
-- workersvc/hibernation_pool.go lists candidates here, then decides each one under the owner
-- advisory lock from ONE fresh read of the owner's pool. All times are passed from Go (the
-- injectable service clock), never SQL now(). The status lists are literals on purpose.

-- name: ListHibernationPoolCandidates :many
-- Surplus candidates only, a hint read without locks: the pass re-decides every one under the
-- owner lock. A candidate is a persistent hosted, registered (template, size and docker fact all
-- known) awake worker with no non-terminal run whose surplus anchor (the GetWorkerIdleness
-- expression, kept text-identical so runs_worker_terminal_end_idx serves it) is at or before
-- @surplus_cutoff, and which has at least one other persistent hosted worker of the same owner
-- (a necessary condition of pool membership, so a lone worker is never a candidate). A worker
-- whose controller recently refused is skipped until @recent_cutoff, like
-- ListAutoHibernationCandidates. Oldest anchor first, then id; bounded by @max_rows.
SELECT w.id, w.user_id, a.anchor::timestamptz AS anchor
FROM workers w
CROSS JOIN LATERAL (
    SELECT GREATEST(
        (SELECT max(COALESCE(r.finished_at, r.status_since)) FROM runs r
          WHERE r.worker_id = w.id AND r.status IN ('completed', 'failed', 'cancelled')),
        w.last_wake_settled_at,
        w.created_at
    ) AS anchor
) a
WHERE w.kind = 'hosted' AND NOT w.ephemeral AND w.hibernation_state = 'none'
  AND w.template_declared IS NOT NULL AND w.hosted_size IS NOT NULL AND w.docker_enabled IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM runs r
      WHERE r.worker_id = w.id AND r.status NOT IN ('completed', 'failed', 'cancelled'))
  AND a.anchor <= @surplus_cutoff::timestamptz
  AND NOT COALESCE(w.hibernation_blocked_reason = 'controller_refused'
                   AND w.hibernation_blocked_at > @recent_cutoff::timestamptz, false)
  AND EXISTS (
      SELECT 1 FROM workers o
      WHERE o.user_id = w.user_id AND o.id <> w.id AND o.kind = 'hosted' AND NOT o.ephemeral)
ORDER BY a.anchor ASC, w.id ASC
LIMIT @max_rows::int;

-- name: ListWorkerOwners :many
-- The owner of each listed worker, so the pass can group the long-rule candidates (which
-- ListAutoHibernationCandidates returns without an owner) by owner. A hint read without locks.
SELECT id, user_id FROM workers WHERE id = ANY(@ids::uuid[]);

-- name: GetOwnerHibernationPoolInput :many
-- The whole pool decision input of one owner in ONE statement (one snapshot): every persistent
-- hosted worker of @owner with the pool key fields, the effective capabilities (the shared
-- fn_effective_worker_caps fold, as the claim gate computes it), the state, intake, readiness
-- and cleanup columns, the non-terminal run count, the surplus anchor (the GetWorkerIdleness
-- expression), and whether a queued run holds a pending demand wake on the worker. It MUST be a
-- separate statement executed AFTER the owner lock and the candidate row lock, never folded into
-- the lock statement or taken from a list query: under READ COMMITTED a statement that waited on
-- a lock still reads its own fresh snapshot, so the decision sees every commit the previous
-- lock holder made.
SELECT
    w.id, w.user_id, w.kind, w.ephemeral, w.status, w.last_heartbeat_at, w.created_at,
    w.hibernation_state, w.wake_error,
    w.template_declared, w.hosted_size, w.isolated_lane, w.docker_enabled,
    fn_effective_worker_caps(w.capabilities, COALESCE(w.docker_enabled, false))::text[] AS effective_caps,
    w.protocol_capabilities, w.max_concurrent_runs, w.max_cross_check_slots,
    w.manual_cordoned_at, w.draining_since, w.maintenance_fenced, w.maintenance_phase,
    w.pending_overflow_until, w.cleanup_phase, w.memory_authority_lost_at,
    (SELECT count(*) FROM runs r
      WHERE r.worker_id = w.id AND r.status NOT IN ('completed', 'failed', 'cancelled'))::bigint AS nonterminal,
    GREATEST(
        (SELECT max(COALESCE(r.finished_at, r.status_since)) FROM runs r
          WHERE r.worker_id = w.id AND r.status IN ('completed', 'failed', 'cancelled')),
        w.last_wake_settled_at,
        w.created_at
    )::timestamptz AS anchor,
    EXISTS (
        SELECT 1 FROM hibernation_wakes hw
        JOIN runs r ON r.id = hw.run_id
        WHERE hw.worker_id = w.id AND r.status = 'queued'
    )::boolean AS pending_wake_demand
FROM workers w
WHERE w.user_id = @owner::uuid AND w.kind = 'hosted' AND NOT w.ephemeral
ORDER BY w.id ASC;
