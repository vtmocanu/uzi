-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4395 (warm reserve): op-bound origin markers on the hibernation op. The migration
-- number is a draft: it is renumbered above the live head at landing. This migration locks
-- workers only.
--
-- hibernation_origin_op_id: the op the two origin markers below belong to. A marker is valid
--   only while it equals hibernation_op_id; anything else reads as legacy.
-- hibernation_admit_origin: who admitted the op (manual, auto_long, auto_surplus).
-- hibernation_wake_origin: who moved the op to waking (manual, demand, system, warm).
-- last_manual_wake_settled_at: the durable manual-grade grace anchor for the owner-wide
--   long idle clock.
-- Every default is NULL and nothing is backfilled, so there is no table rewrite and existing
-- rows read as legacy.
ALTER TABLE workers
 ADD COLUMN hibernation_origin_op_id uuid,
 ADD COLUMN hibernation_admit_origin text,
 ADD COLUMN hibernation_wake_origin text,
 ADD COLUMN last_manual_wake_settled_at timestamptz;

-- Staged CHECKs: NOT VALID defers validating existing rows to the next migration. A wake op
-- carries only a wake origin, so no CHECK requires an admit origin.
ALTER TABLE workers
 ADD CONSTRAINT workers_hibernation_admit_origin_domain CHECK (
  hibernation_admit_origin IS NULL OR hibernation_admit_origin IN ('manual', 'auto_long', 'auto_surplus')
 ) NOT VALID,
 ADD CONSTRAINT workers_hibernation_wake_origin_domain CHECK (
  hibernation_wake_origin IS NULL OR hibernation_wake_origin IN ('manual', 'demand', 'system', 'warm')
 ) NOT VALID,
 ADD CONSTRAINT workers_hibernation_origin_coherent CHECK (
  hibernation_origin_op_id IS NOT NULL
  OR (hibernation_admit_origin IS NULL AND hibernation_wake_origin IS NULL)
 ) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers
 DROP CONSTRAINT workers_hibernation_origin_coherent,
 DROP CONSTRAINT workers_hibernation_wake_origin_domain,
 DROP CONSTRAINT workers_hibernation_admit_origin_domain,
 DROP COLUMN last_manual_wake_settled_at,
 DROP COLUMN hibernation_wake_origin,
 DROP COLUMN hibernation_admit_origin,
 DROP COLUMN hibernation_origin_op_id;
