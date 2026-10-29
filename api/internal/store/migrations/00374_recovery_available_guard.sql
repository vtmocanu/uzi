-- +goose Up
-- Function replacement/catalog-lock contention is unmeasured; no capture-data rewrite.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION recovery_inventory_capture_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    redundant boolean := false;
BEGIN
    IF OLD.redundancy_proof IS NOT NULL AND
        (NEW.redundancy_proof IS DISTINCT FROM OLD.redundancy_proof OR NEW.reason IS DISTINCT FROM OLD.reason) THEN
        RAISE EXCEPTION 'recovery redundancy proof is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.redundancy_proof IS NOT NULL AND OLD.redundancy_proof IS NULL THEN
        IF OLD.state IS DISTINCT FROM 'available' OR NEW.state IS DISTINCT FROM 'expired' THEN
            RAISE EXCEPTION 'recovery redundancy proof requires an available capture to expire' USING ERRCODE = '55000';
        END IF;
        redundant := COALESCE(
            OLD.manifest_bound AND OLD.coverage_digest IS NOT NULL
            AND NEW.reason = 'published_redundant'
            AND NEW.redundancy_proof->>'hold_id' = OLD.hold_id::text
            AND NEW.redundancy_proof->>'coverage_digest' = OLD.coverage_digest
            AND NEW.redundancy_proof->>'source_sha' = OLD.source_sha
            AND NEW.redundancy_proof->>'current_sha' ~ '^[0-9a-f]{40}$'
            AND NEW.redundancy_proof->>'tree' ~ '^[0-9a-f]{40}$'
            AND NEW.redundancy_proof->>'anchor_head' ~ '^[0-9a-f]{40}$'
            AND EXISTS (
                SELECT 1 FROM recovery_custody_holds h JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
                WHERE h.id = OLD.hold_id AND h.run_id = OLD.run_id AND h.user_id = OLD.user_id
                    AND h.original_worker_id = OLD.original_worker_id AND h.inventory_guarded
                    AND h.state = 'released'
                    AND h.completion_identity->>'hold_id' = h.id::text
                    AND NEW.redundancy_proof->>'generation' = h.generation::text
                    AND NEW.redundancy_proof->>'final_head' = h.completion_identity->>'final_head'
                    AND NEW.redundancy_proof->>'mr_iid' = h.completion_identity->>'mr_iid'
                    AND r.status = 'completed' AND r.claim_generation = h.generation
                    AND r.worker_id = h.original_worker_id
                    AND r.completion_final_head = h.completion_identity->>'final_head'
                    AND r.mr_iid::text = h.completion_identity->>'mr_iid'),
            false);
        IF NOT redundant THEN
            RAISE EXCEPTION 'recovery redundancy proof does not match its capture' USING ERRCODE = '55000';
        END IF;
        IF (NEW.hold_id, NEW.run_id, NEW.user_id, NEW.original_worker_id, NEW.source_sha, NEW.coverage_digest)
            IS DISTINCT FROM (OLD.hold_id, OLD.run_id, OLD.user_id, OLD.original_worker_id, OLD.source_sha, OLD.coverage_digest) THEN
            RAISE EXCEPTION 'recovery redundancy proof cannot change the capture identity' USING ERRCODE = '55000';
        END IF;
    END IF;
    -- Raise before any no-op guard: an old expiry CTE may already have deleted chunks.
    -- The exception rolls back that entire statement, including its chunk deletion.
    IF NEW.state = 'expired' AND NOT redundant AND EXISTS (
        SELECT 1 FROM recovery_custody_holds h WHERE h.final_capture_id = OLD.id
    ) AND (OLD.local_replica_worker_id IS NOT NULL OR OLD.expires_at > clock_timestamp()) THEN
        RAISE EXCEPTION 'final recovery capture is protected' USING ERRCODE = '55000';
    END IF;
    IF NEW.state = 'available' AND OLD.state NOT IN ('uploading', 'available') THEN
        RAISE EXCEPTION 'recovery capture must be uploading before becoming available' USING ERRCODE = '55000';
    END IF;
    IF (NEW.hold_id, NEW.run_id, NEW.user_id, NEW.original_worker_id, NEW.source_sha, NEW.coverage_digest)
        IS DISTINCT FROM (OLD.hold_id, OLD.run_id, OLD.user_id, OLD.original_worker_id, OLD.source_sha, OLD.coverage_digest) THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Removes the invariant; this is not future C1b rollback authority.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION recovery_inventory_capture_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    redundant boolean := false;
