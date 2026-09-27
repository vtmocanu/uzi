-- +goose Up
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check
    CHECK (recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable', 'empty_turn', 'provider_outage',
        'codex_account_unavailable', 'vault_locked'
    ));

-- +goose Down
UPDATE runs SET recovery_wait_cause = NULL
WHERE recovery_wait_cause = 'vault_locked';
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check
    CHECK (recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable', 'empty_turn', 'provider_outage',
        'codex_account_unavailable'
    ));
