-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4633: a selected Codex source records the scope of the definition, so the composite foreign
-- key accepts the user's own definition (scope = id) or a shared one (scope = the nil UUID) and
-- nothing else, as 00528 did for Claude. This file touches users and, through the foreign key it
-- moves, takes model_providers.
--
-- Both tables are locked up front, in the order the source-select path takes them (a provider row,
-- then a users write). Without this, ADD COLUMN (users) followed by the foreign key drop (model_providers)
-- takes the same two locks in the opposite order to a request that locks the provider and then
-- writes users, and the two deadlock.
LOCK TABLE model_providers, users IN ACCESS EXCLUSIVE MODE;

-- The guard below refuses a missing, invalid or non-unique model_providers_scope_owner_id_key and names the
-- recovery (ADR 4324, Scope-index recovery).
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index i
        WHERE i.indexrelid = to_regclass('model_providers_scope_owner_id_key')
          AND i.indisvalid AND i.indisunique
    ) THEN
        RAISE EXCEPTION 'refusing to apply users_codex_provider_scope: model_providers_scope_owner_id_key is missing, invalid or not unique. If it is missing, stop and inspect the prerequisite foreign keys user_secrets_provider_fkey and users_claude_provider_fkey, since recreating the index alone does not restore them; if it is invalid with the exact 00526 definition, first confirm those foreign keys still reference it, then rebuild it with REINDEX per the runbook and retry; otherwise stop and investigate. See Scope-index recovery in ADR 4324 (adr/4324-admin-shared-providers.md).';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE users ADD COLUMN codex_provider_scope_id uuid;
-- Bounded backfill: only users who selected a source.
UPDATE users SET codex_provider_scope_id = id WHERE codex_provider_id IS NOT NULL;

-- The foreign key below resolves a delete of a definition by looking up users by codex_provider_id;
-- without an index that is a sequential scan of users per deleted definition. Built inside this
-- transaction (not CONCURRENTLY): this migration already holds ACCESS EXCLUSIVE on users and assumes
-- users stays small, as 00528 did for its own column.
CREATE INDEX users_codex_provider_id_idx ON users (codex_provider_id) WHERE codex_provider_id IS NOT NULL;

ALTER TABLE users ADD CONSTRAINT users_codex_provider_scope_check
    CHECK ((codex_provider_id IS NULL AND codex_provider_scope_id IS NULL)
        OR (codex_provider_id IS NOT NULL AND codex_provider_scope_id IS NOT NULL
            AND codex_provider_scope_id IN (id, '00000000-0000-0000-0000-000000000000'::uuid))) NOT VALID;

-- ON DELETE RESTRICT is kept: deleting a selected definition is refused, never silently nulled.
-- One statement so no window exists without a foreign key. Validated in 00541.
ALTER TABLE users
    DROP CONSTRAINT users_codex_provider_fkey,
    ADD CONSTRAINT users_codex_provider_fkey
        FOREIGN KEY (codex_provider_scope_id, codex_provider_id) REFERENCES model_providers (scope_owner_id, id) ON DELETE RESTRICT NOT VALID;

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
        RAISE EXCEPTION 'refusing to roll back users_codex_provider_scope: a user still has a shared Codex provider source selected; clear it explicitly before downgrading';
    END IF;
    IF EXISTS (SELECT 1 FROM user_secrets WHERE provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid AND kind = 'openai_compatible_key') THEN
        RAISE EXCEPTION 'refusing to roll back users_codex_provider_scope: shared Codex provider key(s) still exist; delete them explicitly before downgrading';
    END IF;
    IF EXISTS (SELECT 1 FROM model_providers WHERE owner_user_id IS NULL AND engine = 'codex') THEN
        RAISE EXCEPTION 'refusing to roll back users_codex_provider_scope: shared Codex provider definition(s) still exist; delete them explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
-- Restore 00517's foreign key (validated, as 00518 left it) under the same name.
ALTER TABLE users
    DROP CONSTRAINT users_codex_provider_fkey,
    ADD CONSTRAINT users_codex_provider_fkey
        FOREIGN KEY (id, codex_provider_id) REFERENCES model_providers (owner_user_id, id) ON DELETE RESTRICT;
-- Dropped explicitly rather than left to the column drop.
DROP INDEX users_codex_provider_id_idx;
ALTER TABLE users DROP CONSTRAINT users_codex_provider_scope_check;
ALTER TABLE users DROP COLUMN codex_provider_scope_id;
