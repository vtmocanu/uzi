-- +goose Up

-- Validate the two CHECKs added NOT VALID in 00360 and 00361 (PRD #3522 M1):
-- users_weekly_reserve_pct_check and the widened runs_pause_mode_check (fourth value
-- 'reserve'). VALIDATE CONSTRAINT takes only a SHARE UPDATE EXCLUSIVE lock (reads and writes
-- proceed), unlike an inline validated ADD CONSTRAINT, per the two-step pattern of
-- 00242/00243. The backlog satisfies both by construction: weekly_reserve_pct is new and
-- NULL on every existing row, and no existing row holds pause_mode = 'reserve'.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users VALIDATE CONSTRAINT users_weekly_reserve_pct_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_pause_mode_check;

-- +goose Down

-- There is no VALIDATE inverse, so restore the pre-00362 state (each constraint present but
-- NOT VALID) by dropping and re-adding it NOT VALID. The CHECK bodies match 00360/00361
-- verbatim. Mirrors 00243's Down.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users DROP CONSTRAINT users_weekly_reserve_pct_check;
ALTER TABLE users ADD CONSTRAINT users_weekly_reserve_pct_check
    CHECK (weekly_reserve_pct IS NULL OR weekly_reserve_pct BETWEEN 1 AND 100) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_pause_mode_check;
ALTER TABLE runs ADD CONSTRAINT runs_pause_mode_check
    CHECK (pause_mode IS NULL OR pause_mode IN ('milestone', 'now', 'wall', 'reserve')) NOT VALID;
