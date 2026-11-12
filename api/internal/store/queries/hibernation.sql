-- Worker hibernation (PRD #4205 M2). Every query here runs inside a transaction that
-- already holds GetWorkerForUpdate on the worker row; the Go state machine in
-- workersvc/hibernation.go reads the locked row, decides, and writes the whole
-- hibernation column set back with WriteWorkerHibernation.

-- name: WriteWorkerHibernation :one
-- Persist the complete hibernation column set of one worker. The caller holds the row
-- lock; the CHECK constraints from migration 00427 still guard coherence.
UPDATE workers SET
    hibernation_state = @hibernation_state::text,
    hibernation_op_id = sqlc.narg('hibernation_op_id'),
    hibernation_requested_at = sqlc.narg('hibernation_requested_at'),
    hibernated_at = sqlc.narg('hibernated_at'),
    wake_requested_at = sqlc.narg('wake_requested_at'),
    wake_error = sqlc.narg('wake_error'),
    hibernation_reason = sqlc.narg('hibernation_reason'),
    hibernation_admission_pod_uid = sqlc.narg('hibernation_admission_pod_uid'),
    hibernation_scaling_down_at = sqlc.narg('hibernation_scaling_down_at'),
    wake_ready_op_id = sqlc.narg('wake_ready_op_id'),
    wake_ready_pod_uid = sqlc.narg('wake_ready_pod_uid'),
    wake_ready_resource_version = sqlc.narg('wake_ready_resource_version'),
    wake_register_op_id = sqlc.narg('wake_register_op_id'),
    wake_register_pod_uid = sqlc.narg('wake_register_pod_uid')
WHERE id = @id
RETURNING *;

-- name: InsertWorkerHibernationEvent :exec
INSERT INTO worker_hibernation_events (owner_id, worker_id, actor_id, actor_label, action, op_id, reason, occurred_at)
VALUES (@owner_id, @worker_id, @actor_id, @actor_label, @action, @op_id, sqlc.narg('reason')::text, @occurred_at);

-- name: WorkerHasOpenCustodyHold :one
-- The same predicate as ListHostedWorkersForController's custody_held column.
SELECT EXISTS (
    SELECT 1 FROM recovery_custody_holds h
    WHERE h.live_worker_id = @worker_id AND h.state = 'open'
)::boolean;

-- name: GetControllerHibernationAdvertisement :one
SELECT hibernation_version, hibernation_advertised_at, poll_interval_seconds
FROM controller_report_status WHERE id = 1;

-- name: UpsertControllerHibernationAdvertisement :exec
-- Record the controller's hibernation protocol version and poll interval from a status
-- report, stamped with the api's own receipt clock. Admission reads it back to refuse a
-- hibernate when no controller that understands the feature has reported recently.
INSERT INTO controller_report_status (id, observed_at, hibernation_version, hibernation_advertised_at, poll_interval_seconds)
VALUES (1, @advertised_at, @hibernation_version, @advertised_at, @poll_interval_seconds)
ON CONFLICT (id) DO UPDATE SET
    hibernation_version = EXCLUDED.hibernation_version,
    hibernation_advertised_at = EXCLUDED.hibernation_advertised_at,
    poll_interval_seconds = EXCLUDED.poll_interval_seconds;

-- name: ListStalePreparingWorkerIDs :many
-- Candidates only: the pass re-checks the predicate on the locked row.
SELECT id FROM workers
WHERE hibernation_state = 'preparing' AND hibernation_requested_at < @cutoff
ORDER BY id;

-- name: ListWakingWorkerIDs :many
SELECT id FROM workers WHERE hibernation_state = 'waking' ORDER BY id;

-- name: ListWorkersToWakeOnDisable :many
-- Setting-disable pacing: at most @max_rows sleeping workers per pass, longest asleep first.
SELECT id FROM workers
WHERE hibernation_state IN ('hibernated', 'preparing')
ORDER BY hibernated_at NULLS LAST, id
LIMIT @max_rows;
