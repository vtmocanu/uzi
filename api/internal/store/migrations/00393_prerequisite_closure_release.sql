-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Goose runs this migration in one transaction. Block hold DDL races and closure
-- inserts before the new key and constraints are added.
LOCK TABLE recovery_custody_holds IN ACCESS EXCLUSIVE MODE;
LOCK TABLE recovery_prerequisite_closures IN SHARE ROW EXCLUSIVE MODE;

-- Composite key the hold's FINAL closure reference points at, so a release binds the
-- exact (closure, capture, hold) triple rather than a bare closure id.
CREATE UNIQUE INDEX recovery_prerequisite_closures_release_key
    ON recovery_prerequisite_closures (id, capture_id, hold_id);

ALTER TABLE recovery_custody_holds
    ADD COLUMN final_closure_id uuid,
    ADD CONSTRAINT recovery_custody_holds_final_closure_fkey
        FOREIGN KEY (final_closure_id, final_capture_id, id)
        REFERENCES recovery_prerequisite_closures (id, capture_id, hold_id) ON DELETE RESTRICT,
    DROP CONSTRAINT recovery_custody_holds_release_evidence_check,
    ADD CONSTRAINT recovery_custody_holds_release_evidence_check
        CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
            'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry', 'completed_publication',
            'held_publication', 'merged_predecessor_discard', 'prerequisite_closure')),
    ADD CONSTRAINT recovery_custody_holds_closure_evidence_check
        CHECK ((release_evidence IS NOT DISTINCT FROM 'prerequisite_closure') = (final_closure_id IS NOT NULL)),
    ADD CONSTRAINT recovery_custody_holds_closure_disposition_check
        CHECK (final_closure_id IS NULL
            OR (final_disposition IS NOT DISTINCT FROM 'archive' AND final_capture_id IS NOT NULL)),
    ADD CONSTRAINT recovery_custody_holds_closure_guarded_check
        CHECK (final_closure_id IS NULL OR inventory_guarded);

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
         NEW.final_publication_id, NEW.final_closure_id)
        IS DISTINCT FROM
        (OLD.final_disposition, OLD.final_capture_id, OLD.final_source_sha, OLD.final_coverage_digest,
         OLD.final_publication_id, OLD.final_closure_id) THEN
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
        -- Held publication requires terminal custody and an exact created publication.
        IF NEW.final_disposition = 'held_publication' THEN
            IF NEW.release_evidence = 'held_publication' AND NEW.final_capture_id IS NULL
                AND COALESCE((SELECT r.status IN ('failed', 'cancelled', 'completed')
                        AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
                        AND r.fail_origin IS DISTINCT FROM 'push_secret_blocked'
                        AND r.fail_origin IS DISTINCT FROM 'worker_residue_blocked'
                        AND r.user_id = OLD.user_id AND r.repo_id = OLD.repo_id
                        AND OLD.generation > 0 AND OLD.generation <= r.claim_generation
                        AND OLD.live_worker_id = OLD.original_worker_id AND OLD.live_run_id = OLD.run_id
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
        -- Prerequisite closure release: the archive predicates plus an exact, still-pinned
        -- closure bound to this hold and capture. Placed before the archive branch so
        -- prerequisite_closure evidence is decided here and never reaches the later branches.
        IF NEW.final_disposition = 'archive' AND NEW.release_evidence = 'prerequisite_closure' THEN
            IF EXISTS (
                SELECT 1 FROM recovery_captures c
                JOIN recovery_prerequisite_closures p ON p.capture_id = c.id
                WHERE c.id = NEW.final_capture_id
                  AND c.hold_id = OLD.id AND c.run_id = OLD.run_id AND c.user_id = OLD.user_id
                  AND c.original_worker_id = OLD.original_worker_id AND c.source_sha = NEW.final_source_sha
                  AND c.coverage_digest = NEW.final_coverage_digest
                  AND c.manifest_bound AND c.state = 'available' AND c.expires_at > clock_timestamp()
                  AND c.local_replica_worker_id = OLD.original_worker_id AND c.ready_retention_seconds > 0
                  AND p.id = NEW.final_closure_id AND p.capture_id = NEW.final_capture_id
                  AND p.hold_id = OLD.id AND p.state = 'pinned' AND p.absent_since IS NULL
                  AND p.checksum = c.checksum AND p.prerequisite_shas = c.prerequisite_shas
            ) THEN RETURN NEW; END IF;
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

-- +goose StatementBegin
CREATE FUNCTION recovery_closure_hold_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.final_closure_id IS NOT NULL OR NEW.release_evidence IS NOT DISTINCT FROM 'prerequisite_closure' THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER recovery_closure_hold_insert_guard BEFORE INSERT ON recovery_custody_holds
    FOR EACH ROW EXECUTE FUNCTION recovery_closure_hold_insert_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Goose runs this migration in one transaction. Block writers before checking ANY evidence.
LOCK TABLE recovery_custody_holds, recovery_prerequisite_closures IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM recovery_custody_holds
        WHERE final_closure_id IS NOT NULL OR release_evidence = 'prerequisite_closure') THEN
        RAISE EXCEPTION 'prerequisite closure release evidence exists; rollback refused' USING ERRCODE='55000';
    END IF;
END $$;
-- +goose StatementEnd

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
        -- Held publication requires terminal custody and an exact created publication.
        IF NEW.final_disposition = 'held_publication' THEN
            IF NEW.release_evidence = 'held_publication' AND NEW.final_capture_id IS NULL
                AND COALESCE((SELECT r.status IN ('failed', 'cancelled', 'completed')
                        AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
                        AND r.fail_origin IS DISTINCT FROM 'push_secret_blocked'
                        AND r.fail_origin IS DISTINCT FROM 'worker_residue_blocked'
                        AND r.user_id = OLD.user_id AND r.repo_id = OLD.repo_id
                        AND OLD.generation > 0 AND OLD.generation <= r.claim_generation
                        AND OLD.live_worker_id = OLD.original_worker_id AND OLD.live_run_id = OLD.run_id
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

DROP TRIGGER recovery_closure_hold_insert_guard ON recovery_custody_holds;
DROP FUNCTION recovery_closure_hold_insert_guard();
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_final_closure_fkey;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_closure_guarded_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_closure_disposition_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_closure_evidence_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
        'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry', 'completed_publication',
        'held_publication', 'merged_predecessor_discard'));
ALTER TABLE recovery_custody_holds DROP COLUMN final_closure_id;
DROP INDEX recovery_prerequisite_closures_release_key;
