-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Optional compatible-lane Fresh (issue #3607 Mf). Two nullable generation stamps record the single
-- claim that consumed a one-claim Fresh authorization and the generation whose current-generation
-- session report confirmed a new conversation. They carry no cross-column coupling with the
-- earlier policy-hold shape, so an older API writing that shape during a rolling deploy stays valid.
ALTER TABLE runs
    ADD COLUMN policy_fresh_consumed_generation bigint,
    ADD COLUMN policy_fresh_confirmed_generation bigint;
-- NOT VALID: new and updated rows are checked immediately; migration 415 validates it, per the
-- repo's paired NOT VALID / VALIDATE convention. The pending-with-consumed and consumed-without-
-- preserving invariants are deliberately not encoded here (an older API writing during a rolling
-- deploy does not know these columns); the writers and this migration's Down enforce them.
ALTER TABLE runs ADD CONSTRAINT runs_policy_fresh_generation_shape CHECK (
    (policy_fresh_consumed_generation IS NULL OR (policy_fresh_consumed_generation > 0 AND policy_fresh_consumed_generation <= 9007199254740991))
    AND (policy_fresh_confirmed_generation IS NULL
         OR (policy_fresh_consumed_generation IS NOT NULL AND policy_fresh_confirmed_generation = policy_fresh_consumed_generation))
) NOT VALID;
-- +goose Down
SET LOCAL lock_timeout = '30s';
-- Goose runs this migration transactionally. Lock before inspecting or converting so no writer can
-- change a row between the check and the drop. A nonterminal row is ACTIVE when it still holds a
-- pending one-claim Fresh authorization, or consumed one that no current-generation report confirmed:
-- the older API knows neither the one-claim bound nor the unconfirmed reclaim refusal, so an active
-- row must not outlive its enforcement. A confirmed (or retired) history is plain history: neither
-- converted nor refused. Refuse before any mutation when a nonterminal row has an orphaned shape
-- or an active row is not paused or queued (live rows must be drained first); then end the
-- paused/queued active rows as failed with the same terminal bookkeeping as the 00397 rollback,
-- keeping session, custody, captures and sources untouched. Only this migration's columns are
-- dropped; the required-recovery identity of 00413 is not touched.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM runs WHERE status NOT IN ('completed','failed','cancelled')
          AND ((policy_fresh_confirmed_generation IS NOT NULL AND policy_fresh_consumed_generation IS NULL)
               OR ((policy_fresh_consumed_generation IS NOT NULL OR policy_pending_fresh) AND NOT policy_preserve_required)
               OR (policy_pending_fresh AND policy_fresh_consumed_generation IS NOT NULL))
    ) THEN
        RAISE EXCEPTION 'policy one-claim fresh rollback refused: orphaned fresh authorization identity';
    END IF;
    IF EXISTS (
        SELECT 1 FROM runs WHERE status NOT IN ('completed','failed','cancelled')
          AND (policy_pending_fresh
               OR (policy_fresh_consumed_generation IS NOT NULL
                   AND policy_fresh_confirmed_generation IS DISTINCT FROM policy_fresh_consumed_generation))
          AND status NOT IN ('paused','queued')
    ) THEN
        RAISE EXCEPTION 'policy one-claim fresh rollback refused: a live run still carries a one-claim fresh authorization';
    END IF;
END $$;
-- +goose StatementEnd
UPDATE runs SET plan_cross_check_gate_reason=NULL,status='failed',status_since=now(),finished_at=now(),updated_at=now(),
    fail_origin='agent_failure',failure_reason='Policy fresh authorization ended by schema rollback',
    move_pending_since=CASE WHEN issue_iid IS NOT NULL THEN now() END,
    milestones_in_progress=NULL,milestones_agents=NULL,
    pause_requested_at=NULL,pause_mode=NULL,pause_after_count=NULL,
    credential_switch_requested_at=NULL,credential_switch_generation=NULL,
    health='ok',health_reason=NULL,health_since=NULL,
    codex_cap_hash=NULL,codex_claim_epoch=codex_claim_epoch+1
WHERE status IN ('paused','queued')
    AND (policy_pending_fresh
         OR (policy_fresh_consumed_generation IS NOT NULL
             AND policy_fresh_confirmed_generation IS DISTINCT FROM policy_fresh_consumed_generation));
ALTER TABLE runs DROP CONSTRAINT runs_policy_fresh_generation_shape;
ALTER TABLE runs
    DROP COLUMN policy_fresh_consumed_generation,
    DROP COLUMN policy_fresh_confirmed_generation;
