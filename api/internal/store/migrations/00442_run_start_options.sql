-- +goose Up
-- First statement: the ALTER below waits for the runs ACCESS EXCLUSIVE lock, so the bound
-- covers it. A lock wait past it fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';

-- PRD #3250 M1: a run's start-time model and reasoning effort, and where each came from.
-- Plain nullable columns; effort is validated at the write surface (the 00285 precedent) and
-- there is deliberately no pairing CHECK between a value and its source.
-- Added NOT VALID so the CHECK validation scan does not run under the ACCESS EXCLUSIVE lock the
-- ADD COLUMN holds; a NOT VALID CHECK is still enforced on new writes and on the backfill UPDATE
-- below. 00443 validates both in its own transaction (SHARE UPDATE EXCLUSIVE). The backfill UPDATE
-- itself does scan runs (rewriting only rows with a model) while that lock is held; the api is a
-- Recreate singleton migrating at boot, so this lengthens boot, not live traffic.
ALTER TABLE runs
 ADD COLUMN effort text,
 ADD COLUMN model_source text,
 ADD COLUMN effort_source text,
 ADD CONSTRAINT runs_model_source_check CHECK (model_source IN ('run_override', 'schedule_pin')) NOT VALID,
 ADD CONSTRAINT runs_effort_source_check CHECK (effort_source IN ('run_override')) NOT VALID;

-- Every existing run that carries a model got it from a schedule (the only pre-existing writer
-- of runs.model), so the backfill labels it a schedule pin.
UPDATE runs SET model_source = 'schedule_pin' WHERE model IS NOT NULL AND model_source IS NULL;

-- +goose Down
-- Down discards effort choices, and a later re-Up labels every model row schedule_pin again.
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs DROP CONSTRAINT runs_effort_source_check;
ALTER TABLE runs DROP CONSTRAINT runs_model_source_check;
ALTER TABLE runs
 DROP COLUMN effort_source,
 DROP COLUMN model_source,
 DROP COLUMN effort;
