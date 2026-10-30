-- +goose Up
ALTER TABLE runs
    ADD COLUMN policy_preserve_required boolean NOT NULL DEFAULT false,
    ADD COLUMN policy_pending_fresh boolean NOT NULL DEFAULT false,
    ADD COLUMN policy_outcome_id uuid,
    ADD COLUMN policy_continuation_mode text,
    ADD COLUMN policy_resolved_at timestamptz,
    ADD COLUMN policy_applying_worker_id uuid,
    ADD COLUMN policy_applying_generation bigint,
    ADD COLUMN policy_source_generation bigint,
    ADD COLUMN policy_refusal_count integer NOT NULL DEFAULT 0,
    ADD COLUMN policy_refusal_tag text,
    ADD COLUMN policy_refusal_phase text,
    ADD COLUMN policy_refusal_correlation_id text,
    ADD COLUMN policy_hold_published boolean;
ALTER TABLE runs ADD CONSTRAINT runs_policy_hold_shape CHECK (
    policy_refusal_count >= 0
    AND (policy_continuation_mode IS NULL OR policy_continuation_mode IN ('same_conversation','fresh_from_saved_work'))
    AND (policy_refusal_tag IS NULL OR policy_refusal_tag IN ('cyberPolicy','misalignmentPolicyViolation'))
    AND (policy_refusal_phase IS NULL OR policy_refusal_phase IN ('planning','implementation'))
    AND ((policy_outcome_id IS NULL AND policy_continuation_mode IS NULL
          AND policy_applying_worker_id IS NULL AND policy_applying_generation IS NULL
          AND policy_source_generation IS NULL AND policy_resolved_at IS NULL
          AND NOT policy_preserve_required AND NOT policy_pending_fresh)
      OR (policy_outcome_id IS NOT NULL AND policy_outcome_id <> '00000000-0000-0000-0000-000000000000'
          AND policy_continuation_mode IS NOT NULL AND policy_applying_worker_id IS NOT NULL
          AND policy_source_generation IS NOT NULL AND policy_applying_generation IS NOT NULL
          AND policy_source_generation > 0 AND policy_applying_generation >= policy_source_generation
          AND policy_applying_generation <= 9007199254740991
          AND (policy_resolved_at IS NOT NULL OR policy_preserve_required)))
    AND (NOT policy_pending_fresh OR (policy_preserve_required AND policy_continuation_mode = 'same_conversation'))
    AND (hold_reason IS DISTINCT FROM 'codex_policy_hold'
         OR (status IN ('paused','failed','cancelled','completed') AND policy_outcome_id IS NOT NULL))
);
-- +goose Down
-- Goose runs this migration transactionally. Lock before inspecting or converting:
-- an old API must never receive an unenforced live preserving constraint.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM runs WHERE status NOT IN ('completed','failed','cancelled')
          AND (policy_preserve_required OR policy_pending_fresh
               OR (policy_outcome_id IS NOT NULL AND policy_resolved_at IS NULL)
               OR hold_reason = 'codex_policy_hold')
          AND (status NOT IN ('paused','queued') OR policy_outcome_id IS NULL
               OR policy_continuation_mode IS NULL OR policy_applying_worker_id IS NULL
               OR policy_applying_generation IS NULL OR policy_source_generation IS NULL
               OR NOT EXISTS (SELECT 1 FROM workers w WHERE w.id=runs.policy_applying_worker_id AND w.user_id=runs.user_id)
)
    ) THEN
        RAISE EXCEPTION 'compact policy hold rollback refused: live or unknown affected run';
    END IF;
END $$;
-- +goose StatementEnd
UPDATE runs SET plan_cross_check_gate_reason=NULL,status='failed',status_since=now(),finished_at=now(),updated_at=now(),
    fail_origin='agent_failure',failure_reason='Policy hold ended by schema rollback (00380)',
    move_pending_since=CASE WHEN issue_iid IS NOT NULL THEN now() END,
    milestones_in_progress=NULL,milestones_agents=NULL,
    pause_requested_at=NULL,pause_mode=NULL,pause_after_count=NULL,
    credential_switch_requested_at=NULL,credential_switch_generation=NULL,
    health='ok',health_reason=NULL,health_since=NULL,
    codex_cap_hash=NULL,codex_claim_epoch=codex_claim_epoch+1
WHERE status IN ('paused','queued')
    AND (policy_preserve_required OR policy_pending_fresh
         OR (policy_outcome_id IS NOT NULL AND policy_resolved_at IS NULL)
         OR hold_reason = 'codex_policy_hold');
ALTER TABLE runs DROP CONSTRAINT runs_policy_hold_shape;
ALTER TABLE runs
    DROP COLUMN policy_preserve_required, DROP COLUMN policy_pending_fresh,
    DROP COLUMN policy_outcome_id, DROP COLUMN policy_continuation_mode,
    DROP COLUMN policy_resolved_at, DROP COLUMN policy_applying_worker_id,
    DROP COLUMN policy_applying_generation, DROP COLUMN policy_source_generation,
    DROP COLUMN policy_refusal_count, DROP COLUMN policy_refusal_tag,
    DROP COLUMN policy_refusal_phase, DROP COLUMN policy_refusal_correlation_id,
    DROP COLUMN policy_hold_published;
