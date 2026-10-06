-- All mutations here require GetWorkerForUpdate in a transaction first.
-- name: SetDindMaintenance :one
UPDATE workers SET maintenance_id = @maintenance_id, maintenance_nonce = @maintenance_nonce,
 maintenance_phase = @maintenance_phase, maintenance_deployment_uid = @maintenance_deployment_uid,
 maintenance_pvc_uid = @maintenance_pvc_uid, maintenance_register_nonce = @maintenance_register_nonce,
 maintenance_fenced = @maintenance_fenced, maintenance_ready_ack = @maintenance_ready_ack,
 draining_since = CASE
   WHEN @maintenance_phase::text = 'requested' THEN COALESCE(draining_since, now())
   WHEN @maintenance_phase::text IN ('cancelled','complete') AND maintenance_owns_drain THEN NULL
   ELSE draining_since END,
 maintenance_owns_drain = CASE
   WHEN @maintenance_phase::text = 'requested' AND draining_since IS NULL THEN true
   WHEN @maintenance_phase::text IN ('cancelled','complete') THEN false
   ELSE maintenance_owns_drain END,
 maintenance_ack_at = CASE WHEN @maintenance_ready_ack::boolean THEN CASE WHEN @refresh_ready_ack::boolean THEN @custody_checked_at::timestamptz ELSE maintenance_ack_at END ELSE NULL END,
 maintenance_activity_floor = CASE WHEN @advance_activity_floor::boolean THEN now() ELSE maintenance_activity_floor END,
 dind_pressure_streak = CASE WHEN @reset_pressure::boolean THEN 0 ELSE dind_pressure_streak END,
 dind_meter_at = CASE WHEN @reset_pressure::boolean THEN NULL ELSE dind_meter_at END,
 dind_below_threshold = CASE WHEN @reset_pressure::boolean THEN false ELSE dind_below_threshold END,
 updated_at = now()
WHERE id = @id RETURNING *;

-- name: DindMaintenanceCustodyHeld :one
SELECT EXISTS (SELECT 1 FROM recovery_custody_holds WHERE live_worker_id = @worker_id AND state = 'open')::boolean;
