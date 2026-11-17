-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4320 M1: the 'anthropic_compatible_key' user_secrets kind (a gateway key bound to one
-- model provider), sealed through the existing vault path with the existing AAD (user, kind).
-- This file touches user_secrets only (model_providers is a new table, not a busy one).
-- provider_id is set at insert and has no UPDATE statement (a store test pins that). The
-- composite foreign key keeps the key and its provider under the same owner. auto_eligible stays
-- false: 00389's user_secrets_auto_eligible_kind_check already excludes this kind.
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_kind_check
    CHECK (kind IN ('anthropic_token', 'openai_api_key', 'codex_auth', 'anthropic_compatible_key'));

ALTER TABLE user_secrets ADD COLUMN provider_id uuid;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_provider_fkey
    FOREIGN KEY (user_id, provider_id) REFERENCES model_providers (owner_user_id, id) ON DELETE CASCADE;
-- A provider key carries a provider and nothing else does. NOT VALID here, validated in 00468.
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_provider_key_check
    CHECK ((kind = 'anthropic_compatible_key') = (provider_id IS NOT NULL)) NOT VALID;
-- One key per (user, provider).
CREATE UNIQUE INDEX user_secrets_one_provider_key
    ON user_secrets (user_id, provider_id)
    WHERE provider_id IS NOT NULL;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy sealed credential material (the 00198 convention).
-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM user_secrets WHERE kind = 'anthropic_compatible_key';
    IF n > 0 THEN
        RAISE EXCEPTION 'refusing to roll back 00467: % provider key row(s) (anthropic_compatible_key) still exist; delete them explicitly before downgrading', n;
    END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX user_secrets_one_provider_key;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_provider_key_check;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_provider_fkey;
ALTER TABLE user_secrets DROP COLUMN provider_id;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_kind_check
    CHECK (kind IN ('anthropic_token', 'openai_api_key', 'codex_auth'));
