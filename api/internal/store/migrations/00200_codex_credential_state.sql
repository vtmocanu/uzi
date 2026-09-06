-- +goose Up

-- Per-alias link/status for BOTH codex kinds (PRD #1147 M1), STORE/SCHEMA only —
-- ships DARK. One row per user_secrets alias of kind 'openai_api_key' or
-- 'codex_auth' (PK is the alias id), recording how that alias resolves:
--   'staging' — created, not yet linked to a provider account.
--   'linked'  — bound to a codex_provider_account (00199) row.
--   'failed'  — a link/refresh attempt failed; last_error carries why.
--   'static'  — a standalone openai_api_key with no subscription account behind it.
-- Ordering: 00199 (codex_provider_account) MUST precede this migration because
-- provider_account_id references it; the Down in 00199 drops that table, so this
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

    -- Owner-scoped composite FK to the account (00199's
    -- codex_provider_account_user_id_id_key). The column-list SET NULL is
    -- load-bearing exactly as 00086 documents: a bare ON DELETE SET NULL on a
    -- COMPOSITE FK would null user_id too (which is NOT NULL, so the delete would
    -- error); `SET NULL (provider_account_id)` nulls only the link and leaves the
    -- owner alone, so deleting an account unlinks its aliases rather than failing.
    FOREIGN KEY (user_id, provider_account_id)
        REFERENCES codex_provider_account (user_id, id)
        ON DELETE SET NULL (provider_account_id)
);

-- Orphan invariant (PRD #1147 F4): the ON DELETE SET NULL above can leave an alias row
-- reading status='linked' with a NULL provider_account_id — a link that points at nothing.
-- A BEFORE UPDATE trigger closes that: when the FK cascade (or any update) nulls
-- provider_account_id on a row that HAD an account, the row is demoted to 'failed' with an
-- explanatory last_error. It is a BEFORE trigger, not AFTER, because an AFTER trigger
-- cannot modify the row in place — a BEFORE trigger sets NEW.* directly and RETURNs NEW,
-- so the demotion is part of the same write rather than a second UPDATE (which would
-- re-fire OF provider_account_id needlessly). Guarded by the WHEN clause so it fires ONLY
-- on the not-null→null transition, never on a static alias (already NULL) or an ordinary
-- re-link (null→not-null / not-null→not-null).
-- The `NEW.status = OLD.status` guard distinguishes the FK cascade from an app-driven
-- unlink: the ON DELETE SET NULL touches ONLY provider_account_id and leaves status
-- untouched, so the cascade still fires this trigger; but a legitimate app UPDATE that
-- nulls provider_account_id AND changes status in the SAME statement (e.g.
-- BumpCodexMaterialRevision re-staging to 'staging'/'static') is skipped, so its intended
-- status is not clobbered to 'failed'.
-- +goose StatementBegin
CREATE FUNCTION codex_credential_state_orphan_fail() RETURNS trigger AS $$
BEGIN
    NEW.status := 'failed';
    NEW.last_error := 'provider account deleted';
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER codex_credential_state_orphan_fail_trg
    BEFORE UPDATE OF provider_account_id ON codex_credential_state
    FOR EACH ROW
    WHEN (OLD.provider_account_id IS NOT NULL AND NEW.provider_account_id IS NULL AND NEW.status = OLD.status)
    EXECUTE FUNCTION codex_credential_state_orphan_fail();

-- +goose Down
DROP TRIGGER codex_credential_state_orphan_fail_trg ON codex_credential_state;
DROP FUNCTION codex_credential_state_orphan_fail();
DROP TABLE codex_credential_state;
