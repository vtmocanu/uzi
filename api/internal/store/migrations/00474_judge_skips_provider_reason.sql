-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4320 M4 (PRD #1429 D4: a derived run that is not created needs a visible reason): the automatic
-- judge of a run is not created for an owner whose Claude source is a model provider, and that skip is
-- recorded as 'provider_lane_unsupported'. This widens the judge_skips.reason CHECK of 00430 by one
-- value. Existing rows satisfy the wider form; the new constraint is NOT VALID here (no full scan under
-- the lock) and validated in 00475. judge_skips is a small table written only by the judge enqueue path.
ALTER TABLE judge_skips DROP CONSTRAINT judge_skips_reason_check;
ALTER TABLE judge_skips ADD CONSTRAINT judge_skips_reason_check
    CHECK (reason IN ('no_usable_credential', 'credential_disabled', 'codex_pool_unavailable', 'provider_lane_unsupported')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Rows carrying the new reason would violate the narrower CHECK; drop them (a skip record is a
-- best-effort explanation, recreated by the next judge refusal). The table is locked first so a
-- concurrent insert of the new reason cannot land between the delete and the constraint swap.
LOCK TABLE judge_skips IN SHARE ROW EXCLUSIVE MODE;
DELETE FROM judge_skips WHERE reason = 'provider_lane_unsupported';
ALTER TABLE judge_skips DROP CONSTRAINT judge_skips_reason_check;
ALTER TABLE judge_skips ADD CONSTRAINT judge_skips_reason_check
    CHECK (reason IN ('no_usable_credential', 'credential_disabled', 'codex_pool_unavailable'));
