-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_kind_check
    CHECK (kind IN ('anthropic_token', 'openai_api_key', 'codex_auth', 'anthropic_compatible_key', 'openai_compatible_key')) NOT VALID;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_provider_key_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_provider_key_check
    CHECK ((kind IN ('anthropic_compatible_key', 'openai_compatible_key')) = (provider_id IS NOT NULL)) NOT VALID;
-- Provider keys cannot become vendor defaults; the existing auto-eligible CHECK already excludes them.
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_provider_default_check
    CHECK (provider_id IS NULL OR NOT is_default) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE user_secrets IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM user_secrets WHERE kind = 'openai_compatible_key') THEN
        RAISE EXCEPTION 'refusing to roll back 00515: Responses provider keys still exist';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_provider_default_check;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_kind_check
    CHECK (kind IN ('anthropic_token', 'openai_api_key', 'codex_auth', 'anthropic_compatible_key'));
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_provider_key_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_provider_key_check
    CHECK ((kind = 'anthropic_compatible_key') = (provider_id IS NOT NULL));
