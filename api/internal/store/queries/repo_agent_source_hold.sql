-- name: SetRunRepoAgentSourceHold :one
UPDATE runs SET
    status             = 'paused',
    status_since       = now(),
    hold_reason        = 'repo_agent_source_unavailable',
    hold_captured_head = NULL,
    open_question_id   = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'running'
  AND claim_released_at IS NULL
  AND claim_generation = @claim_generation::bigint
RETURNING *;
