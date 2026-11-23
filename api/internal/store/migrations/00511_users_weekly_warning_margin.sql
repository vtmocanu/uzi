-- +goose Up
-- PRD #4123 M2: the per-user weekly-limit warning margin. users.weekly_warning_margin_pts is how many
-- percentage points below its effective weekly limit (the usage reserve, else 100) a credential must
-- be before uzi shows the early banner and the `limit` decision item; 0 turns the early warnings off
-- (the park explanations are unaffected). The default is 15.
--
-- Additive: one NOT NULL column with a constant default (no table rewrite). The CHECK is added NOT
-- VALID and validated in 00512, the two-step pattern of 00366/00368, so the ADD takes no validating
-- scan under the ACCESS EXCLUSIVE lock. The ALTER takes a brief lock on users, so the statement times
-- out rather than queue behind a long transaction and stall logins.
--
-- The migration number is a draft: it is renumbered above the live head at landing, the pair (00511,
-- 00512) together.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users
    ADD COLUMN weekly_warning_margin_pts smallint NOT NULL DEFAULT 15,
    ADD CONSTRAINT users_weekly_warning_margin_pts_check
        CHECK (weekly_warning_margin_pts BETWEEN 0 AND 50) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';

ALTER TABLE users
    DROP COLUMN weekly_warning_margin_pts;
