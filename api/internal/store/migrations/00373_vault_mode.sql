-- +goose Up
-- Backfill every existing user as protected, including vaultless, OIDC and inactive
-- users. Only metadata changes: existing ciphertext is neither resealed nor rewritten.
ALTER TABLE users
    ADD COLUMN vault_mode TEXT NOT NULL DEFAULT 'protected' CHECK (vault_mode IN ('protected', 'unprotected')),
    ADD COLUMN vault_epoch BIGINT NOT NULL DEFAULT 0 CHECK (vault_epoch >= 0);

INSERT INTO app_settings (key, value)
VALUES ('vault_policy', CASE WHEN EXISTS (SELECT 1 FROM users) THEN 'required' ELSE 'optional' END);

ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_sealed_with_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_sealed_with_check
    CHECK (sealed_with IN ('master', 'master_aad', 'dek'));
ALTER TABLE codex_provider_account DROP CONSTRAINT codex_provider_account_sealed_with_check;
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_sealed_with_check
    CHECK (sealed_with IN ('master', 'master_aad', 'dek'));
ALTER TABLE codex_provider_account DROP CONSTRAINT codex_provider_account_recovery_sealed_with_check;
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_recovery_sealed_with_check
    CHECK (recovery_sealed_with IN ('master', 'master_aad', 'dek'));

-- +goose Down
-- Refuse rollback before narrowing discriminators or dropping mode metadata:
-- older code cannot interpret master_aad or preserve an unprotected user's mode.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users WHERE vault_mode = 'unprotected')
       OR EXISTS (SELECT 1 FROM user_secrets WHERE sealed_with = 'master_aad')
       OR EXISTS (SELECT 1 FROM codex_provider_account WHERE sealed_with = 'master_aad' OR recovery_sealed_with = 'master_aad') THEN
        RAISE EXCEPTION 'vault mode rollback refused: unprotected users or master_aad material remains';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_sealed_with_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_sealed_with_check CHECK (sealed_with IN ('master', 'dek'));
ALTER TABLE codex_provider_account DROP CONSTRAINT codex_provider_account_sealed_with_check;
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_sealed_with_check CHECK (sealed_with IN ('master', 'dek'));
ALTER TABLE codex_provider_account DROP CONSTRAINT codex_provider_account_recovery_sealed_with_check;
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_recovery_sealed_with_check CHECK (recovery_sealed_with IN ('master', 'dek'));
DELETE FROM app_settings WHERE key = 'vault_policy';
ALTER TABLE users DROP COLUMN vault_mode, DROP COLUMN vault_epoch;
