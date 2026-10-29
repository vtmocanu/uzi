-- PRD #3454: instance-wide intake pause ("maintenance mode"). The claim SQL reads the row
-- through fn_instance_intake_paused(); these queries serve routes, readouts and transitions.

-- name: GetInstanceIntakePause :one
SELECT * FROM instance_intake_pause WHERE id;

-- name: LockInstanceIntakePause :one
-- Serializes concurrent transitions; the claim path never takes this lock.
SELECT * FROM instance_intake_pause WHERE id FOR UPDATE;

-- name: EnsureInstanceIntakePauseRow :execrows
-- Re-seeds a deleted singleton (inactive) so a transition can proceed and lift the
-- fail-closed pause a missing row implies. Returns 1 when it re-created the row, so the
-- caller can audit lifting a missing singleton (effective state paused to unpaused).
INSERT INTO instance_intake_pause (id) VALUES (true) ON CONFLICT (id) DO NOTHING;

-- name: UpdateInstanceIntakePause :one
-- Right-hand column references are the row's pre-update values. since is stamped here
-- (transaction now()) only on off to on, kept on a reason-only edit, cleared on off.
-- keep_attribution leaves by_user_id/by_label untouched (a reason-only edit).
UPDATE instance_intake_pause SET
    active     = @active::boolean,
    since      = CASE WHEN NOT @active::boolean THEN NULL
                      WHEN NOT active THEN now()
                      ELSE since END,
    by_user_id = CASE WHEN @keep_attribution::boolean THEN by_user_id ELSE sqlc.narg('by_user_id')::uuid END,
    by_label   = CASE WHEN @keep_attribution::boolean THEN by_label ELSE sqlc.narg('by_label')::text END,
    reason     = sqlc.narg('reason')::text
WHERE id
RETURNING *;

-- name: InsertInstanceIntakePauseEvent :exec
INSERT INTO instance_intake_pause_events (by_user_id, by_label, action, reason)
VALUES (sqlc.narg('by_user_id')::uuid, sqlc.narg('by_label')::text, @action::text, sqlc.narg('reason')::text);

-- name: ListInstanceIntakePauseEvents :many
SELECT * FROM instance_intake_pause_events ORDER BY at DESC, id DESC LIMIT @row_limit::int;

-- name: RunAdmittableForEphemeralProvision :one
-- PRD #3454: the ephemeral provisioner's per-candidate recheck, run inside provisionOne's
-- transaction right after the advisory lock. Plain snapshot read: NO row lock (a FOR UPDATE /
-- FOR SHARE here could deadlock against the lease path, see the 40P01 note in workersvc/ephemeral_lease.go). false means
-- intake is paused for this run or its Codex account admission deadline is still ahead;
-- no row means the run is gone. The deadline includes recovery spread.
SELECT ((NOT (SELECT fn_instance_intake_paused()) OR fn_run_admitted_during_intake_pause(r))
        AND COALESCE(codex_admission_run_eligible_at(r.id) <= now(), true)
        AND fn_forge_origin_provision_allowed(r, now()))::boolean AS admittable
FROM runs r
WHERE r.id = @run_id;

-- PRD #3454 maintenance readout. Run in ONE REPEATABLE READ READ ONLY transaction
-- (store.ReadMaintenanceReadout) so counts and lists share a snapshot. Every non-terminal
-- status lands in exactly one of active / held / queued; custody is a separate, overlapping
-- view of open recovery holds. Each list query returns the exact total in total (window
-- count before LIMIT) and its oldest 50 rows.

-- name: MaintenanceSnapshotNow :one
SELECT now()::timestamptz AS as_of;

-- name: ListMaintenanceActiveRuns :many
SELECT r.id AS run_id, r.kind, r.status,
       COALESCE(NULLIF(u.display_name, ''), u.email, '')::text AS owner_label,
       rp.path_with_namespace AS repo, r.status_since AS since,
       count(*) OVER ()::bigint AS total
FROM runs r
LEFT JOIN users u ON u.id = r.user_id
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.status IN ('claimed', 'running')
ORDER BY r.status_since, r.id
LIMIT 50;

-- name: ListMaintenanceHeldRuns :many
-- Held: parked on a non-claim status, or queued but already admitted (a requeued run or a
-- cross_check child still claims during the pause). tag needs_owner = only the owner can move it.
SELECT r.id AS run_id, r.kind, r.status,
       COALESCE(NULLIF(u.display_name, ''), u.email, '')::text AS owner_label,
       rp.path_with_namespace AS repo, r.status_since AS since,
       (CASE WHEN (r.status = 'awaiting_approval' AND NOT r.auto_approve)
                  OR r.status IN ('awaiting_input', 'paused')
                  OR (r.status = 'recovery_wait'
                      AND r.recovery_wait_cause IN ('worker_requeue_exhausted', 'worker_memory_pressure', 'retained_recovery_adoption_failed'))
                  -- #3495: a capped retained-recovery park (NULL retry stamp) waits for owner
                  -- Resume; a stamped one is an automatic retry still progressing.
                  OR (r.status = 'recovery_wait'
                      AND r.recovery_wait_cause IN ('retained_recovery_exhausted', 'codex_idle_stall')
                      AND r.recovery_retry_not_before IS NULL)
             THEN 'needs_owner' ELSE 'progressing' END)::text AS tag,
       count(*) OVER ()::bigint AS total
