-- +goose Up
-- Issue #2625: a completed run's recovery archive may expire early, but only on a recorded
-- published-redundancy proof. Three columns carry that evidence on the capture itself:
--   redundancy_proof        the shape the guard below checks against the capture, its hold and the run
--   redundancy_refused_at   when the API last refused a redundancy claim for this capture on forge evidence
--   redundancy_refusal      the bounded reason, so a repeat claim inside the cool-down makes no forge call
ALTER TABLE recovery_captures
    ADD COLUMN redundancy_proof jsonb,
    ADD COLUMN redundancy_refused_at timestamptz,
    ADD COLUMN redundancy_refusal text;
ALTER TABLE recovery_captures ADD CONSTRAINT recovery_captures_redundancy_proof_check
    CHECK ((redundancy_proof IS NULL) = (reason IS DISTINCT FROM 'published_redundant'));
ALTER TABLE recovery_captures ADD CONSTRAINT recovery_captures_redundancy_refusal_check
    CHECK ((redundancy_refused_at IS NULL) = (redundancy_refusal IS NULL));

-- The ADR-2417 guard, extended. Without a proof every branch below is the original: a protected
-- final capture cannot reach 'expired' before its window, a changed identity tuple is dropped.
-- With a proof the guard RAISEs on any violation (a silent RETURN NULL would let a CTE delete
-- chunks of a row it did not expire) and skips the protected-final raise only when the proof
-- matches the capture, its released hold and the completed run. The exemption is a COALESCE so
-- an unknown (NULL) predicate falls through to the original protection, never around it.
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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION recovery_inventory_capture_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Raise before any no-op guard: an old expiry CTE may already have deleted chunks.
    -- The exception rolls back that entire statement, including its chunk deletion.
    IF NEW.state = 'expired' AND EXISTS (
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
-- The proof columns go, so a capture that carries the reason without one would break the CHECK
-- when this migration is applied again. The rewrite runs under the restored guard, which only
-- looks at expiry of a protected final capture, and keeps the row otherwise as it was.
ALTER TABLE recovery_captures DROP CONSTRAINT recovery_captures_redundancy_refusal_check;
ALTER TABLE recovery_captures DROP CONSTRAINT recovery_captures_redundancy_proof_check;
UPDATE recovery_captures SET reason = 'published_redundant_rolled_back' WHERE reason = 'published_redundant';
ALTER TABLE recovery_captures
    DROP COLUMN redundancy_refusal,
    DROP COLUMN redundancy_refused_at,
    DROP COLUMN redundancy_proof;
