-- +goose Up
ALTER TABLE runs ADD COLUMN completion_final_head text CHECK (completion_final_head ~ '^[0-9a-f]{40}$');
ALTER TABLE recovery_custody_holds
    ADD COLUMN completion_identity jsonb,
    ADD COLUMN completed_publication_receipt jsonb,
    ADD COLUMN completed_publication_reason text CHECK (completed_publication_reason IN (
        'completion_identity_missing', 'not_completed', 'identity_changed', 'mr_missing',
        'branch_missing', 'branch_mismatch', 'head_mismatch', 'not_ancestor',
        'ancestry_unknown', 'forge_timeout'));
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
        'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry', 'completed_publication'));
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_final_disposition_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_final_disposition_check
    CHECK (final_disposition IN ('archive', 'settled', 'no_adopted_source', 'completed_publication'));
ALTER TABLE recovery_custody_holds ADD CONSTRAINT completed_publication_receipt_check CHECK (
    (completed_publication_receipt IS NULL AND final_disposition IS DISTINCT FROM 'completed_publication'
        AND release_evidence IS DISTINCT FROM 'completed_publication')
    OR COALESCE(
        completion_identity IS NOT NULL
        AND completed_publication_receipt = completion_identity || jsonb_build_object(
            'observed_branch_head', completed_publication_receipt->>'observed_branch_head')
        AND completed_publication_receipt->>'observed_branch_head' ~ '^[0-9a-f]{40}$'
        AND state = 'released' AND final_disposition = 'completed_publication'
        AND release_evidence = 'completed_publication' AND released_at IS NOT NULL
        AND live_worker_id IS NULL AND live_run_id IS NULL
        AND final_capture_id IS NULL AND final_source_sha IS NULL AND final_coverage_digest IS NULL
        AND completed_publication_reason IS NULL, false));

-- The report's head is stored only on the actual completed transition. Later
-- MR reconciliation and completed-state replacements cannot mint an identity.
-- +goose StatementBegin
CREATE FUNCTION completed_publication_run_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.completion_final_head IS NOT NULL THEN RETURN NULL; END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'completed' AND NEW.status = 'completed'
        AND NEW.completion_final_head IS DISTINCT FROM OLD.completion_final_head THEN RETURN NULL; END IF;
    IF NEW.status <> 'completed' THEN NEW.completion_final_head := NULL; END IF;
    IF NEW.completion_final_head IS NOT NULL AND OLD.status <> 'completed' THEN
        IF NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id AND w.user_id = NEW.user_id
            AND 'recovery_completed_publication_v1' = ANY(w.protocol_capabilities)) THEN
            NEW.completion_final_head := NULL;
        END IF;
        IF NEW.completion_contract_version IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM run_completion_permits p WHERE p.run_id = NEW.id
                AND p.issued_by_worker_id = NEW.worker_id AND p.head = NEW.completion_final_head
                AND p.consumed_at IS NOT NULL AND p.contract_revision = NEW.contract_revision
        ) THEN RETURN NULL; END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER completed_publication_run_guard BEFORE INSERT OR UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION completed_publication_run_guard();

-- +goose StatementBegin
CREATE FUNCTION completed_publication_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'completed' OR NEW.status <> 'completed' OR NEW.completion_final_head IS NULL
        OR NEW.kind NOT IN ('issue', 'mr_rework') OR NEW.claim_released_at IS NOT NULL THEN RETURN NEW; END IF;
    UPDATE recovery_custody_holds h SET completion_identity = jsonb_build_object(
        'hold_id', h.id, 'run_id', NEW.id, 'owner_id', NEW.user_id,
        'worker_id', NEW.worker_id, 'generation', NEW.claim_generation,
        'final_head', NEW.completion_final_head, 'repo_id', r.id,
        'connection_id', c.id, 'project_id', r.forge_project_id,
        'forge_type', c.forge_type, 'base_url', c.base_url,
        'branch', CASE WHEN NEW.kind = 'issue' THEN 'agent/issue-' || NEW.issue_iid::text ELSE NEW.pipeline_ref END,
        'mr_iid', NEW.mr_iid), updated_at = now()
    FROM repos r JOIN forge_connections c ON c.id = r.connection_id
    WHERE r.id = NEW.repo_id AND c.user_id = NEW.user_id
        AND h.run_id = NEW.id AND h.user_id = NEW.user_id AND h.repo_id = NEW.repo_id
        AND h.original_worker_id = NEW.worker_id AND h.generation = NEW.claim_generation
        AND h.inventory_guarded AND h.state = 'open' AND h.completion_identity IS NULL
        AND h.live_worker_id = NEW.worker_id AND h.live_run_id = NEW.id;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER completed_publication_stamp AFTER UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION completed_publication_stamp();

