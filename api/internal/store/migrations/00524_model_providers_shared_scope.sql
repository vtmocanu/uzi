-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4324 M1: admin-shared Claude provider definitions with per-user keys. A shared definition
-- is a model_providers row with owner_user_id NULL and credential_mode 'per_user_key'. Personal
-- definitions keep their owner. scope_owner_id is the derived reference scope: the owner's id for a
-- personal definition and the nil UUID for a shared one. It exists only so a composite foreign key
-- (scope_owner_id, id) can bind a user's key or selected source to a definition the user may use,
-- across both the personal and the shared catalogue; a NULL owner cannot be a foreign key parent.
--
-- The nil UUID is the shared scope, so no real row may carry it: refuse an upgrade that already
-- holds one rather than silently aliasing a user to the shared catalogue.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users WHERE id = '00000000-0000-0000-0000-000000000000'::uuid) THEN
        RAISE EXCEPTION 'refusing to apply 00524: a user row carries the nil UUID, which is reserved as the shared provider scope';
    END IF;
    IF EXISTS (SELECT 1 FROM model_providers WHERE owner_user_id = '00000000-0000-0000-0000-000000000000'::uuid) THEN
        RAISE EXCEPTION 'refusing to apply 00524: a model provider row is owned by the nil UUID, which is reserved as the shared provider scope';
    END IF;
END
$$;
-- +goose StatementEnd

-- Metadata only; the existing foreign key to users (ON DELETE CASCADE) stays.
ALTER TABLE model_providers ALTER COLUMN owner_user_id DROP NOT NULL;
ALTER TABLE model_providers ADD COLUMN credential_mode text;
ALTER TABLE model_providers ADD COLUMN scope_owner_id uuid;

-- Bounded backfill: the table holds at most 20 definitions per owner.
UPDATE model_providers SET scope_owner_id = owner_user_id;

-- All NOT VALID (no full scan under the lock), validated in 00525.
ALTER TABLE model_providers ADD CONSTRAINT model_providers_credential_mode_check
    CHECK (credential_mode IN ('per_user_key')) NOT VALID;
-- A NULL owner is exactly a shared definition, and a shared definition names its credential mode.
ALTER TABLE model_providers ADD CONSTRAINT model_providers_shared_mode_check
    CHECK ((owner_user_id IS NULL) = (credential_mode IS NOT NULL)) NOT VALID;
-- Shared definitions are Claude only in this delivery (shared Codex definitions are a separate issue).
ALTER TABLE model_providers ADD CONSTRAINT model_providers_shared_engine_check
    CHECK (owner_user_id IS NOT NULL OR engine = 'claude') NOT VALID;
ALTER TABLE model_providers ADD CONSTRAINT model_providers_scope_owner_check
    CHECK (scope_owner_id IS NOT NULL AND scope_owner_id = COALESCE(owner_user_id, '00000000-0000-0000-0000-000000000000'::uuid)) NOT VALID;
ALTER TABLE model_providers ADD CONSTRAINT model_providers_owner_not_nil_check
    CHECK (owner_user_id IS NULL OR owner_user_id <> '00000000-0000-0000-0000-000000000000'::uuid) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy shared definitions (the 00198 convention). The table is
-- locked BEFORE the guard so a shared definition created concurrently cannot land between the
-- check and the column drop.
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_providers WHERE owner_user_id IS NULL) THEN
        RAISE EXCEPTION 'refusing to roll back 00524: shared model provider definition(s) still exist; delete them explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE model_providers DROP CONSTRAINT model_providers_owner_not_nil_check;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_scope_owner_check;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_shared_engine_check;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_shared_mode_check;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_credential_mode_check;
-- Dropping the columns also drops the partial and composite indexes 00526 built on them.
ALTER TABLE model_providers DROP COLUMN scope_owner_id;
ALTER TABLE model_providers DROP COLUMN credential_mode;
ALTER TABLE model_providers ALTER COLUMN owner_user_id SET NOT NULL;
