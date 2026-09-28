-- +goose Up

-- PRD #1867 M2: the durable state of a failed run's checkpoint SALVAGE. Salvage is
-- CREATE-ONLY: when a failed, checkpoint-eligible run has a published checkpoint tip, the
-- api sweep creates a run-scoped refs/uzi-salvage/<run-id> at that tip
-- (pushbroker.CreateSalvageRef), only when refs/uzi-checkpoints/<branch> or
-- refs/uzi-recovery/<run-id> is verified at the recorded tip, and later CAS-deletes that
-- salvage ref when it expires. It never deletes or moves the branch checkpoint ref or a
-- recovery ref: #1810's retention owns those. This table records that lifecycle, one row
-- per run. The state name 'promoted' is kept; it means "a salvage copy was created".
--
-- Purely ADDITIVE: one brand-new table, no change to an existing one.
--
-- NOTHING here cascades from runs, repos or owners (the ADR-1296 rule: nothing
-- recovery-related cascades). A cascaded row would vanish while its public remote ref
-- survived, leaving nothing for the sweep to expire. So run_id, user_id and repo_id are
-- PLAIN provenance columns that outlive the run, and a separate nullable LIVE pointer,
-- live_run_id, is an ON DELETE RESTRICT FK to runs (the recovery_custody_holds pattern in
-- 00223). While a remote ref may still exist for the row (pending, promoted), the pointer is
-- set, so deleting the run (directly, or via the repo / forge-connection / owner cascade)
-- fails with 23503 on run_salvage_live_run_id_fkey instead of silently dropping the only
-- record of a public ref. Settling the row clears the pointer, which lifts the restriction.
-- The pointer tracks only the salvage ref: the branch checkpoint ref is #1810's, whatever
-- the row's state. A created salvage ref settles only as 'expired' or 'disabled', both
-- after it is CAS-deleted or confirmed absent. 'unavailable', 'refused' and 'failed' (the
-- attempt cap was reached first) created no salvage ref.
--
-- The repo-removal and forge-connection-removal handlers turn that restriction into an
-- owner-facing 409 naming the refs, and map a 23503 on this constraint (a sweep inserting a
-- row between their count and their delete) to the same 409. The constraint is NAMED
-- explicitly because the handlers match it by name.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
CREATE TABLE run_salvage (
    -- Provenance: the failed run this salvage belongs to. PRIMARY KEY, deliberately NOT a
    -- foreign key, so the row survives the run.
    run_id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    repo_id uuid NOT NULL,
    -- The forge kind of the repo's connection when the row was recorded (the per-forge
    -- UZI_SALVAGE_FORGES switch is keyed on it).
    forge_type text NOT NULL,
    -- The run's checkpoint branch (agent/issue-<n> or uzi/self-improve/<run-id>, exactly
    -- what workersvc.checkpointBranch derives): the branch-scoped ref is
    -- refs/uzi-checkpoints/<branch>.
    branch text NOT NULL CONSTRAINT run_salvage_branch_format_check
        CHECK (branch ~ '^(agent/issue-[0-9]+|uzi/self-improve/[0-9a-f-]{36})$'),
    -- The recorded checkpoint tip (runs.checkpoint_tip at enqueue). The create is bound to
    -- it: no salvage ref is created from a source at any other tip, and a salvage ref at any
    -- other tip is never overwritten. Full lowercase hex object id (SHA-1, or SHA-256 for a sha256 repo).
    tip text NOT NULL CHECK (tip ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    -- The LIVE pointer: ON DELETE RESTRICT while a remote ref may exist for this row. It
    -- only ever points at the row's own run.
    live_run_id uuid CONSTRAINT run_salvage_live_run_id_fkey REFERENCES runs(id) ON DELETE RESTRICT
        CONSTRAINT run_salvage_live_run_is_own_check CHECK (live_run_id IS NULL OR live_run_id = run_id),
    state text NOT NULL CHECK (state IN (
        'pending',        -- recorded; salvage create not yet confirmed (retried by the sweep)
        'promoted',       -- a salvage copy was created (the name is historical)
        'unavailable',    -- no source ref was at the recorded tip; nothing created
        'refused',        -- a salvage ref already existed at another tip; never overwritten
        'failed',         -- the create gave up after the attempt cap; nothing created
        'skipped_secret', -- fail_origin push_secret_blocked: never salvaged
        'expired',        -- the salvage ref was CAS-deleted (or confirmed absent)
        'disabled'        -- the forge was taken off UZI_SALVAGE_FORGES before the create
    )),
    attempts int NOT NULL DEFAULT 0,
    -- Bounded (and scrubbed by the caller) last broker error, shown on the run.
    last_error text CHECK (last_error IS NULL OR char_length(last_error) <= 512),
    -- Set once, when the salvage ref is confirmed to exist at the tip. From then on the row
    -- keeps its live pointer until the salvage ref is removed ('expired' or 'disabled').
    salvage_created_at timestamptz,
    promoted_at timestamptz,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- A created salvage ref is public until it is removed: once salvage_created_at is set,
    -- only 'expired' or 'disabled' (both settle only after the salvage ref is CAS-deleted or
    -- confirmed absent) may drop the live pointer.
    CONSTRAINT run_salvage_created_keeps_live_check
        CHECK (NOT (salvage_created_at IS NOT NULL AND state NOT IN ('expired', 'disabled')
                    AND live_run_id IS NULL)),
    -- 'promoted' means the salvage ref was created first.
    CONSTRAINT run_salvage_promoted_created_check
        CHECK (state <> 'promoted' OR salvage_created_at IS NOT NULL),
    -- The creation time and the expiry are recorded together, never one without the other.
    CONSTRAINT run_salvage_created_expires_pair_check
        CHECK ((salvage_created_at IS NULL) = (expires_at IS NULL)),
    -- A live row (a remote ref may exist) always holds the pointer.
    CONSTRAINT run_salvage_live_states_pointer_check
        CHECK (state NOT IN ('pending', 'promoted') OR live_run_id IS NOT NULL),
    -- A secret-blocked run never holds a ref, so it never blocks a delete.
    CONSTRAINT run_salvage_skipped_secret_no_pointer_check
        CHECK (state <> 'skipped_secret' OR live_run_id IS NULL)
);

-- The sweep's create pass: pending rows, least recently touched first.
CREATE INDEX idx_run_salvage_due_pending ON run_salvage (updated_at) WHERE state = 'pending';
-- The sweep's expiry pass: created salvage refs still live, by expiry.
CREATE INDEX idx_run_salvage_due_expiry ON run_salvage (expires_at)
    WHERE salvage_created_at IS NOT NULL AND state IN ('pending', 'promoted');
-- The RESTRICT FK's referencing side (a runs delete probes it) and the delete guards.
CREATE INDEX idx_run_salvage_live_run ON run_salvage (live_run_id) WHERE live_run_id IS NOT NULL;

-- +goose Down

DROP TABLE run_salvage;
