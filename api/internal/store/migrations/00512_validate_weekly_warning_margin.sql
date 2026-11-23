-- +goose Up

-- Validate the CHECK added NOT VALID in 00511 (PRD #4123 M2): users_weekly_warning_margin_pts_check.
-- VALIDATE CONSTRAINT takes only a SHARE UPDATE EXCLUSIVE lock (reads and writes proceed), unlike an
-- inline validated ADD CONSTRAINT, per the two-step pattern of 00242/00243 and 00366/00368. Every
-- existing row satisfies it by construction: the column is new and holds its default 15.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users VALIDATE CONSTRAINT users_weekly_warning_margin_pts_check;

-- +goose Down

-- There is no VALIDATE inverse, so restore the pre-00512 state (the constraint present but NOT VALID)
-- by dropping and re-adding it NOT VALID. The CHECK body matches 00511 verbatim. Mirrors 00368's Down.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users DROP CONSTRAINT users_weekly_warning_margin_pts_check;
ALTER TABLE users ADD CONSTRAINT users_weekly_warning_margin_pts_check
    CHECK (weekly_warning_margin_pts BETWEEN 0 AND 50) NOT VALID;
