-- +goose Up
-- Issue #2545: a failed run's held work is published to a forge ref
-- refs/uzi-held/<run-id>/<generation> before custody is released, instead of an archive.
--
-- Additive: one brand-new table plus one nullable column on recovery_custody_holds. The
-- hold's CHECKs are widened (relaxed) with the new 'held_publication' class and its
-- recovery_inventory_hold_guard branch is added. The ALTERs below take brief locks on a table
-- ClaimRun writes, so the statement times out rather than queue behind it and stall claims.
--
-- Nothing here cascades from runs, repos or owners (the ADR-1296 rule): run_id, user_id and
-- repo_id are plain provenance, and the nullable live_run_id is the ON DELETE RESTRICT pointer
-- (the run_salvage precedent) held while a remote ref may exist for the row.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

CREATE TABLE run_held_publications (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Plain provenance, no FK: the row outlives the run. UNIQUE per (run, generation).
    run_id uuid NOT NULL,
    generation bigint NOT NULL,
    hold_id uuid NOT NULL REFERENCES recovery_custody_holds(id) ON DELETE RESTRICT,
    user_id uuid NOT NULL,
    repo_id uuid NOT NULL,
    worker_id uuid NOT NULL,
    -- The LIVE pointer: set from the first insert, cleared by the transition into a state
    -- that can no longer own a remote ref (refused, deleted, abandoned). The constraint is
    -- NAMED because the run, repo and connection removal handlers match it by name.
    live_run_id uuid CONSTRAINT run_held_publications_live_run_id_fkey REFERENCES runs(id) ON DELETE RESTRICT
        CONSTRAINT run_held_publications_live_run_is_own_check CHECK (live_run_id IS NULL OR live_run_id = run_id),
    ref text NOT NULL CONSTRAINT run_held_publications_ref_check
        CHECK (ref = 'refs/uzi-held/' || run_id::text || '/' || generation::text),
    tip text NOT NULL CONSTRAINT run_held_publications_tip_check
        CHECK (tip ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    coverage_digest text NOT NULL CONSTRAINT run_held_publications_coverage_digest_check
        CHECK (coverage_digest ~ '^[0-9a-f]{64}$'),
    state text NOT NULL CONSTRAINT run_held_publications_state_check CHECK (state IN (
        'prepared',        -- recorded; no create has been sent
        'invoked',         -- create_invoked_at committed; the create may have been sent
        'created',         -- the ref was read back at the tip
        'create_unknown',  -- the create outcome is unknown; reconcile only
        'refused',         -- a received report carried an explicit ng; no ref was created
        'acknowledged',    -- the hold was released against this publication; retention runs
        'delete_unknown',  -- a delete was attempted and its outcome is unknown
        'deleted',         -- the ref was CAS-deleted or confirmed absent
        'abandoned'        -- the ref is at a foreign tip and is never touched
    )),
    attempts int NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    create_invoked_at timestamptz,
    ref_created_at timestamptz,
    acknowledged_at timestamptz,
    expires_at timestamptz,
    owner_expired_at timestamptz,
    deleted_at timestamptz,
    next_attempt_at timestamptz,
    last_error text CONSTRAINT run_held_publications_last_error_check CHECK (length(last_error) <= 512),
    refusal_reason text CONSTRAINT run_held_publications_refusal_reason_check CHECK (refusal_reason IN (
        'create_refused', 'ref_missing', 'ref_mismatch', 'forge_timeout', 'not_failed',
        'excluded_origin', 'generation_mismatch', 'identity_changed', 'coverage_mismatch',
        'foreign_tip')),
    CONSTRAINT run_held_publications_run_generation_key UNIQUE (run_id, generation),
    CONSTRAINT run_held_publications_live_pointer_check
        CHECK ((state IN ('refused', 'deleted', 'abandoned')) = (live_run_id IS NULL)),
    CONSTRAINT run_held_publications_invoked_marker_check
        CHECK ((state = 'prepared') = (create_invoked_at IS NULL)),
    CONSTRAINT run_held_publications_acknowledged_state_check
        CHECK (acknowledged_at IS NULL OR state IN ('acknowledged', 'delete_unknown', 'deleted', 'abandoned')),
    CONSTRAINT run_held_publications_acknowledged_stamp_check
        CHECK (state <> 'acknowledged' OR (acknowledged_at IS NOT NULL AND expires_at IS NOT NULL))
);

-- The reconcile and delete-retry lane, by due time.
CREATE INDEX idx_run_held_publications_due ON run_held_publications (next_attempt_at)
    WHERE state IN ('invoked', 'create_unknown', 'delete_unknown');
-- The expiry lane.
CREATE INDEX idx_run_held_publications_expiry ON run_held_publications (expires_at)
    WHERE state = 'acknowledged';
-- The orphan lane: a created, never-acknowledged ref whose hold may have closed.
CREATE INDEX idx_run_held_publications_orphan ON run_held_publications (hold_id)
    WHERE state = 'created';
-- The repo and connection removal guards, and the RESTRICT FK's referencing side.
CREATE INDEX idx_run_held_publications_live_repo ON run_held_publications (repo_id)
    WHERE live_run_id IS NOT NULL;
CREATE INDEX idx_run_held_publications_live_run ON run_held_publications (live_run_id)
    WHERE live_run_id IS NOT NULL;

ALTER TABLE recovery_custody_holds ADD COLUMN final_publication_id uuid;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
        'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry', 'completed_publication',
        'held_publication'));
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_final_disposition_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_final_disposition_check
    CHECK (final_disposition IN ('archive', 'settled', 'no_adopted_source', 'completed_publication',
        'held_publication'));
