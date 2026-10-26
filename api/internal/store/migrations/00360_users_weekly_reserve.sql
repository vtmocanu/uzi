-- +goose Up
-- PRD #3522 M1: the per-user weekly usage reserve. users.weekly_reserve_pct is the 7-day
-- utilization percent at which uzi stops spending that user's subscription (NULL = off, the
-- default); users.weekly_reserve_pause_running says whether a running run is also paused.
--
-- Additive: one nullable column and one NOT NULL column with a constant default (no table
-- rewrite). The CHECK is added NOT VALID and validated in 00362, the two-step pattern of
-- 00242/00243, so the ADD takes no validating scan under the ACCESS EXCLUSIVE lock. The ALTER
-- takes a brief lock on users, so the statement times out rather than queue behind a long
-- transaction and stall logins.
--
-- The migration number is a draft: it is renumbered above the live head at landing, the trio
-- (00360, 00361, 00362) together.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users
    ADD COLUMN weekly_reserve_pct smallint,
    ADD COLUMN weekly_reserve_pause_running boolean NOT NULL DEFAULT true,
    ADD CONSTRAINT users_weekly_reserve_pct_check
        CHECK (weekly_reserve_pct IS NULL OR weekly_reserve_pct BETWEEN 1 AND 100) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';

ALTER TABLE users
    DROP COLUMN weekly_reserve_pause_running,
    DROP COLUMN weekly_reserve_pct;
