-- +goose Up
-- First statement: each VALIDATE waits for its table lock, so the bound must cover them all.
-- A lock wait past it fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';
-- 00442 added both CHECKs NOT VALID. VALIDATE takes SHARE UPDATE EXCLUSIVE instead of the
-- ACCESS EXCLUSIVE an inline validated ADD CONSTRAINT would hold: reads and writes continue.
ALTER TABLE runs VALIDATE CONSTRAINT runs_model_source_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_effort_source_check;

-- +goose Down
-- Validation has no data or schema reversal.
