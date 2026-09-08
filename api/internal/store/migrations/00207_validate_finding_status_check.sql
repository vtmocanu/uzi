-- +goose Up

-- Validate the two CHECKs added NOT VALID in 00206 (PRD #1183 Child B, M3): the widened
-- finding_dispositions_status_check (fifth value 'done') and the finding_dispositions_set_via_check.
-- VALIDATE CONSTRAINT scans the table to confirm existing rows satisfy the CHECK, but takes only
-- a SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent reads and writes proceed), unlike
-- the ACCESS EXCLUSIVE lock an inline validated ADD CONSTRAINT ... CHECK would hold. Split from
-- 00206 so the add is lock-cheap and the validation is non-blocking, per the standard two-step
-- pattern for a CHECK on a live table (00204/00205).
ALTER TABLE finding_dispositions VALIDATE CONSTRAINT finding_dispositions_status_check;
ALTER TABLE finding_dispositions VALIDATE CONSTRAINT finding_dispositions_set_via_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00207 state — each constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK bodies match 00206's Up verbatim. 00206's Down then narrows/drops them
-- as before.
ALTER TABLE finding_dispositions DROP CONSTRAINT finding_dispositions_status_check;
ALTER TABLE finding_dispositions ADD CONSTRAINT finding_dispositions_status_check
    CHECK (status IN ('open', 'filing', 'filed', 'dismissed', 'done')) NOT VALID;
ALTER TABLE finding_dispositions DROP CONSTRAINT finding_dispositions_set_via_check;
ALTER TABLE finding_dispositions ADD CONSTRAINT finding_dispositions_set_via_check
    CHECK (set_via IS NULL OR set_via IN ('issue_close')) NOT VALID;
