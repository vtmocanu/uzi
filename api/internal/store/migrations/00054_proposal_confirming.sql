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

-- confirming_since is stamped when a proposal is claimed (pending -> confirming) and
-- cleared when it settles. It bounds the transient state: if a confirm handler is
-- killed AFTER the claim commits but BEFORE it settles/reverts, the proposal would
-- otherwise be stranded in 'confirming' (unclaimable, unconfirmable, undismissable).
-- The sweeper reverts rows whose confirming_since is older than PROPOSAL_CONFIRM_STUCK_TIMEOUT
-- back to pending (mirrors SweepClaimedNeverStarted for stale run claims).
ALTER TABLE issue_proposals ADD COLUMN confirming_since timestamptz;

-- +goose Down
ALTER TABLE issue_proposals DROP COLUMN confirming_since;

-- A row stuck in 'confirming' (a confirm in flight at down time) would violate the
-- reverted CHECK; settle it to pending first, as with any relaxed-then-tightened
-- column.
UPDATE issue_proposals SET status = 'pending' WHERE status = 'confirming';
ALTER TABLE issue_proposals DROP CONSTRAINT issue_proposals_status_check;
ALTER TABLE issue_proposals ADD CONSTRAINT issue_proposals_status_check
    CHECK (status IN ('pending', 'confirmed', 'dismissed'));
