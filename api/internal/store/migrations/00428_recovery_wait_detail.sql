-- Issue #4177: closed-set sub-cause of a retained_recovery_adoption_failed park. The worker
-- reports it; NULL means unclassified and never permits anything new. secret_remediation_active
-- means retrying unchanged cannot help (Resume is refused); remediation_proof_unknown means a
-- retry may help. The CHECK is added NOT VALID and validated by 00429 so this holds only a
-- brief lock.

-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs ADD COLUMN recovery_wait_detail text;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_detail_check CHECK (
    recovery_wait_detail IS NULL OR recovery_wait_detail IN ('secret_remediation_active','remediation_proof_unknown')
) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Serialise against live run writers before the existence check below; a held lock fails the
-- rollback after 5s rather than queueing every runs writer behind it.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- Dropping the column would erase the only record that Resume must be refused for a run still
-- parked under an active secret remediation. Refuse the rollback while any such run exists.
-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM runs
    WHERE status = 'recovery_wait' AND recovery_wait_cause = 'retained_recovery_adoption_failed'
      AND recovery_wait_detail = 'secret_remediation_active';
    IF n > 0 THEN
        RAISE EXCEPTION 'refusing to roll back 00428: % run(s) are parked in recovery_wait at retained_recovery_adoption_failed under an active secret remediation; Cancel those runs first', n;
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_recovery_wait_detail_check;
ALTER TABLE runs DROP COLUMN IF EXISTS recovery_wait_detail;
