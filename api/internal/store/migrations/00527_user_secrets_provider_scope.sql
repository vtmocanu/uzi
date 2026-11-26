-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4324 M1: a provider key records the scope of the definition it is attached to, so a composite
-- foreign key can bind it to a definition its owner may use: the user's own (scope = user_id) or,
-- for a Claude gateway key only, a shared one (scope = the nil UUID). The openai_compatible_key kind
-- therefore never accepts the shared scope. This file touches user_secrets only. Ciphertext, ids and
-- enablement_rev are untouched.
--
-- The foreign key below references model_providers_scope_owner_id_key (00526). Refuse an invalid
-- index explicitly rather than rely on the planner's own refusal. The message names the recovery for a
-- missing, invalid or non-unique index (ADR 4324, Scope-index recovery), not a 00526 rollback.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index i
        WHERE i.indexrelid = to_regclass('model_providers_scope_owner_id_key')
          AND i.indisvalid AND i.indisunique
    ) THEN
        RAISE EXCEPTION 'refusing to apply 00527: model_providers_scope_owner_id_key is missing, invalid or not unique. If it is missing, recreate it as 00526 does and retry; if it is invalid with the exact 00526 definition, rebuild it with REINDEX per the runbook and retry; otherwise stop and investigate. See Scope-index recovery in ADR 4324 (adr/4324-admin-shared-providers.md).';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE user_secrets ADD COLUMN provider_scope_id uuid;
-- Bounded backfill: provider keys are at most one per (user, provider), capped per user.
UPDATE user_secrets SET provider_scope_id = user_id WHERE provider_id IS NOT NULL;

ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_provider_scope_check
    CHECK ((provider_id IS NULL AND provider_scope_id IS NULL)
        OR (provider_id IS NOT NULL AND provider_scope_id IS NOT NULL
            AND (provider_scope_id = user_id
                 OR (provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid AND kind = 'anthropic_compatible_key')))) NOT VALID;

-- One statement so no window exists without a provider foreign key. Validated in 00529.
ALTER TABLE user_secrets
    DROP CONSTRAINT user_secrets_provider_fkey,
    ADD CONSTRAINT user_secrets_provider_fkey
        FOREIGN KEY (provider_scope_id, provider_id) REFERENCES model_providers (scope_owner_id, id) ON DELETE CASCADE NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy keys attached to shared definitions (the 00198 convention).
-- Locked BEFORE the guard so a key attached concurrently cannot slip in between.
LOCK TABLE user_secrets IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM user_secrets WHERE provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid) THEN
        RAISE EXCEPTION 'refusing to roll back 00527: provider key(s) attached to shared definitions still exist; delete them explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
-- Restore 00477's foreign key (validated, as 00478 left it) under the same name.
ALTER TABLE user_secrets
    DROP CONSTRAINT user_secrets_provider_fkey,
    ADD CONSTRAINT user_secrets_provider_fkey
        FOREIGN KEY (user_id, provider_id) REFERENCES model_providers (owner_user_id, id) ON DELETE CASCADE;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_provider_scope_check;
ALTER TABLE user_secrets DROP COLUMN provider_scope_id;
