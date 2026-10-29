-- +goose Up
-- Placement consumes existing immutable FINAL evidence; this grants no custody
-- release, source adoption or deletion authority.
-- +goose StatementBegin
CREATE FUNCTION fn_forge_source_history_safe(r runs) RETURNS boolean LANGUAGE sql STABLE AS $$
WITH history AS MATERIALIZED (
    SELECT h.* FROM recovery_custody_holds h WHERE h.run_id = r.id
)
SELECT COALESCE(
    r.worker_id IS NOT NULL AND r.claim_generation > 0
    AND (SELECT count(*) FROM history) = r.claim_generation
    AND (SELECT count(DISTINCT generation) FROM history) = r.claim_generation
    AND NOT EXISTS (
        SELECT 1 FROM recovery_custody_holds h WHERE h.state = 'open'
          AND (h.live_run_id = r.id OR (h.run_id = r.id
               AND (h.original_worker_id = r.worker_id OR h.live_worker_id = r.worker_id)))
    )
    AND NOT EXISTS (
        SELECT 1 FROM history h WHERE (
            h.generation BETWEEN 1 AND r.claim_generation
            AND h.user_id = r.user_id AND h.repo_id = r.repo_id
            AND (h.generation <> r.claim_generation OR h.original_worker_id = r.worker_id)
            AND h.original_worker_identity <> ''
            AND h.inventory_guarded AND h.live_worker_id IS NULL AND h.live_run_id IS NULL
            AND h.release_successor_generation IS NULL AND h.release_pushed_sha IS NULL
            AND h.release_source_sha IS NULL AND h.release_adopted_sha IS NULL
            AND h.release_final_head_sha IS NULL AND h.release_branch IS NULL
            AND (
                (h.state = 'discarded' AND (
                    (h.release_evidence = 'owner_discard'
                     AND h.final_disposition IS NULL AND h.final_capture_id IS NULL
                     AND h.final_source_sha IS NULL AND h.final_coverage_digest IS NULL
                     AND h.final_publication_id IS NULL)
                    OR (h.release_evidence = 'merged_predecessor_discard'
                        AND h.merged_predecessor_discard_binding @> jsonb_build_object(
                            'hold_id', h.id, 'generation', h.generation, 'run_id', h.run_id,
                            'owner_id', h.user_id, 'repo_id', h.repo_id)
                        AND jsonb_typeof(h.merged_predecessor_discard_binding->'completed_generation') = 'number'
                        AND h.merged_predecessor_discard_binding->'completed_generation' > to_jsonb(h.generation)
                        AND h.merged_predecessor_discard_binding->'completed_generation' <= to_jsonb(r.claim_generation))
                ))
                OR (h.state = 'released' AND h.released_at IS NOT NULL AND (
                    (h.final_disposition = 'no_adopted_source' AND h.release_evidence = 'no_adopted_source'
                     AND h.final_capture_id IS NULL AND h.final_source_sha IS NULL
                     AND h.final_coverage_digest IS NULL AND h.final_publication_id IS NULL
                     AND NOT EXISTS (SELECT 1 FROM recovery_captures c WHERE c.hold_id = h.id))
                    OR (h.final_disposition = 'settled' AND h.release_evidence IN ('forge_no_output', 'publication')
                        AND h.final_capture_id IS NULL AND h.final_source_sha IS NULL
                        AND h.final_publication_id IS NULL
                        AND h.final_coverage_digest = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
                    OR (h.final_disposition = 'completed_publication' AND h.release_evidence = 'completed_publication'
                        AND h.final_capture_id IS NULL AND h.final_source_sha IS NULL
                        AND h.final_coverage_digest IS NULL AND h.final_publication_id IS NULL
                        AND h.completed_publication_reason IS NULL
                        AND h.completion_identity @> jsonb_build_object(
                            'hold_id',h.id,'run_id',h.run_id,'owner_id',h.user_id,
                            'worker_id',h.original_worker_id,'generation',h.generation,'repo_id',h.repo_id)
                        AND h.completion_identity->>'final_head' ~ '^[0-9a-f]{40}$'
                        AND h.completed_publication_receipt = h.completion_identity ||
                            jsonb_build_object('observed_branch_head',h.completed_publication_receipt->>'observed_branch_head')
                        AND h.completed_publication_receipt->>'observed_branch_head' ~ '^[0-9a-f]{40}$')
                    OR (h.final_disposition = 'held_publication' AND h.release_evidence = 'held_publication'
                        AND h.final_capture_id IS NULL AND EXISTS (
                            SELECT 1 FROM run_held_publications p
                            WHERE p.id = h.final_publication_id AND p.hold_id = h.id
                              AND p.run_id = h.run_id AND p.user_id = h.user_id AND p.repo_id = h.repo_id
                              AND p.generation = h.generation AND p.worker_id = h.original_worker_id
                              AND p.ref = 'refs/uzi-held/' || h.run_id::text || '/' || h.generation::text
                              AND p.tip = h.final_source_sha AND p.coverage_digest = h.final_coverage_digest
                              AND p.state = 'acknowledged' AND p.acknowledged_at IS NOT NULL
                              AND p.owner_expired_at IS NULL AND p.expires_at > statement_timestamp()))
                    OR (h.final_disposition = 'archive' AND h.release_evidence = 'archive'
                        AND h.final_publication_id IS NULL AND EXISTS (
                            SELECT 1 FROM recovery_captures c
                            WHERE c.id = h.final_capture_id AND c.hold_id = h.id
                              AND c.run_id = h.run_id AND c.user_id = h.user_id
                              AND c.original_worker_id = h.original_worker_id
                              AND c.original_worker_identity = h.original_worker_identity
                              AND c.source_sha = h.final_source_sha AND c.coverage_digest = h.final_coverage_digest
                              AND c.manifest_bound AND c.state = 'available' AND c.expires_at > statement_timestamp()
                              AND COALESCE(cardinality(c.prerequisite_shas),0) = 0))
                ))
            )
        ) IS NOT TRUE
    )
    -- A recorded dependency or conflicting source remains protected. Existing
    -- FINAL digests certify complete inventory; bare captures certify neither.
    AND NOT EXISTS (
        SELECT 1 FROM recovery_captures c JOIN history h ON h.id = c.hold_id
        WHERE c.run_id <> h.run_id OR c.user_id <> h.user_id
           OR c.original_worker_id <> h.original_worker_id
           OR c.original_worker_identity <> h.original_worker_identity
           OR COALESCE(cardinality(c.prerequisite_shas),0) <> 0
    )
    AND NOT EXISTS (
        SELECT 1 FROM recovery_captures c JOIN history h ON h.id = c.hold_id
        GROUP BY c.hold_id HAVING count(DISTINCT c.source_sha) > 1
    ), false)
$$;
-- +goose StatementEnd

-- A forge-failing custodian keeps its ordinary pin unless the complete guarded
-- history proves cold placement safe. Unknown origin or legacy history never
-- creates a new affinity exemption.
-- +goose StatementBegin
CREATE FUNCTION fn_forge_origin_owner_pins(w workers, r runs, evaluated_at timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
SELECT NOT (
    EXISTS (SELECT 1 FROM authorized_forge_origins a JOIN forge_origin_cooldowns c
            ON c.user_id = a.user_id AND c.origin = a.origin AND c.worker_id = w.id
            WHERE a.repo_id = r.repo_id AND a.user_id = r.user_id)
    AND NOT fn_forge_origin_eligible(w,r,evaluated_at)
    AND fn_forge_source_history_safe(r)
)
$$;
-- +goose StatementEnd

-- Cooldown is not replacement-pod demand. This conservative advisory suppressor
-- waits while an existing worker has unresolved health at this authorized origin;
-- unrelated origins and noncloning lanes do not participate.
-- +goose StatementBegin
CREATE FUNCTION fn_forge_origin_provision_allowed(r runs, evaluated_at timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
SELECT r.repo_id IS NULL OR r.kind = 'chat' OR r.egress_profile_id IS NOT NULL
    OR NOT EXISTS (
        SELECT 1 FROM authorized_forge_origins a
        JOIN forge_origin_cooldowns c ON c.user_id = a.user_id AND c.origin = a.origin
        JOIN workers w ON w.id = c.worker_id AND w.user_id = a.user_id
        WHERE a.repo_id = r.repo_id AND a.user_id = r.user_id
          AND NOT fn_forge_origin_eligible(w,r,evaluated_at)
    )
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION fn_forge_origin_provision_allowed(runs,timestamptz);
DROP FUNCTION fn_forge_origin_owner_pins(workers,runs,timestamptz);
DROP FUNCTION fn_forge_source_history_safe(runs);
