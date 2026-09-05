-- +goose Up

-- Validate the run_schedules.output_mode CHECK added NOT VALID in 00193 (PRD #929 M1).
-- VALIDATE CONSTRAINT scans the table to confirm existing rows satisfy the CHECK, but takes
-- only a SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent reads and writes proceed),
-- unlike the ACCESS EXCLUSIVE lock an inline ADD CONSTRAINT ... CHECK would hold. Split from
-- 00193 so the add is lock-cheap and the validation is non-blocking, per the standard
-- two-step pattern for a CHECK on a live table.
ALTER TABLE run_schedules VALIDATE CONSTRAINT run_schedules_output_mode_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00194 state — the constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. 00193's Down then drops it (and the column) as before.
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_output_mode_check;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_output_mode_check CHECK (output_mode IS NULL OR output_mode IN ('mr','issues')) NOT VALID;
