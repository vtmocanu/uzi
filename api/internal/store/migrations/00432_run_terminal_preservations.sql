-- +goose Up
-- Issue #4007: a run's terminal preservation. One row per run records the single forge ref
-- refs/uzi-terminal/<run-id> that keeps a terminal run's published work alive, its lifecycle and
-- its retirement decision. The per-generation evidence for each push lives in
-- run_terminal_contributions (the next migration).
--
-- Additive: one brand-new table. Its only busy parent is runs, through the named live-pointer FK;
-- the CREATE takes a brief lock there, so the statement times out rather than queue behind a long
-- writer and stall claims.
--
-- Nothing here cascades from runs, repos or owners (the ADR-1296 rule): user_id and repo_id are
-- plain provenance, and the nullable live_run_id is the ON DELETE RESTRICT pointer held while a
-- remote ref may exist for the row (the run_held_publications precedent, migration 00333). The
-- repo and connection removal handlers match the FK by name.
--
-- The migration numbers are drafts: they are renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

CREATE TABLE run_terminal_preservations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- One preservation per run. Plain provenance, no FK: the row outlives the run's own pointer.
    run_id uuid NOT NULL CONSTRAINT run_terminal_preservations_run_id_key UNIQUE,
    user_id uuid NOT NULL,
    repo_id uuid NOT NULL,
    -- The LIVE pointer: set while a remote ref may exist (publishing, live, retiring), cleared by
    -- the transition into a state that cannot own one. NAMED because the run, repo and connection
    -- removal handlers match the 23503 by constraint name.
    live_run_id uuid CONSTRAINT run_terminal_preservations_live_run_id_fkey REFERENCES runs(id) ON DELETE RESTRICT
        CONSTRAINT run_terminal_preservations_live_run_is_own_check CHECK (live_run_id IS NULL OR live_run_id = run_id),
    ref text NOT NULL CONSTRAINT run_terminal_preservations_ref_check
        CHECK (ref = 'refs/uzi-terminal/' || run_id::text),
    -- The tip the ref was last confirmed at; NULL until a first publish.
    tip text CONSTRAINT run_terminal_preservations_tip_check
        CHECK (tip ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    -- Audit only: how the run's lifetime was classified at bind. Only no_pr may become pr.
    lifetime_at_bind text NOT NULL CONSTRAINT run_terminal_preservations_lifetime_check
        CHECK (lifetime_at_bind IN ('issue', 'pr', 'no_pr')),
    state text NOT NULL DEFAULT 'bound' CONSTRAINT run_terminal_preservations_state_check CHECK (state IN (
        'bound',          -- recorded; no push has been sent
        'publishing',     -- a create or update may be in flight; the live pointer is set
        'live',           -- the ref was read back at the tip
        'retiring',       -- irrevocable: the ref is being deleted
        'deleted',        -- the ref was CAS-deleted or confirmed absent
        'abandoned',      -- the ref is at a foreign tip and is never touched
        'not_published'   -- no ref was ever created
    )),
    first_published_at timestamptz,
    retire_tip text CONSTRAINT run_terminal_preservations_retire_tip_check
        CHECK (retire_tip ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    retire_reason text CONSTRAINT run_terminal_preservations_retire_reason_check CHECK (retire_reason IN (
        'issue_closed', 'pr_closed', 'pr_merged', 'no_pr_expired', 'closed_at_bind')),
    retire_observed_at timestamptz,
    retire_decided_at timestamptz,
    next_check_at timestamptz NOT NULL DEFAULT now(),
    attempts int NOT NULL DEFAULT 0,
    last_error text CONSTRAINT run_terminal_preservations_last_error_check CHECK (length(last_error) <= 512),
    deleted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- The composite FK target of run_terminal_contributions.
    CONSTRAINT run_terminal_preservations_id_run_id_key UNIQUE (id, run_id),
    CONSTRAINT run_terminal_preservations_live_pointer_check
        CHECK ((state IN ('publishing', 'live', 'retiring')) = (live_run_id IS NOT NULL)),
    CONSTRAINT run_terminal_preservations_deleted_at_check
        CHECK ((state = 'deleted') = (deleted_at IS NOT NULL)),
    CONSTRAINT run_terminal_preservations_retire_stamp_check
        CHECK (state NOT IN ('retiring', 'deleted') OR (retire_tip IS NOT NULL AND retire_reason IS NOT NULL
            AND retire_observed_at IS NOT NULL AND retire_decided_at IS NOT NULL)),
    -- A live row has read the ref back at a tip.
    CONSTRAINT run_terminal_preservations_live_tip_check
        CHECK (state <> 'live' OR tip IS NOT NULL),
    -- not_published means no ref was ever created: a row that recorded a tip or a first publish
    -- may own a remote ref and can never become one.
    CONSTRAINT run_terminal_preservations_not_published_check
        CHECK (state <> 'not_published' OR (tip IS NULL AND first_published_at IS NULL))
);

-- The sweep's due list, by due time, over the states that still have work.
CREATE INDEX idx_run_terminal_preservations_due ON run_terminal_preservations (next_check_at, id)
    WHERE state IN ('bound', 'publishing', 'live', 'retiring');
-- The RESTRICT FK's referencing side, so a run delete does not scan the table.
CREATE INDEX idx_run_terminal_preservations_live_run ON run_terminal_preservations (live_run_id)
    WHERE live_run_id IS NOT NULL;
-- The repo and connection removal guards.
CREATE INDEX idx_run_terminal_preservations_live_repo ON run_terminal_preservations (repo_id)
    WHERE live_run_id IS NOT NULL;

-- The row's guard. A rejected change returns NULL, so the UPDATE reports 0 rows (the
-- held_publication_guard convention) and a CAS writer reads a lost race.
-- +goose StatementBegin
CREATE FUNCTION run_terminal_preservation_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Identity never changes.
    IF (NEW.id, NEW.run_id, NEW.user_id, NEW.repo_id, NEW.ref, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.run_id, OLD.user_id, OLD.repo_id, OLD.ref, OLD.created_at) THEN
        RETURN NULL;
    END IF;
    -- The lifetime classification is audit only; the one legal change is a no-PR run gaining a PR.
    IF NEW.lifetime_at_bind IS DISTINCT FROM OLD.lifetime_at_bind
        AND NOT (OLD.lifetime_at_bind = 'no_pr' AND NEW.lifetime_at_bind = 'pr') THEN
        RETURN NULL;
    END IF;
    -- Set once, never renewed or cleared.
    IF OLD.first_published_at IS NOT NULL AND NEW.first_published_at IS DISTINCT FROM OLD.first_published_at THEN
        RETURN NULL;
    END IF;
    -- The retirement decision is immutable once it is made.
    IF OLD.retire_tip IS NOT NULL AND
        (NEW.retire_tip, NEW.retire_reason, NEW.retire_observed_at, NEW.retire_decided_at)
        IS DISTINCT FROM (OLD.retire_tip, OLD.retire_reason, OLD.retire_observed_at, OLD.retire_decided_at) THEN
        RETURN NULL;
    END IF;
    IF NEW.state = OLD.state THEN
        -- A finished row is frozen.
        IF OLD.state IN ('deleted', 'abandoned') AND NEW IS DISTINCT FROM OLD THEN RETURN NULL; END IF;
    ELSIF NOT (
        (OLD.state = 'bound' AND NEW.state IN ('publishing', 'not_published'))
        OR (OLD.state = 'not_published' AND NEW.state = 'publishing')
        OR (OLD.state = 'publishing' AND NEW.state IN ('live', 'not_published'))
        OR (OLD.state = 'live' AND NEW.state IN ('publishing', 'retiring'))
        -- Irrevocable: a retiring row only ever finishes.
        OR (OLD.state = 'retiring' AND NEW.state IN ('deleted', 'abandoned'))
    ) THEN
        RETURN NULL;
    END IF;
    -- No ref was ever created: refuse while a push of this preservation may have created one
    -- (invoked, publish_unknown) or did (published). The tip and first_published_at are held to
    -- NULL by the not_published CHECK, but a contribution can be ahead of the preservation row.
    IF NEW.state = 'not_published' AND OLD.state <> 'not_published' THEN
        IF EXISTS (SELECT 1 FROM run_terminal_contributions c
            WHERE c.preservation_id = NEW.id AND c.state IN ('invoked', 'publish_unknown', 'published')) THEN
            RETURN NULL;
        END IF;
    END IF;
    -- The tip moves forward only to the tip of a published contribution of this preservation,
    -- and only from the tip that contribution was prepared against (or from no tip at all).
    IF NEW.tip IS DISTINCT FROM OLD.tip THEN
        IF NEW.tip IS NULL OR OLD.state IN ('retiring', 'deleted', 'abandoned') THEN RETURN NULL; END IF;
        IF NOT EXISTS (SELECT 1 FROM run_terminal_contributions c
            WHERE c.preservation_id = NEW.id AND c.state = 'published' AND c.tip = NEW.tip
              AND (OLD.tip IS NULL OR c.prior_tip = OLD.tip)) THEN
            RETURN NULL;
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_terminal_preservation_guard BEFORE UPDATE ON run_terminal_preservations
    FOR EACH ROW EXECUTE FUNCTION run_terminal_preservation_guard();

