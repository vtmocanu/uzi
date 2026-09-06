-- +goose Up

-- The account-keyed authoritative subscription state for a Codex login (PRD #1147
-- M1), STORE/SCHEMA only — ships DARK. One row is one Codex provider account as uzi
-- knows it: the sealed login material, the coordination/lease state a later
-- refresher (m2) drives, and the recovery slot. Per-alias link status is a separate
-- concern and lives in codex_credential_state (00199), which points HERE.
--
-- WHY ACCOUNT-KEYED, NOT ALIAS-KEYED. Several codex_auth aliases (user_secrets rows)
-- may resolve to the same provider account; the subscription's generation, revision
-- and refresh lease are properties of the ACCOUNT, not of any one alias, so they
-- live once here and the aliases reference this row rather than each carrying a
-- private copy that could drift.
CREATE TABLE codex_provider_account (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID NOT NULL,

    -- The canonical identity tuple, uzi-user-scoped. provider_user_id is the Codex
    -- account principal; workspace_account_id is the workspace/org it acts in. The
    -- SAME workspace with a DIFFERENT provider_user_id is a distinct account (a
    -- distinct row); identical tuples for one uzi user conflict on
    -- codex_provider_account_tuple_key below.
    provider_user_id     TEXT NOT NULL,
    workspace_account_id TEXT NOT NULL,

    -- Sealed login material and which key sealed it, mirroring user_secrets.sealed_with
    -- ('master' legacy box | 'dek' per-user vault DEK).
    sealed_login         BYTEA NOT NULL,
    sealed_with          TEXT NOT NULL CHECK (sealed_with IN ('master', 'dek')),

    -- generation advances each time the login material is rotated; credential_revision
    -- advances on any credential-state write. Both are monotonic counters the m2
    -- refresher/coordinator reason over.
    generation           BIGINT NOT NULL DEFAULT 0,
    credential_revision  BIGINT NOT NULL DEFAULT 0,

    -- The recovery slot: a previously-good sealed login kept so a failed refresh can
    -- roll back. Nullable — there is nothing to recover until at least one refresh has
    -- run (m2).
    recovery_sealed      BYTEA,
    recovery_generation  BIGINT,
    -- Which key sealed the recovery blob, mirroring sealed_with above ('master' legacy
    -- box | 'dek' per-user vault DEK). Nullable — NULL when the recovery slot is empty,
    -- populated whenever recovery_sealed is written so a promotion can open the protected
    -- login with the correct key even if the live sealed_login has since migrated
    -- master→dek (PRD #1147 F14).
    recovery_sealed_with TEXT CHECK (recovery_sealed_with IN ('master', 'dek')),

    -- Refresh coordination (driven by m2, inert here). coord_state is the lease
    -- state machine; coord_operation_id names the in-flight operation holding the
    -- lease; lease_deadline is when that lease expires; committed_generation records
    -- the generation the last committed refresh produced.
    coord_state          TEXT NOT NULL DEFAULT 'idle'
        CHECK (coord_state IN ('idle', 'in_progress', 'committed', 'quarantined')),
    coord_operation_id   UUID,
    lease_deadline       TIMESTAMPTZ,
    committed_generation BIGINT,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,

    -- The canonical identity tuple: one account per (uzi user, provider principal,
    -- workspace). Two uzi users may independently hold the same tuple.
    CONSTRAINT codex_provider_account_tuple_key
        UNIQUE (user_id, provider_user_id, workspace_account_id),

    -- Composite-FK target for the alias link (00199): redundant as a uniqueness
    -- statement (id is already the PK) and load-bearing as the (user_id, id) target
    -- that lets codex_credential_state reference an account owner-scoped, so an alias
    -- can never link to another user's account in the schema.
    CONSTRAINT codex_provider_account_user_id_id_key UNIQUE (user_id, id),

    -- Recovery-slot pairing (PRD #1147 F14): the discriminator and its bytes are written
    -- and cleared together, so the two columns are always both NULL (empty slot) or both
    -- non-NULL (populated slot). This is the invariant PromoteCodexRecovery relies on — it
    -- opens recovery_sealed with recovery_sealed_with — so the schema forbids a populated
    -- blob with no discriminator to open it under (and vice versa).
    CONSTRAINT codex_provider_account_recovery_pairing
        CHECK ((recovery_sealed IS NULL) = (recovery_sealed_with IS NULL))
);

-- +goose Down
DROP TABLE codex_provider_account;