-- NULL-safe in both directions: a plain comparison against a NULL disposition would pass.
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_held_publication_tie_check
    CHECK ((final_disposition IS NOT DISTINCT FROM 'held_publication') = (final_publication_id IS NOT NULL));
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_held_publication_evidence_check
    CHECK ((release_evidence IS NOT DISTINCT FROM 'held_publication')
        = (final_disposition IS NOT DISTINCT FROM 'held_publication'));
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_held_publication_shape_check
    CHECK (final_disposition IS DISTINCT FROM 'held_publication' OR COALESCE(
        state = 'released' AND release_evidence = 'held_publication' AND final_capture_id IS NULL
        AND final_source_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$' AND final_coverage_digest IS NOT NULL
        AND released_at IS NOT NULL AND live_worker_id IS NULL AND live_run_id IS NULL, false));

-- +goose StatementBegin
CREATE FUNCTION held_publication_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.run_id, NEW.generation, NEW.hold_id, NEW.user_id, NEW.repo_id, NEW.worker_id,
        NEW.ref, NEW.tip, NEW.coverage_digest)
        IS DISTINCT FROM (OLD.run_id, OLD.generation, OLD.hold_id, OLD.user_id, OLD.repo_id,
        OLD.worker_id, OLD.ref, OLD.tip, OLD.coverage_digest) THEN
        RETURN NULL;
    END IF;
    IF OLD.create_invoked_at IS NOT NULL AND NEW.create_invoked_at IS DISTINCT FROM OLD.create_invoked_at THEN
        RETURN NULL;
    END IF;
    IF OLD.acknowledged_at IS NOT NULL AND NEW.acknowledged_at IS DISTINCT FROM OLD.acknowledged_at THEN
        RETURN NULL;
    END IF;
    IF NEW.state = OLD.state THEN
        -- A terminal row is frozen.
        IF OLD.state IN ('refused', 'deleted', 'abandoned') AND NEW IS DISTINCT FROM OLD THEN RETURN NULL; END IF;
        RETURN NEW;
    END IF;
    IF NOT (
        (OLD.state = 'prepared' AND NEW.state = 'invoked')
        OR (OLD.state = 'invoked' AND NEW.state IN ('created', 'refused', 'create_unknown'))
        OR (OLD.state = 'create_unknown' AND NEW.state IN ('created', 'refused'))
        OR (OLD.state = 'created' AND NEW.state IN ('acknowledged', 'delete_unknown', 'deleted', 'abandoned'))
        OR (OLD.state = 'acknowledged' AND NEW.state IN ('delete_unknown', 'deleted', 'abandoned'))
        OR (OLD.state = 'delete_unknown' AND NEW.state IN ('deleted', 'abandoned'))
    ) THEN
        RETURN NULL;
    END IF;
    IF OLD.state = 'created' AND NEW.state = 'acknowledged' THEN
        -- The hold is released first (its guard needs state = 'created'), so the release is
        -- already visible here.
        IF NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.id = NEW.hold_id
            AND h.state = 'released' AND h.final_disposition = 'held_publication'
            AND h.final_publication_id = NEW.id) THEN
            RETURN NULL;
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER held_publication_guard BEFORE UPDATE ON run_held_publications
    FOR EACH ROW EXECUTE FUNCTION held_publication_guard();

-- A hold is never born released: no INSERT may carry the held classification.
-- +goose StatementBegin
CREATE FUNCTION held_publication_hold_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.final_publication_id IS NOT NULL OR NEW.final_disposition = 'held_publication'
        OR NEW.release_evidence = 'held_publication' THEN RETURN NULL; END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER held_publication_hold_insert_guard BEFORE INSERT ON recovery_custody_holds
    FOR EACH ROW EXECUTE FUNCTION held_publication_hold_insert_guard();

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
-- A live publication row owns (or may own) a remote ref that nothing else records: refuse the
-- rollback rather than forget it. Drain or delete the refs first.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM run_held_publications
        WHERE state IN ('invoked', 'created', 'create_unknown', 'acknowledged', 'delete_unknown')) THEN
        RAISE EXCEPTION 'run_held_publications has live rows; delete the refs/uzi-held refs first';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER held_publication_hold_insert_guard ON recovery_custody_holds;
DROP FUNCTION held_publication_hold_insert_guard();
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_held_publication_shape_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_held_publication_evidence_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_held_publication_tie_check;

-- Temporarily allow normalization through the otherwise immutable inventory guard. The older
-- schema cannot represent the classification: clear it, never delete the hold record.
ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard;
UPDATE recovery_custody_holds SET final_disposition = NULL, final_publication_id = NULL,
        final_source_sha = NULL, final_coverage_digest = NULL
    WHERE final_disposition = 'held_publication';
UPDATE recovery_custody_holds SET release_evidence = NULL WHERE release_evidence = 'held_publication';
ALTER TABLE recovery_custody_holds DROP COLUMN final_publication_id;
DROP TABLE run_held_publications;
DROP FUNCTION held_publication_guard();
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN ('publication', 'archive', 'forge_no_output',
        'owner_discard', 'no_adopted_source', 'ancestry', 'live_ancestry', 'completed_publication'));
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_final_disposition_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_final_disposition_check
    CHECK (final_disposition IN ('archive', 'settled', 'no_adopted_source', 'completed_publication'));

-- Restore the guard installed by migration 00319.
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

ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard;
