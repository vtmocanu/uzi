-- name: ListMergedPredecessorCandidates :many
-- OPEN exact-identity predecessor holds are the durable workset, including
-- historical merged caches and issue-less runs. One row per repository visit.
SELECT r.id AS run_id, r.user_id AS owner_id, p.id AS repo_id,
    p.connection_id, p.forge_project_id AS project_id, r.mr_iid::bigint AS mr_iid,
    r.claim_generation AS completed_generation, c.forge_type, c.base_url,
    c.token_ciphertext
FROM runs r
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id AND c.user_id = r.user_id
WHERE p.id = @repo_id AND p.enabled
    AND r.status = 'completed' AND r.mr_iid > 0 AND r.claim_generation > 0
    AND r.id > @after_run_id
    AND EXISTS (
        SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = r.id AND h.user_id = r.user_id AND h.repo_id = p.id
            AND h.state = 'open' AND h.generation < r.claim_generation
    )
ORDER BY r.id LIMIT 1;

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
ORDER BY generation, id LIMIT 1001 FOR NO KEY UPDATE;

-- name: LockMergedPredecessorNonReadyCaptures :many
SELECT id FROM recovery_captures
WHERE hold_id = @hold_id AND state IN ('preparing', 'uploading', 'needs_action')
ORDER BY id LIMIT @capture_limit::int FOR UPDATE;

-- name: DiscardMergedPredecessorHold :execrows
UPDATE recovery_custody_holds SET
    state = 'discarded', live_worker_id = NULL, live_run_id = NULL,
    release_evidence = 'merged_predecessor_discard',
    merged_predecessor_discard_binding = @binding::jsonb, updated_at = now()
WHERE id = @hold_id AND run_id = @run_id AND user_id = @owner_id AND repo_id = @repo_id
    AND generation = @generation AND generation < @completed_generation AND state = 'open'
    AND merged_predecessor_discard_binding IS NULL;