BEGIN
    IF OLD.redundancy_proof IS NOT NULL AND
        (NEW.redundancy_proof IS DISTINCT FROM OLD.redundancy_proof OR NEW.reason IS DISTINCT FROM OLD.reason) THEN
        RAISE EXCEPTION 'recovery redundancy proof is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.redundancy_proof IS NOT NULL AND OLD.redundancy_proof IS NULL THEN
        IF OLD.state IS DISTINCT FROM 'available' OR NEW.state IS DISTINCT FROM 'expired' THEN
            RAISE EXCEPTION 'recovery redundancy proof requires an available capture to expire' USING ERRCODE = '55000';
        END IF;
        redundant := COALESCE(
            OLD.manifest_bound AND OLD.coverage_digest IS NOT NULL
            AND NEW.reason = 'published_redundant'
            AND NEW.redundancy_proof->>'hold_id' = OLD.hold_id::text
            AND NEW.redundancy_proof->>'coverage_digest' = OLD.coverage_digest
            AND NEW.redundancy_proof->>'source_sha' = OLD.source_sha
            AND NEW.redundancy_proof->>'current_sha' ~ '^[0-9a-f]{40}$'
            AND NEW.redundancy_proof->>'tree' ~ '^[0-9a-f]{40}$'
            AND NEW.redundancy_proof->>'anchor_head' ~ '^[0-9a-f]{40}$'
            AND EXISTS (
                SELECT 1 FROM recovery_custody_holds h JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
                WHERE h.id = OLD.hold_id AND h.run_id = OLD.run_id AND h.user_id = OLD.user_id
                    AND h.original_worker_id = OLD.original_worker_id AND h.inventory_guarded
                    AND h.state = 'released'
                    AND h.completion_identity->>'hold_id' = h.id::text
                    AND NEW.redundancy_proof->>'generation' = h.generation::text
                    AND NEW.redundancy_proof->>'final_head' = h.completion_identity->>'final_head'
                    AND NEW.redundancy_proof->>'mr_iid' = h.completion_identity->>'mr_iid'
                    AND r.status = 'completed' AND r.claim_generation = h.generation
                    AND r.worker_id = h.original_worker_id
                    AND r.completion_final_head = h.completion_identity->>'final_head'
                    AND r.mr_iid::text = h.completion_identity->>'mr_iid'),
            false);
        IF NOT redundant THEN
            RAISE EXCEPTION 'recovery redundancy proof does not match its capture' USING ERRCODE = '55000';
        END IF;
        IF (NEW.hold_id, NEW.run_id, NEW.user_id, NEW.original_worker_id, NEW.source_sha, NEW.coverage_digest)
            IS DISTINCT FROM (OLD.hold_id, OLD.run_id, OLD.user_id, OLD.original_worker_id, OLD.source_sha, OLD.coverage_digest) THEN
            RAISE EXCEPTION 'recovery redundancy proof cannot change the capture identity' USING ERRCODE = '55000';
        END IF;
    END IF;
    -- Raise before any no-op guard: an old expiry CTE may already have deleted chunks.
    -- The exception rolls back that entire statement, including its chunk deletion.
    IF NEW.state = 'expired' AND NOT redundant AND EXISTS (
        SELECT 1 FROM recovery_custody_holds h WHERE h.final_capture_id = OLD.id
    ) AND (OLD.local_replica_worker_id IS NOT NULL OR OLD.expires_at > clock_timestamp()) THEN
        RAISE EXCEPTION 'final recovery capture is protected' USING ERRCODE = '55000';
    END IF;
    IF (NEW.hold_id, NEW.run_id, NEW.user_id, NEW.original_worker_id, NEW.source_sha, NEW.coverage_digest)
        IS DISTINCT FROM (OLD.hold_id, OLD.run_id, OLD.user_id, OLD.original_worker_id, OLD.source_sha, OLD.coverage_digest) THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