FROM runs r
LEFT JOIN users u ON u.id = r.user_id
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.status IN ('awaiting_approval', 'awaiting_input', 'awaiting_followup', 'paused',
                   'limit_wait', 'recovery_wait', 'pool_wait')
   OR (r.status = 'queued' AND fn_run_admitted_during_intake_pause(r))
ORDER BY r.status_since, r.id
LIMIT 50;

-- name: ListMaintenanceCustodyRuns :many
-- One item per run with an OPEN custody hold (terminal runs included), from the oldest open
-- hold. tag needs_owner exactly when any of the run's open holds has decision_needed.
WITH open_holds AS (
    SELECT h.id AS hold_id, h.run_id, h.user_id, h.repo_id, h.created_at, f.decision_needed
    FROM recovery_custody_hold_facts f
    JOIN recovery_custody_holds h ON h.id = f.id
    -- h.state repeats the filter so the partial open-holds index applies to this second
    -- reference too (f.state is h.state in the view) instead of a full scan of the table.
    WHERE f.state = 'open' AND h.state = 'open'
), per_run AS (
    SELECT DISTINCT ON (run_id) hold_id, run_id, user_id, repo_id, created_at,
           bool_or(decision_needed) OVER (PARTITION BY run_id) AS needs_owner
    FROM open_holds
    ORDER BY run_id, created_at, hold_id
)
SELECT p.run_id, COALESCE(r.kind, '')::text AS kind, COALESCE(r.status, '')::text AS status,
       COALESCE(NULLIF(u.display_name, ''), u.email, '')::text AS owner_label,
       rp.path_with_namespace AS repo, p.created_at AS since,
       (CASE WHEN p.needs_owner THEN 'needs_owner' ELSE 'progressing' END)::text AS tag,
       p.hold_id,
       count(*) OVER ()::bigint AS total
FROM per_run p
LEFT JOIN runs r ON r.id = p.run_id
LEFT JOIN users u ON u.id = p.user_id
LEFT JOIN repos rp ON rp.id = p.repo_id
ORDER BY p.created_at, p.run_id
LIMIT 50;

-- name: ListMaintenanceQueuedRuns :many
-- Informational: new roots that will not claim until the pause lifts.
SELECT r.id AS run_id, r.kind, r.status,
       COALESCE(NULLIF(u.display_name, ''), u.email, '')::text AS owner_label,
       rp.path_with_namespace AS repo, r.status_since AS since,
       count(*) OVER ()::bigint AS total
FROM runs r
LEFT JOIN users u ON u.id = r.user_id
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.status = 'queued' AND NOT fn_run_admitted_during_intake_pause(r)
ORDER BY r.status_since, r.id
LIMIT 50;

-- name: ListMaintenanceIdleBoundWorkers :many
-- Informational: ephemeral workers bound to a queued run the pause will not admit.
SELECT w.id AS worker_id,
       COALESCE(NULLIF(u.display_name, ''), u.email, '')::text AS owner_label,
       r.id AS run_id, w.created_at AS since,
       count(*) OVER ()::bigint AS total
FROM workers w
JOIN runs r ON r.id = w.ephemeral_run_id
LEFT JOIN users u ON u.id = w.user_id
WHERE w.ephemeral AND r.status = 'queued' AND NOT fn_run_admitted_during_intake_pause(r)
ORDER BY w.created_at, w.id
LIMIT 50;

-- name: ListRunQueueReasons :many
-- PRD #3454: the derived queue_reason for a set of runs, evaluated at read time so it never lags
-- the pause. A queued run that is not admitted while intake is paused reads "maintenance"; PRD
-- #3522: a queued run carrying a weekly-reserve hold marker otherwise reads "weekly_reserve"
-- (maintenance wins when both apply). Every other id is absent.
SELECT r.id AS run_id,
       (CASE WHEN (SELECT fn_instance_intake_paused()) AND NOT fn_run_admitted_during_intake_pause(r)
             THEN 'maintenance' ELSE 'weekly_reserve' END)::text AS queue_reason
FROM runs r
WHERE r.id = ANY(@run_ids::uuid[])
  AND r.status = 'queued'
  AND (((SELECT fn_instance_intake_paused()) AND NOT fn_run_admitted_during_intake_pause(r))
       OR r.reserve_hold_reason IS NOT NULL);

-- name: ListRunAdmissionQueueReasons :many
-- Read-time precedence: maintenance, then the PRD #3522 weekly reserve, then Codex account
-- admission; owner admission details remain available independently.
SELECT r.id AS run_id,
       (CASE WHEN (SELECT fn_instance_intake_paused()) AND NOT fn_run_admitted_during_intake_pause(r)
             THEN 'maintenance'
             WHEN r.reserve_hold_reason IS NOT NULL THEN 'weekly_reserve'
             WHEN admission.codex_admission_class = 'rate_limit' THEN 'codex_rate_limit'
             WHEN admission.codex_admission_class = 'overload' THEN 'codex_overload'
             ELSE '' END)::text AS queue_reason,
       admission.codex_admission_class, admission.codex_admission_eligible_at
FROM runs r
LEFT JOIN codex_queued_run_admission admission ON admission.run_id = r.id AND admission.user_id = r.user_id AND admission.user_id = @viewer_id::uuid
WHERE r.id = ANY(@run_ids::uuid[]) AND r.status = 'queued'
  AND (admission.run_id IS NOT NULL
       OR r.reserve_hold_reason IS NOT NULL
       OR ((SELECT fn_instance_intake_paused()) AND NOT fn_run_admitted_during_intake_pause(r)));
