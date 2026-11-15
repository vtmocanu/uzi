-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_detail_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_detail_check CHECK (
    recovery_wait_detail IS NULL OR recovery_wait_detail IN ('secret_remediation_active','remediation_proof_unknown','source_end_declined')
) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- Refuse historical rows too: narrowing must never discard retained-source evidence.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runs WHERE recovery_wait_detail = 'source_end_declined') THEN
        RAISE EXCEPTION 'refusing rollback 00457 while source-end hold evidence exists';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_detail_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_detail_check CHECK (
    recovery_wait_detail IS NULL OR recovery_wait_detail IN ('secret_remediation_active','remediation_proof_unknown')
) NOT VALID;
