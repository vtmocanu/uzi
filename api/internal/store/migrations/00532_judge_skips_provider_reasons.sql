-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4322 M1a: a provider-bound Claude judge can now be created, so its refusals get their own
-- visible reasons (PRD #1429 D4) instead of 'provider_lane_unsupported' (kept for a Codex judge whose
-- owner has a Codex source, and for historical rows). This widens the judge_skips.reason CHECK of 00484
-- by two values: provider_source_unusable and provider_model_unmapped. Existing rows satisfy the wider
-- form; the new constraint is NOT VALID here (no full scan under the lock) and validated in 00533.
ALTER TABLE judge_skips DROP CONSTRAINT judge_skips_reason_check;
ALTER TABLE judge_skips ADD CONSTRAINT judge_skips_reason_check
    CHECK (reason IN ('no_usable_credential', 'credential_disabled', 'codex_pool_unavailable', 'provider_lane_unsupported', 'provider_source_unusable', 'provider_model_unmapped')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Rows carrying the new reasons would violate the narrower CHECK; drop them (a skip record is a
-- best-effort explanation, recreated by the next judge refusal). The table is locked first so a
-- concurrent insert of a new reason cannot land between the delete and the constraint swap.
LOCK TABLE judge_skips IN SHARE ROW EXCLUSIVE MODE;
DELETE FROM judge_skips WHERE reason IN ('provider_source_unusable', 'provider_model_unmapped');
ALTER TABLE judge_skips DROP CONSTRAINT judge_skips_reason_check;
ALTER TABLE judge_skips ADD CONSTRAINT judge_skips_reason_check
    CHECK (reason IN ('no_usable_credential', 'credential_disabled', 'codex_pool_unavailable', 'provider_lane_unsupported'));
