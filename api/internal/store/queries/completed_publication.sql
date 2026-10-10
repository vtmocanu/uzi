-- name: GetCompletedPublicationHold :one
SELECT * FROM recovery_custody_holds
WHERE run_id = @run_id AND user_id = @user_id AND original_worker_id = @worker_id
    AND generation = @generation AND inventory_guarded;

-- name: GetCompletedPublicationBinding :one
SELECT r.id AS repo_id, c.id AS connection_id, r.forge_project_id AS project_id,
    c.forge_type, c.base_url, c.token_ciphertext
FROM repos r JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = @repo_id AND c.user_id = @user_id;

-- name: LockCompletedPublicationBinding :one
SELECT r.id AS repo_id, c.id AS connection_id, r.forge_project_id AS project_id,
    c.forge_type, c.base_url
FROM repos r JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = @repo_id AND c.user_id = @user_id
FOR SHARE OF r, c;

-- name: ReleaseCompletedPublicationHold :execrows
UPDATE recovery_custody_holds h SET
    state = 'released', live_worker_id = NULL, live_run_id = NULL, released_at = now(),
    updated_at = now(), release_evidence = 'completed_publication',
    final_disposition = 'completed_publication', completed_publication_reason = NULL,
    completed_publication_receipt = h.completion_identity || jsonb_build_object(
        'observed_branch_head', @observed_branch_head::text)
WHERE h.id = @hold_id AND h.run_id = @run_id AND h.user_id = @user_id
    AND h.original_worker_id = @worker_id AND h.generation = @generation
    AND h.state = 'open' AND h.inventory_guarded
    AND h.live_worker_id = @worker_id AND h.live_run_id = @run_id
    AND h.completed_publication_receipt IS NULL AND h.completion_identity = @identity::jsonb
    AND EXISTS (SELECT 1 FROM runs r JOIN repos p ON p.id = r.repo_id
        JOIN forge_connections c ON c.id = p.connection_id
        WHERE r.id = h.run_id AND r.user_id = h.user_id AND r.status = 'completed'
            AND r.worker_id = h.original_worker_id AND r.claim_generation = h.generation
            AND r.claim_released_at IS NULL
            AND r.completion_final_head = h.completion_identity->>'final_head'
            AND r.repo_id::text = h.completion_identity->>'repo_id'
            AND p.connection_id::text = h.completion_identity->>'connection_id'
            AND p.forge_project_id::text = h.completion_identity->>'project_id'
            AND c.forge_type = h.completion_identity->>'forge_type'
            AND c.base_url = h.completion_identity->>'base_url'
            AND r.mr_iid::text = h.completion_identity->>'mr_iid'
            AND CASE WHEN r.kind = 'issue' THEN 'agent/issue-' || r.issue_iid::text
                WHEN r.kind = 'mr_rework' THEN r.pipeline_ref
                WHEN r.kind = 'self_improve' THEN 'uzi/self-improve/' || r.id::text END = h.completion_identity->>'branch');

-- name: RecordCompletedPublicationRefusal :execrows
UPDATE recovery_custody_holds SET completed_publication_reason = @reason, updated_at = now()
WHERE id = @hold_id AND run_id = @run_id AND user_id = @user_id
    AND original_worker_id = @worker_id AND generation = @generation
    AND state = 'open' AND completed_publication_receipt IS NULL
    AND completion_identity = @identity::jsonb;
