-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE recovery_custody_holds ADD COLUMN merged_predecessor_discard_binding jsonb;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
        'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry', 'completed_publication',
        'held_publication', 'merged_predecessor_discard'));
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_merged_predecessor_tie_check
    CHECK ((release_evidence IS NOT DISTINCT FROM 'merged_predecessor_discard')
        = (merged_predecessor_discard_binding IS NOT NULL));
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_merged_predecessor_state_check
    CHECK (merged_predecessor_discard_binding IS NULL OR
        (state = 'discarded' AND live_worker_id IS NULL AND live_run_id IS NULL));

-- This guard applies to legacy holds too. It deliberately does not infer merge from worker
-- declarations or local source ancestry. Only the server operation supplies this audit binding.
-- +goose StatementBegin
CREATE FUNCTION merged_predecessor_discard_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    b jsonb;
    verified_at timestamptz;
    mutable_keys text[] := ARRAY['state', 'live_worker_id', 'live_run_id', 'release_evidence',
        'merged_predecessor_discard_binding', 'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.merged_predecessor_discard_binding IS NOT NULL
            OR NEW.release_evidence = 'merged_predecessor_discard' THEN
            RAISE EXCEPTION 'merged predecessor discard cannot be inserted';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        IF OLD.merged_predecessor_discard_binding IS NOT NULL THEN
            RAISE EXCEPTION 'merged predecessor audit cannot be deleted';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.merged_predecessor_discard_binding IS NOT NULL THEN
        IF (to_jsonb(NEW) - 'updated_at') IS DISTINCT FROM (to_jsonb(OLD) - 'updated_at') THEN
            RAISE EXCEPTION 'merged predecessor audit is immutable';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.merged_predecessor_discard_binding IS NULL
        AND NEW.release_evidence IS DISTINCT FROM 'merged_predecessor_discard' THEN RETURN NEW; END IF;

    b := NEW.merged_predecessor_discard_binding;
    IF OLD.state <> 'open' OR NEW.state <> 'discarded'
        OR NEW.release_evidence IS DISTINCT FROM 'merged_predecessor_discard'
        OR NEW.live_worker_id IS NOT NULL OR NEW.live_run_id IS NOT NULL
        OR jsonb_typeof(b) IS DISTINCT FROM 'object'
        OR jsonb_typeof(b->'verified_at') IS DISTINCT FROM 'string'
        OR (to_jsonb(NEW) - mutable_keys) IS DISTINCT FROM (to_jsonb(OLD) - mutable_keys) THEN
        RAISE EXCEPTION 'invalid merged predecessor transition';
    END IF;
    -- Cast errors reject malformed values; NULL, infinity, relative dates and future values
    -- are refused. The server emits an absolute RFC3339 timestamp.
    IF (b->>'verified_at') !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' THEN
        RAISE EXCEPTION 'invalid merged predecessor verification timestamp';
    END IF;
    verified_at := (b->>'verified_at')::timestamptz;
    IF verified_at IS NULL OR NOT isfinite(verified_at) OR verified_at > clock_timestamp() THEN
        RAISE EXCEPTION 'invalid merged predecessor verification timestamp';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runs r JOIN repos p ON p.id = r.repo_id
        JOIN forge_connections c ON c.id = p.connection_id
        WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id AND r.repo_id = OLD.repo_id
            AND c.user_id = OLD.user_id AND r.status = 'completed'
            AND r.claim_generation > OLD.generation AND r.mr_iid > 0
            AND b = jsonb_build_object(
                'hold_id', OLD.id, 'generation', OLD.generation, 'run_id', OLD.run_id,
                'owner_id', OLD.user_id, 'repo_id', OLD.repo_id, 'connection_id', c.id,
                'forge_type', c.forge_type, 'base_url', c.base_url, 'project_id', p.forge_project_id,
                'mr_iid', r.mr_iid, 'completed_generation', r.claim_generation,
                'verified_at', b->'verified_at')
    ) THEN
        RAISE EXCEPTION 'merged predecessor binding mismatch';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER merged_predecessor_discard_guard BEFORE INSERT OR UPDATE OR DELETE
    ON recovery_custody_holds FOR EACH ROW EXECUTE FUNCTION merged_predecessor_discard_guard();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION recovery_inventory_hold_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.inventory_guarded IS DISTINCT FROM OLD.inventory_guarded THEN
        RETURN NULL;
    END IF;
    IF NOT OLD.inventory_guarded THEN RETURN NEW; END IF;
    IF (NEW.user_id, NEW.repo_id, NEW.run_id, NEW.generation, NEW.original_worker_id, NEW.original_worker_identity)
        IS DISTINCT FROM (OLD.user_id, OLD.repo_id, OLD.run_id, OLD.generation, OLD.original_worker_id, OLD.original_worker_identity) THEN
        RETURN NULL;
    END IF;
    IF OLD.final_disposition IS NOT NULL AND
        (NEW.final_disposition, NEW.final_capture_id, NEW.final_source_sha, NEW.final_coverage_digest,
         NEW.final_publication_id)
        IS DISTINCT FROM
        (OLD.final_disposition, OLD.final_capture_id, OLD.final_source_sha, OLD.final_coverage_digest,
         OLD.final_publication_id) THEN
        RETURN NULL;
    END IF;
    IF OLD.state <> 'open' THEN
        IF (NEW.state, NEW.live_worker_id, NEW.live_run_id, NEW.release_evidence)
            IS DISTINCT FROM (OLD.state, OLD.live_worker_id, OLD.live_run_id, OLD.release_evidence) THEN
            RETURN NULL;
        END IF;
        RETURN NEW;
    END IF;
    -- The universal merged_predecessor_discard_guard validates the binding and preserves
    -- every column except this discard's lifecycle fields, including prior FINAL evidence.
    IF NEW.state = 'discarded' AND NEW.release_evidence = 'merged_predecessor_discard'
        AND NEW.merged_predecessor_discard_binding IS NOT NULL
        AND NEW.live_worker_id IS NULL AND NEW.live_run_id IS NULL THEN RETURN NEW; END IF;
    IF NEW.state = 'discarded' AND NEW.release_evidence = 'owner_discard'
        AND NEW.live_worker_id IS NULL AND NEW.live_run_id IS NULL
        AND NEW.final_disposition IS NULL AND NEW.final_capture_id IS NULL
        AND NEW.final_source_sha IS NULL AND NEW.final_coverage_digest IS NULL
        AND NEW.final_publication_id IS NULL THEN RETURN NEW; END IF;
    IF NEW.state IS DISTINCT FROM OLD.state OR NEW.live_worker_id IS DISTINCT FROM OLD.live_worker_id
        OR NEW.live_run_id IS DISTINCT FROM OLD.live_run_id OR NEW.final_disposition IS NOT NULL THEN
        IF NEW.state <> 'released' OR NEW.live_worker_id IS NOT NULL OR NEW.live_run_id IS NOT NULL THEN
            RETURN NULL;
        END IF;
        IF NEW.final_disposition = 'completed_publication' THEN
            IF NEW.release_evidence = 'completed_publication'
                AND NEW.final_capture_id IS NULL AND NEW.final_source_sha IS NULL
                AND NEW.final_coverage_digest IS NULL
                AND NEW.completed_publication_receipt IS NOT NULL
                AND EXISTS (SELECT 1 FROM runs r JOIN repos p ON p.id = r.repo_id
                    JOIN forge_connections c ON c.id = p.connection_id WHERE r.id = OLD.run_id
                    AND r.user_id = OLD.user_id AND c.user_id = OLD.user_id AND r.worker_id = OLD.original_worker_id
                    AND r.claim_generation = OLD.generation AND r.status = 'completed'
                    AND r.claim_released_at IS NULL
                    AND r.completion_final_head = OLD.completion_identity->>'final_head'
                    AND r.repo_id::text = OLD.completion_identity->>'repo_id'
                    AND p.connection_id::text = OLD.completion_identity->>'connection_id'
                    AND p.forge_project_id::text = OLD.completion_identity->>'project_id'
                    AND c.forge_type = OLD.completion_identity->>'forge_type'
                    AND c.base_url = OLD.completion_identity->>'base_url'
                    AND r.mr_iid::text = OLD.completion_identity->>'mr_iid'
                    AND CASE WHEN r.kind = 'issue' THEN 'agent/issue-' || r.issue_iid::text
                        WHEN r.kind = 'mr_rework' THEN r.pipeline_ref
                        WHEN r.kind = 'self_improve' THEN 'uzi/self-improve/' || r.id::text END = OLD.completion_identity->>'branch') THEN
                RETURN NEW;
            END IF;
            RETURN NULL;
        END IF;
        IF NEW.final_disposition = 'no_adopted_source' THEN
            IF NEW.release_evidence = 'no_adopted_source'
                AND NEW.final_capture_id IS NULL AND NEW.final_source_sha IS NULL
                AND NEW.final_coverage_digest IS NULL AND EXISTS (
                SELECT 1 FROM runs r WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id
                  AND r.worker_id = OLD.original_worker_id AND r.claim_generation = OLD.generation
                  AND r.status = 'claimed' AND r.claim_released_at IS NULL
            ) AND NOT EXISTS (SELECT 1 FROM recovery_captures c WHERE c.hold_id = OLD.id) THEN
                RETURN NEW;
            END IF;
            RETURN NULL;
        END IF;
        IF NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id
            AND (r.claim_generation > OLD.generation OR
                 (r.claim_generation = OLD.generation AND
                  (r.status IN ('completed', 'failed', 'cancelled') OR r.claim_released_at IS NOT NULL
                   -- issue #1924: a forge_unreachable pre-clone park keeps its claim; only the
                   -- worker's settled forge_no_output proof (empty inventory) ends that generation.
                   OR (r.status = 'recovery_wait' AND r.recovery_wait_cause = 'forge_unreachable'
                       AND NEW.final_disposition = 'settled' AND NEW.release_evidence = 'forge_no_output'))))) THEN
            RETURN NULL;
        END IF;
        -- Held publication (issue #2545): only a FAILED run whose exact generation the hold
        -- covers, with a created, hold-bound, tip- and digest-matching publication row.
        -- Cancelled runs (including a NULL fail_origin), stale generations and the
        -- push_secret_blocked / worker_residue_blocked origins never release this way.
        IF NEW.final_disposition = 'held_publication' THEN
            IF NEW.release_evidence = 'held_publication' AND NEW.final_capture_id IS NULL
                AND COALESCE((SELECT r.status = 'failed'
                        AND r.fail_origin IS DISTINCT FROM 'push_secret_blocked'
                        AND r.fail_origin IS DISTINCT FROM 'worker_residue_blocked'
                        AND r.user_id = OLD.user_id AND r.claim_generation = OLD.generation
                        AND r.worker_id = OLD.original_worker_id
                        AND p.hold_id = OLD.id AND p.run_id = OLD.run_id AND p.user_id = OLD.user_id
                        AND p.repo_id = OLD.repo_id
                        AND p.generation = OLD.generation AND p.worker_id = OLD.original_worker_id
                        AND p.ref = 'refs/uzi-held/' || OLD.run_id::text || '/' || OLD.generation::text
                        AND p.coverage_digest = NEW.final_coverage_digest
                        AND p.tip = NEW.final_source_sha AND p.state = 'created'
                    FROM runs r JOIN run_held_publications p ON p.id = NEW.final_publication_id
                    WHERE r.id = OLD.run_id), false) THEN
                RETURN NEW;
            END IF;
            RETURN NULL;
        END IF;
        IF NEW.final_disposition = 'archive' AND NEW.release_evidence = 'archive' AND EXISTS (
            SELECT 1 FROM recovery_captures c WHERE c.id = NEW.final_capture_id
              AND c.hold_id = OLD.id AND c.run_id = OLD.run_id AND c.user_id = OLD.user_id
              AND c.original_worker_id = OLD.original_worker_id AND c.source_sha = NEW.final_source_sha
              AND c.coverage_digest = NEW.final_coverage_digest
              AND c.manifest_bound AND c.state = 'available' AND c.expires_at > clock_timestamp()
              AND c.local_replica_worker_id = OLD.original_worker_id AND c.ready_retention_seconds > 0
        ) THEN RETURN NEW; END IF;
        IF NEW.final_disposition = 'settled' AND NEW.final_capture_id IS NULL
            AND NEW.final_source_sha IS NULL
            AND NEW.final_coverage_digest = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
            AND (NEW.release_evidence = 'forge_no_output' OR
                (NEW.release_evidence = 'publication' AND EXISTS (
                    SELECT 1 FROM runs r WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id
                      AND r.status = 'completed' AND r.claim_generation = OLD.generation
                      AND r.worker_id = OLD.original_worker_id
                ))) THEN RETURN NEW; END IF;
        RETURN NULL;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE recovery_custody_holds IN ACCESS EXCLUSIVE MODE;
-- Retained merge authority cannot be relabelled or silently erased by a rollback.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM recovery_custody_holds
        WHERE merged_predecessor_discard_binding IS NOT NULL
            OR release_evidence = 'merged_predecessor_discard') THEN
        RAISE EXCEPTION 'merged predecessor audit exists; refusing destructive rollback';
    END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER merged_predecessor_discard_guard ON recovery_custody_holds;
