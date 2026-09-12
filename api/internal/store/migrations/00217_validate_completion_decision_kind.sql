-- +goose Up

-- Validate the CHECK added NOT VALID in 00214 (PRD #1226 M5, D7): the widened
-- run_user_inputs_kind_check (twelfth kind 'completion_decision'). VALIDATE CONSTRAINT
-- scans the table to confirm existing rows satisfy the CHECK, but takes only a SHARE UPDATE
-- EXCLUSIVE lock (write-compatible: concurrent reads and writes proceed), unlike the ACCESS
-- EXCLUSIVE lock an inline validated ADD CONSTRAINT ... CHECK would hold. Split from 00214
-- so the add is lock-cheap and the validation is non-blocking, per the standard two-step
-- pattern (00204/00205) for a CHECK on a live table.
ALTER TABLE run_user_inputs VALIDATE CONSTRAINT run_user_inputs_kind_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00215 state — the constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK body matches 00214's Up verbatim (incl. 'completion_decision').
-- 00214's Down then narrows it as before. Mirrors 00205's Down exactly.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume', 'completion_decision')) NOT VALID;