-- +goose StatementBegin
CREATE FUNCTION completed_publication_hold_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.completion_identity IS NOT NULL OR NEW.completed_publication_receipt IS NOT NULL
            OR NEW.completed_publication_reason IS NOT NULL
            OR NEW.final_disposition = 'completed_publication'
            OR NEW.release_evidence = 'completed_publication' THEN RETURN NULL; END IF;
        RETURN NEW;
    END IF;
    IF OLD.completion_identity IS NOT NULL AND NEW.completion_identity IS DISTINCT FROM OLD.completion_identity THEN RETURN NULL; END IF;
    IF OLD.completed_publication_receipt IS NOT NULL AND
        (NEW.completed_publication_receipt, NEW.completed_publication_reason)
        IS DISTINCT FROM (OLD.completed_publication_receipt, OLD.completed_publication_reason) THEN RETURN NULL; END IF;
    IF OLD.state <> 'open' AND
        (NEW.completion_identity, NEW.completed_publication_receipt, NEW.completed_publication_reason)
        IS DISTINCT FROM (OLD.completion_identity, OLD.completed_publication_receipt, OLD.completed_publication_reason) THEN RETURN NULL; END IF;
    IF OLD.completion_identity IS NULL AND NEW.completion_identity IS NOT NULL THEN
        -- Only the nested completion stamp may initialize identity.
        IF pg_trigger_depth() < 2 OR NOT NEW.inventory_guarded OR NEW.state <> 'open' THEN RETURN NULL; END IF;
        IF NOT COALESCE(NEW.completion_identity->>'hold_id' = NEW.id::text
            AND NEW.completion_identity->>'run_id' = NEW.run_id::text
            AND NEW.completion_identity->>'owner_id' = NEW.user_id::text
            AND NEW.completion_identity->>'worker_id' = NEW.original_worker_id::text
            AND NEW.completion_identity->>'generation' = NEW.generation::text
            AND NEW.completion_identity->>'repo_id' = NEW.repo_id::text
            AND NEW.completion_identity->>'final_head' ~ '^[0-9a-f]{40}$', false) THEN RETURN NULL; END IF;
    END IF;
    IF NEW.completed_publication_reason IS DISTINCT FROM OLD.completed_publication_reason
        AND NEW.completed_publication_reason IS NOT NULL
        AND (OLD.state <> 'open' OR OLD.completion_identity IS NULL OR OLD.completed_publication_receipt IS NOT NULL) THEN RETURN NULL; END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER completed_publication_hold_guard BEFORE INSERT OR UPDATE ON recovery_custody_holds
    FOR EACH ROW EXECUTE FUNCTION completed_publication_hold_guard();

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
                        WHEN r.kind = 'mr_rework' THEN r.pipeline_ref END = OLD.completion_identity->>'branch') THEN
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
-- The older schema cannot retain completion identities, receipts, reasons or final heads.
-- Remove only the new classifications; released custody and its timestamps remain intact.
DROP TRIGGER completed_publication_run_guard ON runs;
DROP TRIGGER completed_publication_stamp ON runs;
DROP TRIGGER completed_publication_hold_guard ON recovery_custody_holds;
DROP FUNCTION completed_publication_run_guard();
DROP FUNCTION completed_publication_stamp();
DROP FUNCTION completed_publication_hold_guard();
ALTER TABLE recovery_custody_holds DROP CONSTRAINT completed_publication_receipt_check;

-- Temporarily allow normalization through the otherwise immutable inventory guard.
ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard;
UPDATE recovery_custody_holds SET final_disposition = NULL
    WHERE final_disposition = 'completed_publication';
UPDATE recovery_custody_holds SET release_evidence = NULL
    WHERE release_evidence = 'completed_publication';
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
        'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry'));
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_final_disposition_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_final_disposition_check
    CHECK (final_disposition IN ('archive', 'settled', 'no_adopted_source'));

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

ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard;
ALTER TABLE recovery_custody_holds
    DROP COLUMN completion_identity,
    DROP COLUMN completed_publication_receipt,
    DROP COLUMN completed_publication_reason;
ALTER TABLE runs DROP COLUMN completion_final_head;