-- A row is born bound: no INSERT may carry a later state or any lifecycle marker, so the
-- transition matrix above cannot be bypassed by inserting a row already past it.
-- +goose StatementBegin
CREATE FUNCTION run_terminal_preservation_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'bound' OR NEW.tip IS NOT NULL OR NEW.first_published_at IS NOT NULL
        OR NEW.retire_tip IS NOT NULL OR NEW.retire_reason IS NOT NULL
        OR NEW.retire_observed_at IS NOT NULL OR NEW.retire_decided_at IS NOT NULL
        OR NEW.deleted_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_terminal_preservation_insert_guard BEFORE INSERT ON run_terminal_preservations
    FOR EACH ROW EXECUTE FUNCTION run_terminal_preservation_insert_guard();

-- A row that bears the live pointer is, or may be, the only record of a remote ref: never deleted.
-- +goose StatementBegin
CREATE FUNCTION run_terminal_preservation_delete_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.live_run_id IS NOT NULL THEN RETURN NULL; END IF;
    RETURN OLD;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_terminal_preservation_delete_guard BEFORE DELETE ON run_terminal_preservations
    FOR EACH ROW EXECUTE FUNCTION run_terminal_preservation_delete_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- The previous migration's Down drops run_terminal_contributions before this one runs. Until the
-- DROP FUNCTION below, run_terminal_preservation_guard still names that table inside its nested IFs,
-- which plpgsql plans only when a branch is reached: a preservation UPDATE that reaches one in that
-- window would fail. Nothing here exercises that window; roll the chain back with writers stopped.
-- Close the check-then-drop window: no live row may be inserted between the check and the DROP.
LOCK TABLE run_terminal_preservations IN SHARE ROW EXCLUSIVE MODE;
-- A pointer-bearing row owns (or may own) a remote ref that nothing else records: refuse the
-- rollback rather than forget it. Drain or delete the refs/uzi-terminal refs first.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM run_terminal_preservations WHERE live_run_id IS NOT NULL) THEN
        RAISE EXCEPTION 'run_terminal_preservations has live rows; delete the refs/uzi-terminal refs first';
    END IF;
END $$;
-- +goose StatementEnd

DROP TABLE run_terminal_preservations;
DROP FUNCTION run_terminal_preservation_guard();
DROP FUNCTION run_terminal_preservation_insert_guard();
DROP FUNCTION run_terminal_preservation_delete_guard();
