-- +goose Up

-- The durable pre-rotation quarantine for a Codex subscription refresh (PRD #1147
-- M2), STORE/SCHEMA only — ships DARK. One row is one in-flight (or recently
-- resolved) rotation of a codex_provider_account (00199), keyed by the operation id
-- the service mints BEFORE it touches the provider. Its whole reason to exist is
-- process loss: the coordination fields on codex_provider_account (coord_state,
-- lease_deadline) tell a survivor that SOME operation held the lease, but not what
-- that operation was mid-way through. This table is the pre-rotation record written
-- durably first, so a refresher that crashes between "asked the provider to rotate"
-- and "committed the new material" leaves a 'rotating' intent a recovery scan can
-- find and reconcile, rather than an orphaned lease with no memory of the operation.
--
-- STATE MACHINE (driven by the m2 service, inert here):
--   'rotating'      — the intent is recorded; the provider rotation is in flight or
--                     its outcome is unknown. This is the only state a recovery scan
--                     acts on (see the partial index below).
--   'committed'     — the new material was durably committed to the account
--                     (CommitCodexRefresh advanced generation); the operation is done.
--   'unrecoverable' — the rotation cannot be resolved either way (e.g. the provider
--                     rotated but the commit was lost and no recovery slot holds the
--                     new material); the account needs operator/re-auth attention.
--   'reconciled'    — a recovery scan resolved a stranded intent (rolled back or
--                     reconciled to the account's actual generation).
--
-- The operation_id PRIMARY KEY is load-bearing for idempotence: the service reads an
-- existing intent first and a duplicate INSERT of the same operation fails on the PK,
-- so two refreshers cannot both believe they own the same operation.
CREATE TABLE codex_refresh_intent (
    -- The operation id the service mints before touching the provider; PK enforces
    -- one intent per operation (idempotence).
    operation_id        UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    provider_account_id UUID NOT NULL,

    -- The account generation this rotation started from. A recovery scan compares it
    -- against the account's current generation to decide whether the rotation
    -- committed (generation moved) or stranded (unchanged).
    from_generation     BIGINT NOT NULL,

    state               TEXT NOT NULL
        CHECK (state IN ('rotating', 'committed', 'unrecoverable', 'reconciled')),

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Owner-scoped composite FK to the account (00199's
    -- codex_provider_account_user_id_id_key): an intent can only reference an account
    -- the SAME user owns, and deleting the account cascades its intents away (an
    -- intent is meaningless without the account it rotates). ON DELETE CASCADE, not
    -- SET NULL: provider_account_id is NOT NULL here, so there is nothing to null to.
    FOREIGN KEY (user_id, provider_account_id)
        REFERENCES codex_provider_account (user_id, id) ON DELETE CASCADE
);

-- The recovery scan's index: find every unresolved ('rotating') intent for an
-- account. Partial so it stays small — a committed/reconciled intent is history the
-- scan never looks at.
CREATE INDEX idx_codex_refresh_intent_unresolved
    ON codex_refresh_intent (user_id, provider_account_id)
    WHERE state = 'rotating';

-- +goose Down
DROP TABLE codex_refresh_intent;