DROP FUNCTION merged_predecessor_discard_guard();
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_merged_predecessor_tie_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_merged_predecessor_state_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
        'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry', 'completed_publication',
        'held_publication'));
ALTER TABLE recovery_custody_holds DROP COLUMN merged_predecessor_discard_binding;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION recovery_inventory_hold_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.inventory_guarded IS DISTINCT FROM OLD.inventory_guarded THEN
        RETURN NULL;
    END IF;
    IF NOT OLD.inventory_guarded THEN RETURN NEW; END IF;
    IF (NEW.user_id, NEW.repo_id, NEW.run_id, NEW.generation, NEW.original_worker_id, NEW.original_worker_identity)
        IS DISTINCT FROM (OLD.user_id, OLD.repo_id, OLD.run_id, OLD.generation, OLD.original_worker_id, OLD.original_worker_identity) THEN
        RETURN NULL;
    END IF;
    IF OLD.final_disposition IS NOT NULL AND
        (NEW.final_disposition, NEW.final_capture_id, NEW.final_source_sha, NEW.final_coverage_digest,
         NEW.final_publication_id)
        IS DISTINCT FROM
        (OLD.final_disposition, OLD.final_capture_id, OLD.final_source_sha, OLD.final_coverage_digest,
         OLD.final_publication_id) THEN
        RETURN NULL;
    END IF;
    IF OLD.state <> 'open' THEN
        IF (NEW.state, NEW.live_worker_id, NEW.live_run_id, NEW.release_evidence)
            IS DISTINCT FROM (OLD.state, OLD.live_worker_id, OLD.live_run_id, OLD.release_evidence) THEN
            RETURN NULL;
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.state = 'discarded' AND NEW.release_evidence = 'owner_discard'
        AND NEW.live_worker_id IS NULL AND NEW.live_run_id IS NULL
        AND NEW.final_disposition IS NULL AND NEW.final_capture_id IS NULL
        AND NEW.final_source_sha IS NULL AND NEW.final_coverage_digest IS NULL
        AND NEW.final_publication_id IS NULL THEN RETURN NEW; END IF;
    IF NEW.state IS DISTINCT FROM OLD.state OR NEW.live_worker_id IS DISTINCT FROM OLD.live_worker_id
        OR NEW.live_run_id IS DISTINCT FROM OLD.live_run_id OR NEW.final_disposition IS NOT NULL THEN
        IF NEW.state <> 'released' OR NEW.live_worker_id IS NOT NULL OR NEW.live_run_id IS NOT NULL THEN
            RETURN NULL;
        END IF;
        IF NEW.final_disposition = 'completed_publication' THEN
            IF NEW.release_evidence = 'completed_publication'
                AND NEW.final_capture_id IS NULL AND NEW.final_source_sha IS NULL
                AND NEW.final_coverage_digest IS NULL
                AND NEW.completed_publication_receipt IS NOT NULL
                AND EXISTS (SELECT 1 FROM runs r JOIN repos p ON p.id = r.repo_id
                    JOIN forge_connections c ON c.id = p.connection_id WHERE r.id = OLD.run_id
                    AND r.user_id = OLD.user_id AND c.user_id = OLD.user_id AND r.worker_id = OLD.original_worker_id
                    AND r.claim_generation = OLD.generation AND r.status = 'completed'
                    AND r.claim_released_at IS NULL
                    AND r.completion_final_head = OLD.completion_identity->>'final_head'
                    AND r.repo_id::text = OLD.completion_identity->>'repo_id'
                    AND p.connection_id::text = OLD.completion_identity->>'connection_id'
                    AND p.forge_project_id::text = OLD.completion_identity->>'project_id'
                    AND c.forge_type = OLD.completion_identity->>'forge_type'
                    AND c.base_url = OLD.completion_identity->>'base_url'
                    AND r.mr_iid::text = OLD.completion_identity->>'mr_iid'
                    AND CASE WHEN r.kind = 'issue' THEN 'agent/issue-' || r.issue_iid::text
                        WHEN r.kind = 'mr_rework' THEN r.pipeline_ref
                        WHEN r.kind = 'self_improve' THEN 'uzi/self-improve/' || r.id::text END = OLD.completion_identity->>'branch') THEN
                RETURN NEW;
            END IF;
            RETURN NULL;
        END IF;
        IF NEW.final_disposition = 'no_adopted_source' THEN
            IF NEW.release_evidence = 'no_adopted_source'
                AND NEW.final_capture_id IS NULL AND NEW.final_source_sha IS NULL
                AND NEW.final_coverage_digest IS NULL AND EXISTS (
                SELECT 1 FROM runs r WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id
                  AND r.worker_id = OLD.original_worker_id AND r.claim_generation = OLD.generation
                  AND r.status = 'claimed' AND r.claim_released_at IS NULL
            ) AND NOT EXISTS (SELECT 1 FROM recovery_captures c WHERE c.hold_id = OLD.id) THEN
                RETURN NEW;
            END IF;
            RETURN NULL;
        END IF;
        IF NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id
            AND (r.claim_generation > OLD.generation OR
                 (r.claim_generation = OLD.generation AND
                  (r.status IN ('completed', 'failed', 'cancelled') OR r.claim_released_at IS NOT NULL
                   -- issue #1924: a forge_unreachable pre-clone park keeps its claim; only the
                   -- worker's settled forge_no_output proof (empty inventory) ends that generation.
                   OR (r.status = 'recovery_wait' AND r.recovery_wait_cause = 'forge_unreachable'
                       AND NEW.final_disposition = 'settled' AND NEW.release_evidence = 'forge_no_output'))))) THEN
            RETURN NULL;
        END IF;
        -- Held publication (issue #2545): only a FAILED run whose exact generation the hold
        -- covers, with a created, hold-bound, tip- and digest-matching publication row.
        -- Cancelled runs (including a NULL fail_origin), stale generations and the
        -- push_secret_blocked / worker_residue_blocked origins never release this way.
        IF NEW.final_disposition = 'held_publication' THEN
            IF NEW.release_evidence = 'held_publication' AND NEW.final_capture_id IS NULL
                AND COALESCE((SELECT r.status = 'failed'
                        AND r.fail_origin IS DISTINCT FROM 'push_secret_blocked'
                        AND r.fail_origin IS DISTINCT FROM 'worker_residue_blocked'
                        AND r.user_id = OLD.user_id AND r.claim_generation = OLD.generation
                        AND r.worker_id = OLD.original_worker_id
                        AND p.hold_id = OLD.id AND p.run_id = OLD.run_id AND p.user_id = OLD.user_id
                        AND p.repo_id = OLD.repo_id
                        AND p.generation = OLD.generation AND p.worker_id = OLD.original_worker_id
                        AND p.ref = 'refs/uzi-held/' || OLD.run_id::text || '/' || OLD.generation::text
                        AND p.coverage_digest = NEW.final_coverage_digest
                        AND p.tip = NEW.final_source_sha AND p.state = 'created'
                    FROM runs r JOIN run_held_publications p ON p.id = NEW.final_publication_id
                    WHERE r.id = OLD.run_id), false) THEN
                RETURN NEW;
            END IF;
            RETURN NULL;
        END IF;
        IF NEW.final_disposition = 'archive' AND NEW.release_evidence = 'archive' AND EXISTS (
            SELECT 1 FROM recovery_captures c WHERE c.id = NEW.final_capture_id
              AND c.hold_id = OLD.id AND c.run_id = OLD.run_id AND c.user_id = OLD.user_id
              AND c.original_worker_id = OLD.original_worker_id AND c.source_sha = NEW.final_source_sha
              AND c.coverage_digest = NEW.final_coverage_digest
              AND c.manifest_bound AND c.state = 'available' AND c.expires_at > clock_timestamp()
              AND c.local_replica_worker_id = OLD.original_worker_id AND c.ready_retention_seconds > 0
        ) THEN RETURN NEW; END IF;
        IF NEW.final_disposition = 'settled' AND NEW.final_capture_id IS NULL
            AND NEW.final_source_sha IS NULL
            AND NEW.final_coverage_digest = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
            AND (NEW.release_evidence = 'forge_no_output' OR
                (NEW.release_evidence = 'publication' AND EXISTS (
                    SELECT 1 FROM runs r WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id
                      AND r.status = 'completed' AND r.claim_generation = OLD.generation
                      AND r.worker_id = OLD.original_worker_id
                ))) THEN RETURN NEW; END IF;
        RETURN NULL;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
