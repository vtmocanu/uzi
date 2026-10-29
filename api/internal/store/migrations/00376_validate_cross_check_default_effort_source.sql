-- +goose Up
-- Validate the CHECK added NOT VALID in 00375. VALIDATE CONSTRAINT takes only a SHARE
-- UPDATE EXCLUSIVE lock, so reads and writes proceed during the scan (00368 pattern).
SET LOCAL lock_timeout = '5s';
ALTER TABLE cross_checks VALIDATE CONSTRAINT cross_checks_checker_effort_source_check;

-- +goose Down
-- There is no VALIDATE inverse, so restore the post-00375 state (the same three-value
-- CHECK, present but NOT VALID) by dropping and re-adding it. Mirrors 00368's Down.
SET LOCAL lock_timeout = '5s';
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_checker_effort_source_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_checker_effort_source_check
    CHECK (checker_effort_source IN ('pin', 'worker default', 'cross-check default')) NOT VALID;
