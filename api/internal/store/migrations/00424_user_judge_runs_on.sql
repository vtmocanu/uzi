-- +goose Up

-- Issue #3257: per-user "Judge runs on". NULL means "same as the run" (today's
-- behaviour: the judge runs on the harness of the run it reviews); 'claude' or 'codex'
-- pins the judge run's harness. Additive only, so an N-1 api keeps working: it never
-- reads or writes the column and every existing user reads NULL.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
--
-- users is a hot table, so the statement times out rather than queue behind a long
-- writer and stall everyone behind it. A nullable ADD COLUMN with no default is
-- metadata-only; the CHECK is added NOT VALID and VALIDATE-d in the sibling 00425, the
-- lock-cheap two-step 00233/00234 established.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users ADD COLUMN judge_runs_on text;
ALTER TABLE users ADD CONSTRAINT users_judge_runs_on_check
    CHECK (judge_runs_on IS NULL OR judge_runs_on IN ('claude', 'codex')) NOT VALID;

-- +goose Down

-- LOSSY: this Down permanently erases every user's Judge runs on choice. goose downs are
-- not run in this deployment (store.Migrate only goes up).
SET LOCAL lock_timeout = '5s';

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_judge_runs_on_check;
ALTER TABLE users DROP COLUMN IF EXISTS judge_runs_on;
