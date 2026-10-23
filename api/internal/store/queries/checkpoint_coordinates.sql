-- name: ResolveCheckpointCoordinates :one
SELECT fn_checkpoint_coordinates(r)::jsonb AS coordinates FROM runs r WHERE r.id = @run_id;

-- name: EnrollCheckpointCoordinates :one
SELECT fn_enroll_checkpoint_coordinates(@run_id::uuid, @worker_id::uuid, @generation::bigint)::jsonb AS coordinates;

-- name: GetClaimCheckpointRecovery :one
SELECT r.checkpoint_tip, r.checkpoint_attested_source,
       EXISTS (SELECT 1 FROM checkpoint_publish_attempts a WHERE a.run_id = r.id)::boolean AS checkpoint_write_unresolved
FROM runs r WHERE r.id = @run_id;

-- name: OptInCheckpointCoordinates :exec
SELECT set_config('uzi.task_checkpoint_coordinates_api', 'v1', true);
