-- +goose Up
-- Completed self-improvement uses the server-owned run branch, even with a tracking issue.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION completed_publication_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'completed' OR NEW.status <> 'completed' OR NEW.completion_final_head IS NULL
        OR NEW.kind NOT IN ('issue', 'mr_rework', 'self_improve') OR NEW.claim_released_at IS NOT NULL THEN RETURN NEW; END IF;
    UPDATE recovery_custody_holds h SET completion_identity = jsonb_build_object(
        'hold_id', h.id, 'run_id', NEW.id, 'owner_id', NEW.user_id,
        'worker_id', NEW.worker_id, 'generation', NEW.claim_generation,
        'final_head', NEW.completion_final_head, 'repo_id', r.id,
        'connection_id', c.id, 'project_id', r.forge_project_id,
        'forge_type', c.forge_type, 'base_url', c.base_url,
        'branch', CASE WHEN NEW.kind = 'issue' THEN 'agent/issue-' || NEW.issue_iid::text
            WHEN NEW.kind = 'self_improve' THEN 'uzi/self-improve/' || NEW.id::text ELSE NEW.pipeline_ref END,
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION completed_publication_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
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
