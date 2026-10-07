-- +goose Up
ALTER TABLE recovery_custody_holds
    ADD COLUMN inventory_guarded boolean NOT NULL DEFAULT false,
    ADD COLUMN final_disposition text CHECK (final_disposition IN ('archive', 'settled', 'no_adopted_source')),
    ADD COLUMN final_capture_id uuid,
    ADD COLUMN final_source_sha text,
    ADD COLUMN final_coverage_digest text CHECK (final_coverage_digest ~ '^[0-9a-f]{64}$');
ALTER TABLE recovery_captures
    ADD COLUMN coverage_digest text CHECK (coverage_digest ~ '^[0-9a-f]{64}$'),
    ADD COLUMN local_replica_worker_id uuid,
    ADD COLUMN ready_retention_seconds bigint CHECK (ready_retention_seconds > 0);

-- +goose StatementBegin
CREATE FUNCTION recovery_inventory_hold_guard() RETURNS trigger LANGUAGE plpgsql AS $$
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
        (NEW.final_disposition, NEW.final_capture_id, NEW.final_source_sha, NEW.final_coverage_digest)
        IS DISTINCT FROM
        (OLD.final_disposition, OLD.final_capture_id, OLD.final_source_sha, OLD.final_coverage_digest) THEN
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
        AND NEW.final_source_sha IS NULL AND NEW.final_coverage_digest IS NULL THEN RETURN NEW; END IF;
    IF NEW.state IS DISTINCT FROM OLD.state OR NEW.live_worker_id IS DISTINCT FROM OLD.live_worker_id
        OR NEW.live_run_id IS DISTINCT FROM OLD.live_run_id OR NEW.final_disposition IS NOT NULL THEN
        IF NEW.state <> 'released' OR NEW.live_worker_id IS NOT NULL OR NEW.live_run_id IS NOT NULL THEN
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
CREATE TRIGGER recovery_inventory_hold_guard BEFORE UPDATE ON recovery_custody_holds
FOR EACH ROW EXECUTE FUNCTION recovery_inventory_hold_guard();

-- +goose StatementBegin
CREATE FUNCTION recovery_inventory_capture_guard() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER recovery_inventory_capture_guard BEFORE UPDATE ON recovery_captures
FOR EACH ROW EXECUTE FUNCTION recovery_inventory_capture_guard();

-- The worker DELETE owns the worker lock before taking selected capture locks.
-- Lock every selected capture before renewing any deadline, so a later lock wait
-- cannot consume an earlier capture's post-deletion retention interval.
-- Visit each selected capture once; any failure rolls back the whole worker deletion.
-- +goose StatementBegin
CREATE FUNCTION recovery_inventory_worker_delete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c recovery_captures;
BEGIN
    PERFORM 1 FROM recovery_captures
        WHERE local_replica_worker_id = OLD.id
          AND EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.final_capture_id = recovery_captures.id)
        ORDER BY id FOR UPDATE;
    FOR c IN SELECT * FROM recovery_captures
        WHERE local_replica_worker_id = OLD.id
          AND EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.final_capture_id = recovery_captures.id)
        ORDER BY id FOR UPDATE
    LOOP
        UPDATE recovery_captures SET
            expires_at = GREATEST(expires_at, clock_timestamp() + make_interval(secs => c.ready_retention_seconds)),
            local_replica_worker_id = NULL, updated_at = clock_timestamp()
        WHERE id = c.id AND local_replica_worker_id = OLD.id;
    END LOOP;
    RETURN OLD;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER recovery_inventory_worker_delete BEFORE DELETE ON workers
FOR EACH ROW EXECUTE FUNCTION recovery_inventory_worker_delete();

-- +goose Down
DROP TRIGGER recovery_inventory_worker_delete ON workers;
DROP FUNCTION recovery_inventory_worker_delete();
DROP TRIGGER recovery_inventory_capture_guard ON recovery_captures;
DROP FUNCTION recovery_inventory_capture_guard();
DROP TRIGGER recovery_inventory_hold_guard ON recovery_custody_holds;
DROP FUNCTION recovery_inventory_hold_guard();
ALTER TABLE recovery_captures DROP COLUMN coverage_digest, DROP COLUMN local_replica_worker_id, DROP COLUMN ready_retention_seconds;
ALTER TABLE recovery_custody_holds DROP COLUMN inventory_guarded, DROP COLUMN final_disposition,
    DROP COLUMN final_capture_id, DROP COLUMN final_source_sha, DROP COLUMN final_coverage_digest;
