-- +goose Up
-- Issue #4007: the completed-release gate can hold a release behind an unfinished or refused
-- terminal preservation. Two new completed_publication_reason values record why:
-- terminal_preservation_pending and terminal_preservation_refused.
--
-- The CHECK is widened (relaxed) with the lock-cheap two-step 00428/00429 established: the
-- constraint is re-added NOT VALID here and VALIDATE-d by the sibling migration after this one.
-- Only recovery_custody_holds, a table ClaimRun writes, is touched, so the statement times out
-- rather than queue behind it. The constraint name is the default one 00318 created.
--
-- Both numbers are drafts, renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_completed_publication_reason_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_completed_publication_reason_check
    CHECK (completed_publication_reason IN (
        'completion_identity_missing', 'not_completed', 'identity_changed', 'mr_missing',
        'branch_missing', 'branch_mismatch', 'head_mismatch', 'not_ancestor',
        'ancestry_unknown', 'forge_timeout',
        'terminal_preservation_pending', 'terminal_preservation_refused')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';

-- A rolled-back chain must refuse before it clears anything: the same live-rows refusal the 00449
-- and 00448 Downs make, repeated here because this Down runs first and would otherwise null the
-- reasons of a rollback that those two then refuse.
-- Lock order matches the writers' (worker, run, hold, preservation, contribution): the hold table
-- first. ACCESS EXCLUSIVE is the mode the CHECK swap below needs, so no lock upgrade happens
-- mid-transaction, which is the usual deadlock shape. This Down drops no table; the 00449 and 00448
-- Downs that follow take their own up-front locks.
-- A writer already holding a row lock blocks this statement until it finishes (bounded by the
-- lock_timeout above), and once the locks are held no writer can start; the order above is the
-- writers' own, so the Down never waits on a lock a writer would take later.
LOCK TABLE recovery_custody_holds IN ACCESS EXCLUSIVE MODE;
LOCK TABLE run_terminal_preservations, run_terminal_contributions IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM run_terminal_preservations WHERE live_run_id IS NOT NULL)
        OR EXISTS (SELECT 1 FROM run_terminal_contributions WHERE state IN ('invoked', 'publish_unknown')) THEN
        RAISE EXCEPTION 'run_terminal_preservations has live rows; delete the refs/uzi-terminal refs first';
    END IF;
END $$;
-- +goose StatementEnd

-- The older CHECK cannot represent the two new values: clear them, never delete the hold. The
-- completed_publication_hold_guard trigger freezes the reason once a hold is no longer open, and
-- merged_predecessor_discard_guard (00338) rejects any change to a hold carrying a merged
-- predecessor discard binding, so both are disabled for the normalization only (the 00333 Down
-- does the same for its guard).
ALTER TABLE recovery_custody_holds DISABLE TRIGGER completed_publication_hold_guard;
ALTER TABLE recovery_custody_holds DISABLE TRIGGER merged_predecessor_discard_guard;
UPDATE recovery_custody_holds SET completed_publication_reason = NULL
    WHERE completed_publication_reason IN ('terminal_preservation_pending', 'terminal_preservation_refused');
ALTER TABLE recovery_custody_holds ENABLE TRIGGER merged_predecessor_discard_guard;
ALTER TABLE recovery_custody_holds ENABLE TRIGGER completed_publication_hold_guard;

ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_completed_publication_reason_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_completed_publication_reason_check
    CHECK (completed_publication_reason IN (
        'completion_identity_missing', 'not_completed', 'identity_changed', 'mr_missing',
        'branch_missing', 'branch_mismatch', 'head_mismatch', 'not_ancestor',
        'ancestry_unknown', 'forge_timeout'));
