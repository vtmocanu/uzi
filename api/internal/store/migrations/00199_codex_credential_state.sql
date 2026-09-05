-- +goose Up

-- Per-alias link/status for BOTH codex kinds (PRD #1147 M1), STORE/SCHEMA only —
-- ships DARK. One row per user_secrets alias of kind 'openai_api_key' or
-- 'codex_auth' (PK is the alias id), recording how that alias resolves:
--   'staging' — created, not yet linked to a provider account.
--   'linked'  — bound to a codex_provider_account (00198) row.
--   'failed'  — a link/refresh attempt failed; last_error carries why.
--   'static'  — a standalone openai_api_key with no subscription account behind it.
-- Ordering: 00198 (codex_provider_account) MUST precede this migration because
-- provider_account_id references it; the Down in 00198 drops that table, so this
-- table (and its FK) must already be gone, which the migration order guarantees.
CREATE TABLE codex_credential_state (
    -- PK is the alias id: exactly one state row per user_secrets credential.
    user_secret_id      UUID PRIMARY KEY,
    user_id             UUID NOT NULL,

    status              TEXT NOT NULL
        CHECK (status IN ('staging', 'linked', 'failed', 'static')),

    -- The account this alias resolves to, or NULL while staging/static/failed.
    provider_account_id UUID,

    -- Bumped whenever the underlying material changes, so a consumer can detect a
    -- stale binding. last_error carries the reason for a 'failed' status.
    material_revision   BIGINT NOT NULL DEFAULT 0,
    last_error          TEXT,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Owner-scoped composite FK to the alias, using 00077's
    -- user_secrets_user_id_id_key: a state row can only reference a secret the SAME
    -- user owns, and deleting the secret cascades its state row away.
    FOREIGN KEY (user_id, user_secret_id)
        REFERENCES user_secrets (user_id, id) ON DELETE CASCADE,

    -- Owner-scoped composite FK to the account (00198's
    -- codex_provider_account_user_id_id_key). The column-list SET NULL is
    -- load-bearing exactly as 00086 documents: a bare ON DELETE SET NULL on a
    -- COMPOSITE FK would null user_id too (which is NOT NULL, so the delete would
    -- error); `SET NULL (provider_account_id)` nulls only the link and leaves the
    -- owner alone, so deleting an account unlinks its aliases rather than failing.
    FOREIGN KEY (user_id, provider_account_id)
        REFERENCES codex_provider_account (user_id, id)
        ON DELETE SET NULL (provider_account_id)
);

-- +goose Down
DROP TABLE codex_credential_state;
