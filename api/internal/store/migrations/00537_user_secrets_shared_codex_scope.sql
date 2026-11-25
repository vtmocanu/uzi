-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4633: a shared definition's key may now carry the shared scope for a Codex gateway key
-- (openai_compatible_key) as well as a Claude one (anthropic_compatible_key). The personal scope
-- (provider_scope_id = user_id) is unchanged. The composite foreign key to model_providers
-- (scope_owner_id, id) is untouched, so a key still binds only to a definition that exists. Replaced
-- in one statement so no window exists without the check. NOT VALID, validated in 00540.
ALTER TABLE user_secrets
    DROP CONSTRAINT user_secrets_provider_scope_check,
    ADD CONSTRAINT user_secrets_provider_scope_check
        CHECK ((provider_id IS NULL AND provider_scope_id IS NULL)
            OR (provider_id IS NOT NULL AND provider_scope_id IS NOT NULL
                AND (provider_scope_id = user_id
                     OR (provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid
                         AND kind IN ('anthropic_compatible_key', 'openai_compatible_key'))))) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy or orphan shared Codex state: a shared Codex definition, a key
-- attached to one, or a user's selection of one. Every table the guard reads is locked BEFORE the
-- guard (provider first, then secrets, then users), so none can land between the check and the ALTER
-- that follows. That matches the select and admin-delete paths (provider row, then users); some
-- key-write paths lock a users row before the provider, so a deadlock with a running request is
-- possible (PostgreSQL aborts one side, nothing is lost): run a Down with the API stopped.
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
LOCK TABLE user_secrets IN ACCESS EXCLUSIVE MODE;
LOCK TABLE users IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM user_secrets WHERE provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid AND kind = 'openai_compatible_key') THEN
        RAISE EXCEPTION 'refusing to roll back user_secrets_shared_codex_scope: shared Codex provider key(s) still exist; delete them explicitly before downgrading';
    END IF;
    IF EXISTS (SELECT 1 FROM model_providers WHERE owner_user_id IS NULL AND engine = 'codex') THEN
        RAISE EXCEPTION 'refusing to roll back user_secrets_shared_codex_scope: shared Codex provider definition(s) still exist; delete them explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
-- Restore 00527's check (validated, as 00529 left it) under the same name.
ALTER TABLE user_secrets
    DROP CONSTRAINT user_secrets_provider_scope_check,
    ADD CONSTRAINT user_secrets_provider_scope_check
        CHECK ((provider_id IS NULL AND provider_scope_id IS NULL)
            OR (provider_id IS NOT NULL AND provider_scope_id IS NOT NULL
                AND (provider_scope_id = user_id
                     OR (provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid AND kind = 'anthropic_compatible_key'))));
