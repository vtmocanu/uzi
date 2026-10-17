-- name: ListPolicyStopRevisionInputs :many
SELECT * FROM run_user_inputs
WHERE run_id = $1 AND kind = 'revise_plan'
  AND applied_at IS NULL AND settled_at IS NULL
  AND disposition IS DISTINCT FROM 'superseded'
  AND disposition IS DISTINCT FROM 'superseded_by_stop'
ORDER BY id;

-- name: GetPolicyStopCrossCheck :one
SELECT * FROM cross_checks WHERE id = $1;
