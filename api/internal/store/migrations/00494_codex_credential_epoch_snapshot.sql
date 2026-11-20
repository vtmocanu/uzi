-- +goose Up
-- Nullable snapshots: historical and Claude epochs have no Codex binding.
ALTER TABLE run_credential_epochs
    ADD COLUMN codex_auth_mode text,
    ADD COLUMN codex_material_revision bigint,
    ADD COLUMN codex_account_key text,
    ADD COLUMN codex_account_id text,
    ADD COLUMN codex_claim_epoch bigint;
ALTER TABLE run_credential_switch_epochs
    ADD COLUMN codex_auth_mode text,
    ADD COLUMN codex_material_revision bigint,
    ADD COLUMN codex_account_key text,
    ADD COLUMN codex_account_id text,
    ADD COLUMN codex_claim_epoch bigint;

-- +goose Down
ALTER TABLE run_credential_switch_epochs
    DROP COLUMN codex_auth_mode,
    DROP COLUMN codex_material_revision,
    DROP COLUMN codex_account_key,
    DROP COLUMN codex_account_id,
    DROP COLUMN codex_claim_epoch;
ALTER TABLE run_credential_epochs
    DROP COLUMN codex_auth_mode,
    DROP COLUMN codex_material_revision,
    DROP COLUMN codex_account_key,
    DROP COLUMN codex_account_id,
    DROP COLUMN codex_claim_epoch;
