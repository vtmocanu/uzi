-- +goose Up

-- Validate the two CHECKs added NOT VALID in 00241 (PRD #1497 M1): the widened
-- runs_pause_mode_check (third value 'wall') and runs_budget_finalize_seconds_check
-- (budget_finalize_seconds >= 0). VALIDATE CONSTRAINT scans the runs table to confirm
-- existing rows satisfy the CHECK but takes only a SHARE UPDATE EXCLUSIVE lock
-- (write-compatible: concurrent reads and writes proceed), unlike the ACCESS EXCLUSIVE lock
-- an inline validated ADD CONSTRAINT ... CHECK would hold. Split from 00241 so the adds are
-- lock-cheap and the validation non-blocking, per the two-step pattern (00219/00220) for a
-- CHECK on a live table.
--
-- The backlog satisfies both CHECKs by construction: no existing row has pause_mode = 'wall'
-- (00241 is the first migration to admit it), and budget_finalize_seconds was added NOT NULL
-- DEFAULT 0 so every existing row is >= 0.
ALTER TABLE runs VALIDATE CONSTRAINT runs_pause_mode_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_budget_finalize_seconds_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00242 state — each constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK bodies match 00241's Up verbatim; 00241's Down then narrows/drops
-- them as before. Mirrors 00220's Down exactly.
ALTER TABLE runs DROP CONSTRAINT runs_pause_mode_check;
ALTER TABLE runs ADD CONSTRAINT runs_pause_mode_check
    CHECK (pause_mode IS NULL OR pause_mode IN ('milestone', 'now', 'wall')) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_budget_finalize_seconds_check;
ALTER TABLE runs ADD CONSTRAINT runs_budget_finalize_seconds_check
    CHECK (budget_finalize_seconds >= 0) NOT VALID;
