-- +goose Up

-- Validate the set_via CHECK widened NOT VALID in 00215 (PRD #1184 M2): the three-value
-- recommendation_dispositions_set_via_check that now admits 'admin'. VALIDATE CONSTRAINT scans
-- the table to confirm existing rows satisfy the CHECK but takes only a SHARE UPDATE EXCLUSIVE
-- lock (write-compatible: concurrent reads and writes proceed), unlike the ACCESS EXCLUSIVE lock
-- an inline validated ADD CONSTRAINT ... CHECK would hold. Split from 00215 so the add is
-- lock-cheap and the validation is non-blocking, the standard two-step pattern for a CHECK on a
-- live table (00209/00210).
ALTER TABLE recommendation_dispositions VALIDATE CONSTRAINT recommendation_dispositions_set_via_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00216 state — the constraint present but NOT VALID — by dropping and re-adding it NOT VALID.
-- The CHECK body matches 00215's Up verbatim (the widened three-value domain). 00215's Down then
-- re-narrows it as before.
ALTER TABLE recommendation_dispositions DROP CONSTRAINT recommendation_dispositions_set_via_check;
ALTER TABLE recommendation_dispositions ADD CONSTRAINT recommendation_dispositions_set_via_check
    CHECK (set_via IS NULL OR set_via IN ('issue_close', 'denied_cli', 'admin')) NOT VALID;
