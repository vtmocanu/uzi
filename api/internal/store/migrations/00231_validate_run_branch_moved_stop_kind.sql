-- +goose Up

-- Validate the CHECK added NOT VALID in 00230 (issue #1117): the widened
-- runs_stop_kind_check (seventh value 'branch_moved'). VALIDATE CONSTRAINT scans the table to
-- confirm existing rows satisfy the CHECK, but takes only a SHARE UPDATE EXCLUSIVE lock
-- (write-compatible: concurrent reads and writes proceed), unlike the ACCESS EXCLUSIVE lock an
-- inline validated ADD CONSTRAINT ... CHECK would hold. Split from 00230 so the add is lock-cheap
-- and the validation is non-blocking, per the standard two-step pattern (00224/00225) for a CHECK
-- on a live table.
ALTER TABLE runs VALIDATE CONSTRAINT runs_stop_kind_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00231 state — the constraint present but NOT VALID — by dropping and re-adding it NOT VALID.
-- The CHECK body matches 00230's Up verbatim (incl. 'branch_moved'). 00230's Down then narrows it
-- as before. Mirrors 00225's Down exactly: goose runs this Down first (re-adding the widened list
-- NOT VALID), then 00230's Down drops and re-adds the narrower validated list.
ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped', 'scope_capped', 'scope_reduced', 'branch_moved')) NOT VALID;
