-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4207: automatic hibernation of idle workers. The migration number is a draft:
-- it is renumbered above the live head at landing. This migration locks workers only.
--
-- last_wake_settled_at: stamped when a waking worker settles back to none (a hibernation
--   grace anchor, so a worker that just woke is not re-hibernated at once).
-- hibernation_blocked_*: the last reason an automatic pass could not admit the worker, with
--   WHEN it was last seen and SINCE when the reason has held (a changed reason restarts it).
-- Every default is NULL, so there is no table rewrite.
ALTER TABLE workers
 ADD COLUMN last_wake_settled_at timestamptz,
 ADD COLUMN hibernation_blocked_reason text,
 ADD COLUMN hibernation_blocked_at timestamptz,
 ADD COLUMN hibernation_blocked_since timestamptz;

-- A worker that already woke and settled keeps its wake as the grace anchor.
UPDATE workers SET last_wake_settled_at = wake_requested_at
 WHERE hibernation_state = 'none' AND wake_requested_at IS NOT NULL;

-- Staged CHECKs: NOT VALID so no workers scan runs under the ACCESS EXCLUSIVE lock; the next
-- migration validates them.
ALTER TABLE workers
 ADD CONSTRAINT workers_hibernation_blocked_reason_bounds CHECK (
  hibernation_blocked_reason IS NULL OR char_length(hibernation_blocked_reason) BETWEEN 1 AND 64
 ) NOT VALID,
 ADD CONSTRAINT workers_hibernation_blocked_coherent CHECK (
  (hibernation_blocked_reason IS NULL AND hibernation_blocked_at IS NULL AND hibernation_blocked_since IS NULL)
  OR (hibernation_blocked_reason IS NOT NULL AND hibernation_blocked_at IS NOT NULL AND hibernation_blocked_since IS NOT NULL)
 ) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers
 DROP CONSTRAINT workers_hibernation_blocked_coherent,
 DROP CONSTRAINT workers_hibernation_blocked_reason_bounds,
 DROP COLUMN hibernation_blocked_since,
 DROP COLUMN hibernation_blocked_at,
 DROP COLUMN hibernation_blocked_reason,
 DROP COLUMN last_wake_settled_at;
