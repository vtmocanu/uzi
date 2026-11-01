-- name: ListMergedPredecessorCandidates :many
-- OPEN exact-identity predecessor holds are the durable workset, including
-- historical merged caches and issue-less runs. One row per repository visit.
SELECT r.id AS run_id, r.user_id AS owner_id, p.id AS repo_id,
    p.connection_id, p.forge_project_id AS project_id, r.mr_iid::bigint AS mr_iid,
    r.claim_generation AS completed_generation, c.forge_type, c.base_url,
    c.token_ciphertext, r.kind, r.issue_iid, r.created_at,
    (EXISTS (
        SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = r.id AND h.user_id = r.user_id AND h.repo_id = p.id
            AND h.state = 'open' AND h.generation < r.claim_generation
    ))::boolean AS original_branch
FROM runs r
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id AND c.user_id = r.user_id
WHERE p.id = @repo_id AND p.enabled
    AND r.status = 'completed' AND r.mr_iid > 0 AND r.claim_generation > 0
    AND r.id > @after_run_id
    AND (EXISTS (
        SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = r.id AND h.user_id = r.user_id AND h.repo_id = p.id
            AND h.state = 'open' AND h.generation < r.claim_generation
    ) OR (r.kind = 'issue' AND r.issue_iid > 0
        AND isfinite(r.created_at) AND EXISTS (
        SELECT 1 FROM runs a WHERE a.user_id = r.user_id AND a.repo_id = p.id AND a.issue_iid = r.issue_iid
            AND a.kind = 'issue' AND a.issue_iid > 0
            AND a.status IN ('failed', 'cancelled') AND a.claim_generation > 0
            AND a.mr_iid IS NULL AND a.fail_origin IS DISTINCT FROM 'run_timeout'
            AND isfinite(a.finished_at) AND a.finished_at < r.created_at
            AND NOT EXISTS (SELECT 1 FROM worker_active_runs w WHERE w.run_id = a.id)
            AND EXISTS (
                SELECT 1 FROM recovery_custody_holds h
                WHERE h.run_id = a.id AND h.user_id = a.user_id AND h.repo_id = a.repo_id
                    AND h.state = 'open'
            )
    )))
ORDER BY r.id LIMIT 1;


-- name: ListCrossRunTargets :many
-- The issue history index supplies descending creation order; UUID breaks ties.
-- Only immutable keys drive continuation, never a mutable completion timestamp.
SELECT a.id AS run_id, a.user_id AS owner_id, a.repo_id, a.kind, a.status,
    a.claim_generation AS generation, a.issue_iid, a.finished_at, a.created_at
FROM runs a
WHERE a.repo_id = @repo_id AND a.user_id = @owner_id
    AND a.issue_iid = @issue_iid AND a.issue_iid > 0 AND a.kind = 'issue'
    AND a.id <> @witness_run_id
    AND a.status IN ('failed', 'cancelled') AND a.claim_generation > 0
    AND a.mr_iid IS NULL AND a.fail_origin IS DISTINCT FROM 'run_timeout'
    AND isfinite(a.finished_at) AND a.finished_at < @witness_created_at::timestamptz
    AND NOT EXISTS (SELECT 1 FROM worker_active_runs w WHERE w.run_id = a.id)
    AND EXISTS (
        SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = a.id AND h.user_id = a.user_id AND h.repo_id = a.repo_id
            AND h.state = 'open'
    )
    AND (sqlc.narg('after_created_at')::timestamptz IS NULL
        OR (a.created_at, a.id) < (sqlc.narg('after_created_at')::timestamptz, @after_run_id::uuid))
ORDER BY a.created_at DESC, a.id DESC LIMIT 1;

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

-- name: LockCrossRunTarget :one
-- The caller orders target/witness run locks by ascending UUID.
SELECT id, user_id, repo_id, kind, status, claim_generation, issue_iid, mr_iid,
    fail_origin, finished_at, created_at
FROM runs WHERE id = @run_id FOR UPDATE;

-- name: LockCrossRunWitness :one
SELECT id, user_id, repo_id, kind, status, claim_generation, issue_iid, mr_iid,
    fail_origin, finished_at, created_at
FROM runs WHERE id = @run_id FOR SHARE;

-- name: HasCrossRunLiveClaim :one
SELECT EXISTS (SELECT 1 FROM worker_active_runs WHERE run_id = @run_id);

-- name: HasCrossRunLiveConsumer :one
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE user_id = @owner_id AND repo_id = @repo_id
        AND status NOT IN ('completed', 'failed', 'cancelled')
        AND (issue_iid = @issue_iid OR continued_issue_iid = @issue_iid)
);

-- name: LockCrossRunInventory :many
-- Include every state and identity: closed or anomalous siblings must be classified
-- under the same disposer lock as open holds, before locking publications/captures.
SELECT * FROM recovery_custody_holds WHERE run_id = @run_id
ORDER BY generation, id LIMIT 1001 FOR NO KEY UPDATE;

-- name: LockCrossRunPublications :many
SELECT pub.* FROM run_held_publications pub
WHERE pub.run_id = @run_id OR pub.hold_id IN (
    SELECT id FROM recovery_custody_holds WHERE run_id = @run_id
) OR pub.id IN (
    SELECT final_publication_id FROM recovery_custody_holds WHERE run_id = @run_id
)
ORDER BY pub.generation, pub.id LIMIT 1001 FOR NO KEY UPDATE;

-- name: ListCrossRunFinalCaptures :many
-- Identity only: historical FINALs do not depend on current archive availability.
SELECT c.* FROM recovery_captures c
WHERE c.id IN (SELECT h.final_capture_id FROM recovery_custody_holds h WHERE h.run_id = @run_id)
ORDER BY c.id LIMIT 1001;

-- name: HasCrossRunSharedFinal :one
SELECT EXISTS (
    SELECT 1 FROM recovery_custody_holds other
    WHERE other.run_id <> @run_id AND (
        other.live_run_id = @run_id
        OR other.final_capture_id IN (
            SELECT c.id FROM recovery_captures c
            JOIN recovery_custody_holds own ON own.id = c.hold_id
            WHERE own.run_id = @run_id)
        OR other.final_capture_id IN (
            SELECT final_capture_id FROM recovery_custody_holds WHERE run_id = @run_id)
        OR other.final_publication_id IN (
            SELECT pub.id FROM run_held_publications pub
            WHERE pub.run_id = @run_id OR pub.hold_id IN (
                SELECT id FROM recovery_custody_holds WHERE run_id = @run_id)
                OR pub.id IN (
                    SELECT final_publication_id FROM recovery_custody_holds WHERE run_id = @run_id)))
);

-- name: DiscardCrossRunHold :execrows
UPDATE recovery_custody_holds SET
    state = 'discarded', live_worker_id = NULL, live_run_id = NULL,
    release_evidence = 'merged_predecessor_discard',
    merged_predecessor_discard_binding = @binding::jsonb, updated_at = now()
WHERE id = @hold_id AND run_id = @run_id AND user_id = @owner_id AND repo_id = @repo_id
    AND generation = @generation AND generation > 0 AND generation <= @terminal_generation
    AND state = 'open' AND merged_predecessor_discard_binding IS NULL;
