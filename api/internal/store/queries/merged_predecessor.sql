-- name: LockMergedPredecessorRun :one
SELECT id, user_id, repo_id, status, claim_generation, mr_iid
FROM runs WHERE id = @run_id FOR UPDATE;

-- name: LockMergedPredecessorBinding :one
SELECT p.id AS repo_id, p.connection_id, p.forge_project_id AS project_id,
    c.user_id AS owner_id, c.forge_type, c.base_url
FROM repos p JOIN forge_connections c ON c.id = p.connection_id
WHERE p.id = @repo_id
FOR SHARE OF p, c;

-- name: LockMergedPredecessorHolds :many
-- Discard changes no referenced key. NO KEY UPDATE excludes other disposers while
-- permitting an admitted upload's FK key-share check to finish before capture locking.
SELECT id, generation FROM recovery_custody_holds
WHERE run_id = @run_id AND user_id = @owner_id AND repo_id = @repo_id
    AND state = 'open' AND generation < @completed_generation
ORDER BY generation, id FOR NO KEY UPDATE;

-- name: LockMergedPredecessorNonReadyCaptures :many
SELECT id FROM recovery_captures
WHERE hold_id = @hold_id AND state IN ('preparing', 'uploading', 'needs_action')
ORDER BY id FOR UPDATE;

-- name: DiscardMergedPredecessorHold :execrows
UPDATE recovery_custody_holds SET
    state = 'discarded', live_worker_id = NULL, live_run_id = NULL,
    release_evidence = 'merged_predecessor_discard',
    merged_predecessor_discard_binding = @binding::jsonb, updated_at = now()
WHERE id = @hold_id AND run_id = @run_id AND user_id = @owner_id AND repo_id = @repo_id
    AND generation = @generation AND generation < @completed_generation AND state = 'open'
    AND merged_predecessor_discard_binding IS NULL;
