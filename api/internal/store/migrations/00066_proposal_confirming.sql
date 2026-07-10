-- +goose Up

-- Claim-first issue-proposal confirmation (PRD #39 M3, audit Minor #1). Confirm is
-- now two-phase to make concurrent double-confirm impossible: an atomic
-- pending -> confirming claim (only one caller wins) BEFORE the forge CreateIssue,
-- then confirming -> confirmed on success (or confirming -> pending on forge
-- failure, so the user can retry). 'confirming' is a transient in-flight state; a
-- proposal only rests in pending/confirmed/dismissed.
ALTER TABLE issue_proposals DROP CONSTRAINT issue_proposals_status_check;
ALTER TABLE issue_proposals ADD CONSTRAINT issue_proposals_status_check
    CHECK (status IN ('pending', 'confirming', 'confirmed', 'dismissed'));

-- +goose Down

-- A row stuck in 'confirming' (a confirm in flight at down time) would violate the
-- reverted CHECK; settle it to pending first, as with any relaxed-then-tightened
-- column.
UPDATE issue_proposals SET status = 'pending' WHERE status = 'confirming';
ALTER TABLE issue_proposals DROP CONSTRAINT issue_proposals_status_check;
ALTER TABLE issue_proposals ADD CONSTRAINT issue_proposals_status_check
    CHECK (status IN ('pending', 'confirmed', 'dismissed'));
