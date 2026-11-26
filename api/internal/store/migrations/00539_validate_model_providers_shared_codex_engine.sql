-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_shared_engine_check;

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
    IF EXISTS (SELECT 1 FROM users WHERE codex_provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid) THEN
        RAISE EXCEPTION 'refusing to roll back validate_model_providers_shared_codex_engine: a user still has a shared Codex provider source selected; clear it explicitly before downgrading';
    END IF;
    IF EXISTS (SELECT 1 FROM user_secrets WHERE provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid AND kind = 'openai_compatible_key') THEN
        RAISE EXCEPTION 'refusing to roll back validate_model_providers_shared_codex_engine: shared Codex provider key(s) still exist; delete them explicitly before downgrading';
    END IF;
    IF EXISTS (SELECT 1 FROM model_providers WHERE owner_user_id IS NULL AND engine = 'codex') THEN
        RAISE EXCEPTION 'refusing to roll back validate_model_providers_shared_codex_engine: shared Codex provider definition(s) still exist; delete them explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
