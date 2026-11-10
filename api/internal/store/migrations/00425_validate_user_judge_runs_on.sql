-- +goose Up

SET LOCAL lock_timeout = '5s';

-- Validate the CHECK added NOT VALID in 00424 (issue #3257). VALIDATE CONSTRAINT scans
-- the table but takes only a SHARE UPDATE EXCLUSIVE lock (write-compatible), unlike the
-- ACCESS EXCLUSIVE lock an inline validated ADD CONSTRAINT would hold. Split from 00424
-- per the two-step pattern (00233/00234).
--
-- The backlog satisfies the CHECK by construction: judge_runs_on is NULL on every
-- existing row. Both numbers are drafts, renumbered above the live head at landing.
ALTER TABLE users VALIDATE CONSTRAINT users_judge_runs_on_check;

-- +goose Down

SET LOCAL lock_timeout = '5s';

-- There is no VALIDATE inverse, so restore the pre-00425 state (constraint present but
-- NOT VALID) by dropping and re-adding it NOT VALID. The body matches 00424's Up
-- verbatim; 00424's Down then drops it. Mirrors 00234's Down.
ALTER TABLE users DROP CONSTRAINT users_judge_runs_on_check;
ALTER TABLE users ADD CONSTRAINT users_judge_runs_on_check
    CHECK (judge_runs_on IS NULL OR judge_runs_on IN ('claude', 'codex')) NOT VALID;
