-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4395: persist the last fresh memory-authority held report. The migration number is a
-- draft: it is renumbered above the live head at landing. This migration locks workers only.
--
-- memory_authority_held_at: when a fenced held report on a snapshot-carrying run-lane claim of the
-- worker's CURRENT registration was last accepted; NULL until then. Register and a lost report
-- clear it, so a held receipt never outlives the registration or the loss it contradicts. Pool
-- readiness for a worker with both memory capabilities requires it set and fresh within the
-- heartbeat-staleness window (an unknown memory authority is not ready). NULL default, so there
-- is no table rewrite.
ALTER TABLE workers
 ADD COLUMN memory_authority_held_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers
 DROP COLUMN memory_authority_held_at;
