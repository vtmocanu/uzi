-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4324 M1: a selected Claude source records the scope of the definition, so the composite
-- foreign key accepts the user's own definition (scope = id) or a shared one (scope = the nil UUID)
-- and nothing else. users.codex_provider_id and users_codex_provider_fkey are untouched: a Codex
-- source stays bound to its owner, so a shared definition can never be a Codex source. This file
-- touches users only.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index i
        WHERE i.indexrelid = to_regclass('model_providers_scope_owner_id_key')
          AND i.indisvalid AND i.indisunique
    ) THEN
        RAISE EXCEPTION 'refusing to apply 00528: model_providers_scope_owner_id_key is missing or invalid; roll 00526 back and apply it again';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE users ADD COLUMN claude_provider_scope_id uuid;
-- Bounded backfill: only users who selected a source.
UPDATE users SET claude_provider_scope_id = id WHERE claude_provider_id IS NOT NULL;

ALTER TABLE users ADD CONSTRAINT users_claude_provider_scope_check
    CHECK ((claude_provider_id IS NULL AND claude_provider_scope_id IS NULL)
        OR (claude_provider_id IS NOT NULL AND claude_provider_scope_id IS NOT NULL
            AND claude_provider_scope_id IN (id, '00000000-0000-0000-0000-000000000000'::uuid))) NOT VALID;

-- ON DELETE RESTRICT is kept: deleting a selected definition is refused, never silently nulled.
-- Validated in 00530.
ALTER TABLE users
    DROP CONSTRAINT users_claude_provider_fkey,
    ADD CONSTRAINT users_claude_provider_fkey
        FOREIGN KEY (claude_provider_scope_id, claude_provider_id) REFERENCES model_providers (scope_owner_id, id) ON DELETE RESTRICT NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently clear a source selected on a shared definition: dropping it would
-- turn the selection into a vendor fallback. Locked BEFORE the guard so a selection cannot land
-- between the check and the column drop.
LOCK TABLE users IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users WHERE claude_provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid) THEN
        RAISE EXCEPTION 'refusing to roll back 00528: user(s) still have a shared Claude provider source selected; clear it explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
-- Restore 00479's foreign key (validated, as 00480 left it) under the same name.
ALTER TABLE users
    DROP CONSTRAINT users_claude_provider_fkey,
    ADD CONSTRAINT users_claude_provider_fkey
        FOREIGN KEY (id, claude_provider_id) REFERENCES model_providers (owner_user_id, id) ON DELETE RESTRICT;
ALTER TABLE users DROP CONSTRAINT users_claude_provider_scope_check;
ALTER TABLE users DROP COLUMN claude_provider_scope_id;
