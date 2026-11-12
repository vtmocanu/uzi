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

-- The older CHECK cannot represent the two new values: clear them, never delete the hold. The
-- completed_publication_hold_guard trigger freezes the reason once a hold is no longer open, so it
-- is disabled for the normalization only (the 00333 Down does the same for its guard).
ALTER TABLE recovery_custody_holds DISABLE TRIGGER completed_publication_hold_guard;
UPDATE recovery_custody_holds SET completed_publication_reason = NULL
    WHERE completed_publication_reason IN ('terminal_preservation_pending', 'terminal_preservation_refused');
ALTER TABLE recovery_custody_holds ENABLE TRIGGER completed_publication_hold_guard;

ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_completed_publication_reason_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_completed_publication_reason_check
    CHECK (completed_publication_reason IN (
        'completion_identity_missing', 'not_completed', 'identity_changed', 'mr_missing',
        'branch_missing', 'branch_mismatch', 'head_mismatch', 'not_ancestor',
        'ancestry_unknown', 'forge_timeout'));
