-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4395: persist memory-authority loss. The migration number is a draft: it is
-- renumbered above the live head at landing. This migration locks workers only.
--
-- memory_authority_lost_at: when a run-lane claim first reported lost memory authority for
-- the worker's CURRENT registration; NULL while held or never reported lost. Register clears
-- it, so a loss never outlives the registration that reported it. NULL default, so there is
-- no table rewrite.
ALTER TABLE workers
 ADD COLUMN memory_authority_lost_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers
 DROP COLUMN memory_authority_lost_at;
