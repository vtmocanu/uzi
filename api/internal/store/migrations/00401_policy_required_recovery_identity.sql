-- +goose Up
-- Required-recovery identity for the compact policy hold (issue #3607 M2a). The columns are
-- additive and deliberately carry no cross-column coupling, so an older API writing the
-- earlier policy-hold shape during a rolling deploy stays valid.
ALTER TABLE runs
    ADD COLUMN policy_required_head text,
    ADD COLUMN policy_recovery_verified_generation bigint,
    ADD COLUMN policy_recovery_verified_head text,
    ADD COLUMN policy_hold_cause text;
ALTER TABLE runs ADD CONSTRAINT runs_policy_required_recovery_shape CHECK (
    (policy_required_head IS NULL OR policy_required_head ~ '^([0-9a-f]{40}|[0-9a-f]{64})$')
    AND (policy_recovery_verified_head IS NULL OR policy_recovery_verified_head ~ '^([0-9a-f]{40}|[0-9a-f]{64})$')
    AND (policy_hold_cause IS NULL OR policy_hold_cause IN (
        'refused','attempt_path','saved_work_missing','session_missing','transcript_unknown',
        'provider_resume_rejected','pending_conversion','fresh_start_uncertain'))
);
-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_policy_required_recovery_shape;
ALTER TABLE runs
    DROP COLUMN policy_required_head,
    DROP COLUMN policy_recovery_verified_generation,
    DROP COLUMN policy_recovery_verified_head,
    DROP COLUMN policy_hold_cause;
