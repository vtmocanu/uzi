-- +goose Up
-- Issue #4007: one row per (run, generation) recording what that generation contributed to the
-- run's terminal preservation (run_terminal_preservations, the previous migration): the tip it
-- pushed, the tip it was prepared against, the server-canonicalised coverage manifest and its
-- digest, and the push lifecycle. A contribution with disposition other than 'preserved' records
-- an abandonment decision and never pushes.
--
-- Additive: one brand-new table. Its one busy parent is recovery_custody_holds, through the
-- nullable hold_id FK (only an open_guarded_hold contribution names a hold); the CREATE takes a
-- brief lock there, so the statement times out rather than queue behind a writer. The
-- preservation parent is new and not busy. run_id is plain provenance with no FK to runs.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

CREATE TABLE run_terminal_contributions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    preservation_id uuid NOT NULL,
    run_id uuid NOT NULL,
    generation bigint NOT NULL CONSTRAINT run_terminal_contributions_generation_check CHECK (generation > 0),
    worker_id uuid NOT NULL,
    user_id uuid NOT NULL,
    hold_id uuid CONSTRAINT run_terminal_contributions_hold_id_fkey REFERENCES recovery_custody_holds(id) ON DELETE RESTRICT,
    authority text NOT NULL CONSTRAINT run_terminal_contributions_authority_check
        CHECK (authority IN ('current_flight', 'open_guarded_hold')),
    tip text NOT NULL CONSTRAINT run_terminal_contributions_tip_check
        CHECK (tip ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    prior_tip text CONSTRAINT run_terminal_contributions_prior_tip_check
        CHECK (prior_tip ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    -- The server-computed sha256 of the canonical manifest.
    coverage_digest text NOT NULL CONSTRAINT run_terminal_contributions_coverage_digest_check
        CHECK (coverage_digest ~ '^[0-9a-f]{64}$'),
    manifest jsonb NOT NULL CONSTRAINT run_terminal_contributions_manifest_check
        CHECK (jsonb_typeof(manifest) = 'object' AND octet_length(manifest::text) <= 262144),
    disposition text NOT NULL CONSTRAINT run_terminal_contributions_disposition_check
        CHECK (disposition IN ('preserved', 'closed_issue_abandoned', 'closed_pr_abandoned')),
    state text NOT NULL CONSTRAINT run_terminal_contributions_state_check CHECK (state IN (
        'prepared',         -- recorded; no push has been sent
        'invoked',          -- create_invoked_at committed; the push may have been sent
        'published',        -- the ref was read back at the tip
        'publish_unknown',  -- the push outcome is unknown; reconcile only
        'refused',          -- a received report carried an explicit refusal; nothing was published
        'recorded'          -- an abandonment decision was recorded; nothing was pushed
    )),
    refusal_reason text CONSTRAINT run_terminal_contributions_refusal_reason_check CHECK (refusal_reason IN (
        'create_refused', 'update_refused', 'foreign_tip')),
    create_invoked_at timestamptz,
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT run_terminal_contributions_preservation_fkey FOREIGN KEY (preservation_id, run_id)
        REFERENCES run_terminal_preservations (id, run_id) ON DELETE RESTRICT,
    CONSTRAINT run_terminal_contributions_run_generation_key UNIQUE (run_id, generation),
    CONSTRAINT run_terminal_contributions_hold_authority_check
        CHECK ((authority = 'open_guarded_hold') = (hold_id IS NOT NULL)),
    -- Only a preserved contribution has a push lifecycle; an abandonment is simply recorded.
    CONSTRAINT run_terminal_contributions_recorded_check
        CHECK ((disposition = 'preserved') = (state <> 'recorded')),
    CONSTRAINT run_terminal_contributions_invoked_marker_check
        CHECK (disposition <> 'preserved' OR (state = 'prepared') = (create_invoked_at IS NULL)),
    CONSTRAINT run_terminal_contributions_abandoned_never_invoked_check
        CHECK (disposition = 'preserved' OR create_invoked_at IS NULL),
    -- A refusal reason belongs to a refused contribution, and a refused contribution always has one.
    CONSTRAINT run_terminal_contributions_refusal_state_check
        CHECK ((state = 'refused') = (refusal_reason IS NOT NULL)),
    CONSTRAINT run_terminal_contributions_published_stamp_check
        CHECK ((state = 'published') = (published_at IS NOT NULL))
);

CREATE INDEX idx_run_terminal_contributions_preservation ON run_terminal_contributions (preservation_id);
-- At most one unresolved push per preservation: a second concurrent invoker is refused by the
-- database, not only by the writers' guards.
CREATE UNIQUE INDEX idx_run_terminal_contributions_unresolved ON run_terminal_contributions (preservation_id)
    WHERE state IN ('invoked', 'publish_unknown');
-- The RESTRICT FK's referencing side, so a hold delete does not scan the table.
CREATE INDEX idx_run_terminal_contributions_hold ON run_terminal_contributions (hold_id)
    WHERE hold_id IS NOT NULL;

-- A rejected change returns NULL, so the UPDATE reports 0 rows and a CAS writer reads a lost race.
-- +goose StatementBegin
CREATE FUNCTION run_terminal_contribution_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Identity, manifest and digest never change.
    IF (NEW.id, NEW.preservation_id, NEW.run_id, NEW.generation, NEW.worker_id, NEW.user_id, NEW.hold_id,
        NEW.authority, NEW.tip, NEW.prior_tip, NEW.coverage_digest, NEW.manifest, NEW.disposition,
        NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.preservation_id, OLD.run_id, OLD.generation, OLD.worker_id, OLD.user_id,
        OLD.hold_id, OLD.authority, OLD.tip, OLD.prior_tip, OLD.coverage_digest, OLD.manifest,
        OLD.disposition, OLD.created_at) THEN
        RETURN NULL;
    END IF;
    -- Set once, never renewed or cleared.
    IF OLD.create_invoked_at IS NOT NULL AND NEW.create_invoked_at IS DISTINCT FROM OLD.create_invoked_at THEN
        RETURN NULL;
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RETURN NULL;
    END IF;
    IF NEW.state = OLD.state THEN
        -- A finished row is frozen.
        IF OLD.state IN ('published', 'refused', 'recorded') AND NEW IS DISTINCT FROM OLD THEN RETURN NULL; END IF;
        RETURN NEW;
    END IF;
    IF NOT (
        (OLD.state = 'prepared' AND NEW.state = 'invoked')
        OR (OLD.state = 'invoked' AND NEW.state IN ('published', 'refused', 'publish_unknown'))
        OR (OLD.state = 'publish_unknown' AND NEW.state IN ('published', 'refused'))
    ) THEN
        RETURN NULL;
    END IF;
    -- A push may be marked only while its preservation is publishing. The preservation row is
    -- locked FOR SHARE, so a concurrent preservation -> not_published (which needs FOR UPDATE on that
    -- row) either commits first and this marker is refused, or waits for this transaction and then
    -- sees an invoked contribution and is refused by the preservation guard.
    IF OLD.state = 'prepared' AND NEW.state = 'invoked' THEN
        PERFORM 1 FROM run_terminal_preservations p
            WHERE p.id = NEW.preservation_id AND p.state = 'publishing' FOR SHARE;
        IF NOT FOUND THEN RETURN NULL; END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_terminal_contribution_guard BEFORE UPDATE ON run_terminal_contributions
    FOR EACH ROW EXECUTE FUNCTION run_terminal_contribution_guard();

-- A row is born prepared (or recorded, for an abandonment): no INSERT may carry a later state or
-- any lifecycle marker, so the transition matrix above cannot be bypassed.
-- +goose StatementBegin
CREATE FUNCTION run_terminal_contribution_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.create_invoked_at IS NOT NULL OR NEW.published_at IS NOT NULL OR NEW.refusal_reason IS NOT NULL THEN
        RETURN NULL;
    END IF;
    IF NOT ((NEW.disposition = 'preserved' AND NEW.state = 'prepared')
        OR (NEW.disposition <> 'preserved' AND NEW.state = 'recorded')) THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_terminal_contribution_insert_guard BEFORE INSERT ON run_terminal_contributions
    FOR EACH ROW EXECUTE FUNCTION run_terminal_contribution_insert_guard();

-- A row that is, or may be, the only record of a pushed tip is never deleted.
-- +goose StatementBegin
CREATE FUNCTION run_terminal_contribution_delete_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IN ('invoked', 'published', 'publish_unknown') THEN RETURN NULL; END IF;
    RETURN OLD;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_terminal_contribution_delete_guard BEFORE DELETE ON run_terminal_contributions
    FOR EACH ROW EXECUTE FUNCTION run_terminal_contribution_delete_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE run_terminal_contributions IN ACCESS EXCLUSIVE MODE;
-- A contribution whose ref may still exist is the only record of the tip pushed to it: refuse the
-- rollback rather than forget it. The ref may exist while its preservation bears the live pointer,
-- and an unresolved push (invoked, publish_unknown) may have created it. Drain or delete the
-- refs/uzi-terminal refs first.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM run_terminal_contributions c
        WHERE c.state IN ('invoked', 'publish_unknown')
           OR EXISTS (SELECT 1 FROM run_terminal_preservations p
               WHERE p.id = c.preservation_id AND p.live_run_id IS NOT NULL)) THEN
        RAISE EXCEPTION 'run_terminal_contributions has live rows; delete the refs/uzi-terminal refs first';
    END IF;
END $$;
-- +goose StatementEnd

DROP TABLE run_terminal_contributions;
DROP FUNCTION run_terminal_contribution_guard();
DROP FUNCTION run_terminal_contribution_insert_guard();
DROP FUNCTION run_terminal_contribution_delete_guard();
