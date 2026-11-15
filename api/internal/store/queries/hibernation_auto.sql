-- Automatic hibernation of idle workers (issue #4207). The pass in
-- workersvc/hibernation_auto.go lists candidates here, then re-checks each one on the locked
-- row. All times are passed from Go (the injectable service clock), never SQL now().

-- name: GetWorkerIdleness :one
-- A worker's idleness in ONE statement: the count of runs still assigned (any non-terminal
-- status: queued, paused and every wait included) and the anchor, the latest of the newest
-- terminal run's finish (status_since when finished_at is unset), the last settled wake and
-- the worker's creation. It MUST stay one statement: a run finishing takes no worker lock,
-- and under READ COMMITTED each statement has its own snapshot, so two separate reads could
-- see a run as already gone from the count and not yet present in the anchor, which would
-- read a worker that just finished work as long idle. One statement shares one snapshot.
SELECT
    (SELECT count(*) FROM runs r
      WHERE r.worker_id = w.id AND r.status NOT IN ('completed', 'failed', 'cancelled'))::bigint AS nonterminal,
    GREATEST(
        (SELECT max(COALESCE(r.finished_at, r.status_since)) FROM runs r
          WHERE r.worker_id = w.id AND r.status IN ('completed', 'failed', 'cancelled')),
        w.last_wake_settled_at,
        w.created_at
    )::timestamptz AS anchor
FROM workers w
WHERE w.id = @worker_id;

-- name: ListAutoHibernationCandidates :many
-- Candidates only: the pass re-checks every predicate on the locked row. Idle means no
-- non-terminal run is assigned and the anchor (see GetWorkerIdleness) is at or before
-- @cutoff. A worker whose controller recently refused is skipped until @recent_cutoff. Order:
-- never or long-ago blocked workers first, oldest anchor first, so a pile of recently
-- blocked workers cannot starve a clean one; recently blocked ones go last, least recently
-- blocked first.
SELECT w.id, a.anchor::timestamptz AS anchor
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
  AND NOT EXISTS (
      SELECT 1 FROM runs r
      WHERE r.worker_id = w.id AND r.status NOT IN ('completed', 'failed', 'cancelled'))
  AND a.anchor <= @cutoff::timestamptz
  AND NOT COALESCE(w.hibernation_blocked_reason = 'controller_refused'
                   AND w.hibernation_blocked_at > @recent_cutoff::timestamptz, false)
ORDER BY COALESCE(w.hibernation_blocked_at > @recent_cutoff::timestamptz, false) ASC,
         CASE WHEN COALESCE(w.hibernation_blocked_at > @recent_cutoff::timestamptz, false)
              THEN w.hibernation_blocked_at END ASC NULLS LAST,
         a.anchor ASC, w.id ASC
LIMIT @max_rows::int;

-- name: SetWorkerHibernationBlocked :exec
-- Record why the automatic pass could not admit a worker. The caller holds the row lock.
-- hibernation_blocked_since restarts only when the reason changes, so it says how long one
-- reason has held; hibernation_blocked_at is the latest sighting.
UPDATE workers SET
    hibernation_blocked_since = CASE
        WHEN hibernation_blocked_reason IS DISTINCT FROM @reason::text THEN @at::timestamptz
        ELSE hibernation_blocked_since END,
    hibernation_blocked_reason = @reason::text,
    hibernation_blocked_at = @at::timestamptz
WHERE id = @id;

-- name: ListAutoHibernationBlockedIdle :many
-- fleet.hibernation (admin health): idle persistent hosted workers the automatic pass could
-- not hibernate. The worker and idleness predicates repeat ListAutoHibernationCandidates
-- (same non-terminal NOT EXISTS text, so runs_worker_nonterminal_idx serves it). A recorded
-- block counts only when it was seen at or after the anchor (hibernation_blocked_at >= anchor),
-- so a stale reason from before the worker's latest activity is ignored. total is the exact
-- number of matching workers across the window, not the LIMIT row count. The set is disjoint
-- from ListAutoHibernationParkedBlockers (this one has no non-terminal run, that one has at
-- least one). @cutoff comes from Go (the injectable clock).
SELECT w.id, w.name, a.anchor::timestamptz AS anchor,
       w.hibernation_blocked_reason::text AS reason,
       w.hibernation_blocked_since AS blocked_since,
       (count(*) OVER ())::bigint AS total
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
  AND NOT EXISTS (
      SELECT 1 FROM runs r
      WHERE r.worker_id = w.id AND r.status NOT IN ('completed', 'failed', 'cancelled'))
  AND a.anchor <= @cutoff::timestamptz
  AND w.hibernation_blocked_reason IS NOT NULL
  AND w.hibernation_blocked_at >= a.anchor
ORDER BY a.anchor ASC, w.id ASC
LIMIT @max_rows::int;

-- name: ListAutoHibernationParkedBlockers :many
-- fleet.hibernation (admin health): persistent hosted workers whose every assigned run is
-- parked (recovery_wait, paused or limit_wait), with the oldest parked run past @cutoff. The
-- automatic pass cannot hibernate them while a run is assigned. One row per worker: the
-- oldest parked run (status_since, then id). total is the exact number of matching workers
-- across the window, not the LIMIT row count. Disjoint from ListAutoHibernationBlockedIdle.
SELECT w.id, w.name, p.run_id, p.status::text AS status, p.status_since::timestamptz AS status_since,
       (count(*) OVER ())::bigint AS total
FROM workers w
CROSS JOIN LATERAL (
    SELECT r.id AS run_id, r.status, r.status_since
    FROM runs r
    WHERE r.worker_id = w.id AND r.status NOT IN ('completed', 'failed', 'cancelled')
    ORDER BY r.status_since ASC NULLS LAST, r.id ASC
    LIMIT 1
) p
WHERE w.kind = 'hosted' AND NOT w.ephemeral AND w.hibernation_state = 'none'
  AND NOT EXISTS (
      SELECT 1 FROM runs r
      WHERE r.worker_id = w.id AND r.status NOT IN ('completed', 'failed', 'cancelled')
        AND r.status NOT IN ('recovery_wait', 'paused', 'limit_wait'))
  AND p.status_since <= @cutoff::timestamptz
ORDER BY p.status_since ASC, w.id ASC
LIMIT @max_rows::int;
