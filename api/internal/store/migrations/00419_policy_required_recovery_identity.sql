-- +goose Up
SET LOCAL lock_timeout = '5s';
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
SET LOCAL lock_timeout = '30s';
-- Goose runs this migration transactionally. Lock before inspecting or converting so no writer can
-- change a row between the check and the drop. A nonterminal row is ACTIVE when it still carries a
-- required recovery head or a recorded verification, or when it is resolved and still preserving (a
-- headless hold the owner resumed with preserve: the older API never writes policy_resolved_at, so it
-- would claim such a row without enforcing preserve-only). policy_hold_cause alone is history, so a resolved attempt-path row that
-- merely retains its cause (preserve false) is neither active nor converted. The older API has no notion of the
-- requirement, so an active row must not outlive its enforcement. Refuse before any mutation when an
-- active row is not paused or queued (live claimed/running/awaiting_* rows must be drained first) or
-- when a nonterminal row has an orphaned or malformed identity; then end the paused/queued active rows
-- as failed with the same terminal bookkeeping as the 00397 rollback, keeping session, custody,
-- captures and sources untouched.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM runs WHERE status NOT IN ('completed','failed','cancelled')
          AND ((policy_required_head IS NULL AND (policy_recovery_verified_generation IS NOT NULL OR policy_recovery_verified_head IS NOT NULL))
               OR (policy_required_head IS NOT NULL AND policy_required_head !~ '^([0-9a-f]{40}|[0-9a-f]{64})$')
               OR (policy_recovery_verified_head IS NOT NULL AND policy_recovery_verified_head !~ '^([0-9a-f]{40}|[0-9a-f]{64})$'))
    ) THEN
        RAISE EXCEPTION 'policy required-recovery rollback refused: orphaned or malformed recovery identity';
    END IF;
    IF EXISTS (
        SELECT 1 FROM runs WHERE status NOT IN ('completed','failed','cancelled')
          AND (policy_required_head IS NOT NULL
               OR policy_recovery_verified_generation IS NOT NULL OR policy_recovery_verified_head IS NOT NULL
               OR (policy_preserve_required AND policy_resolved_at IS NOT NULL))
          AND status NOT IN ('paused','queued')
    ) THEN
        RAISE EXCEPTION 'policy required-recovery rollback refused: a live run still carries a required recovery head or preserving constraint';
    END IF;
END $$;
-- +goose StatementEnd
UPDATE runs SET plan_cross_check_gate_reason=NULL,status='failed',status_since=now(),finished_at=now(),updated_at=now(),
    fail_origin='agent_failure',failure_reason='Policy recovery requirement ended by schema rollback',
    move_pending_since=CASE WHEN issue_iid IS NOT NULL THEN now() END,
    milestones_in_progress=NULL,milestones_agents=NULL,
    pause_requested_at=NULL,pause_mode=NULL,pause_after_count=NULL,
    credential_switch_requested_at=NULL,credential_switch_generation=NULL,
    health='ok',health_reason=NULL,health_since=NULL,
    codex_cap_hash=NULL,codex_claim_epoch=codex_claim_epoch+1
WHERE status IN ('paused','queued')
    AND (policy_required_head IS NOT NULL
         OR policy_recovery_verified_generation IS NOT NULL OR policy_recovery_verified_head IS NOT NULL
         OR (policy_preserve_required AND policy_resolved_at IS NOT NULL));
ALTER TABLE runs DROP CONSTRAINT runs_policy_required_recovery_shape;
ALTER TABLE runs
    DROP COLUMN policy_required_head,
    DROP COLUMN policy_recovery_verified_generation,
    DROP COLUMN policy_recovery_verified_head,
    DROP COLUMN policy_hold_cause;
