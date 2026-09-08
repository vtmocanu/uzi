-- +goose Up

-- Validate the two CHECKs added NOT VALID in 00204 (PRD #1190 M1): the widened
-- runs_status_check (twelfth value 'paused') and the widened run_user_inputs_kind_check
-- (pause/pause_cancel/resume). VALIDATE CONSTRAINT scans each table to confirm existing rows
-- satisfy the CHECK, but takes only a SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent
-- reads and writes proceed), unlike the ACCESS EXCLUSIVE lock an inline validated ADD
-- CONSTRAINT ... CHECK would hold. Split from 00204 so the add is lock-cheap and the validation
-- is non-blocking, per the standard two-step pattern for a CHECK on a live table.
ALTER TABLE runs VALIDATE CONSTRAINT runs_status_check;
ALTER TABLE run_user_inputs VALIDATE CONSTRAINT run_user_inputs_kind_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00205 state — each constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK bodies match 00204's Up verbatim. 00204's Down then narrows/drops them
-- as before.
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup',
                      'pool_wait', 'paused')) NOT VALID;
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume')) NOT VALID;
