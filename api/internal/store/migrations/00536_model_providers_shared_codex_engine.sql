-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Admin-shared Codex provider definitions (issue #4633): a shared definition (owner_user_id NULL) may
-- now be a Codex one as well as a Claude one. The same-named check is replaced in one statement so
-- no window exists without it. NOT VALID (no full scan under the lock), validated in 00539.
ALTER TABLE model_providers
    DROP CONSTRAINT model_providers_shared_engine_check,
    ADD CONSTRAINT model_providers_shared_engine_check
        CHECK (owner_user_id IS NOT NULL OR engine IN ('claude', 'codex')) NOT VALID;

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
        RAISE EXCEPTION 'refusing to roll back model_providers_shared_codex_engine: shared Codex provider key(s) still exist; delete them explicitly before downgrading';
    END IF;
    IF EXISTS (SELECT 1 FROM model_providers WHERE owner_user_id IS NULL AND engine = 'codex') THEN
        RAISE EXCEPTION 'refusing to roll back model_providers_shared_codex_engine: shared Codex provider definition(s) still exist; delete them explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
-- Restore 00524's check (validated, as 00525 left it) under the same name.
ALTER TABLE model_providers
    DROP CONSTRAINT model_providers_shared_engine_check,
    ADD CONSTRAINT model_providers_shared_engine_check
        CHECK (owner_user_id IS NOT NULL OR engine = 'claude');
