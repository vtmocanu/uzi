-- +goose Up

-- Explicit recovery provenance; existing slots remain NULL without inference or backfill.
ALTER TABLE codex_provider_account ADD COLUMN recovery_cause TEXT;

ALTER TABLE codex_provider_account
    ADD CONSTRAINT codex_provider_account_recovery_cause_check
        CHECK (recovery_cause IS NULL OR recovery_cause IN ('vault_locked')),
    ADD CONSTRAINT codex_provider_account_recovery_cause_slot_check
        CHECK (recovery_cause IS NULL OR (
            coord_state = 'quarantined'
            AND recovery_sealed IS NOT NULL
            AND recovery_sealed_with IS NOT NULL
            AND recovery_generation IS NOT NULL
        ));

-- +goose Down

-- Remove only the provenance metadata; preserve all recovery material.
ALTER TABLE codex_provider_account
    DROP CONSTRAINT codex_provider_account_recovery_cause_slot_check,
    DROP CONSTRAINT codex_provider_account_recovery_cause_check,
    DROP COLUMN recovery_cause;
