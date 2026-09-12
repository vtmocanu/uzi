-- +goose Up

-- Validate the two CHECKs added NOT VALID in 00219 (PRD #1189 M1): the widened
-- run_user_inputs_kind_check (twelfth value 'extend') and runs_budget_extension_seconds_check
-- (budget_extension_seconds >= 0). VALIDATE CONSTRAINT scans the table to confirm existing rows
-- satisfy the CHECK, but takes only a SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent
-- reads and writes proceed), unlike the ACCESS EXCLUSIVE lock an inline validated
-- ADD CONSTRAINT ... CHECK would hold. Split from 00219 so the adds are lock-cheap and the
-- validation is non-blocking, per the standard two-step pattern for a CHECK on a live table.
ALTER TABLE run_user_inputs VALIDATE CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_budget_extension_seconds_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00220 state — each constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK bodies match 00219's Up verbatim. 00219's Down then narrows/drops them
-- as before.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume', 'completion_decision', 'extend')) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_budget_extension_seconds_check;
ALTER TABLE runs ADD CONSTRAINT runs_budget_extension_seconds_check
    CHECK (budget_extension_seconds >= 0) NOT VALID;
