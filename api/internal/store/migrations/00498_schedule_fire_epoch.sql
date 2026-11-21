-- +goose Up
-- Issue #4512 (ADR-4007 T3a): the occurrence identity of a one-time schedule's fire.
-- ALTER TABLE takes ACCESS EXCLUSIVE on run_schedules (the column default is a constant,
-- so no rewrite) and CREATE TRIGGER takes SHARE ROW EXCLUSIVE: fail fast instead of
-- queueing behind a long transaction (the 00402 pattern).
SET LOCAL lock_timeout = '5s';

ALTER TABLE run_schedules ADD COLUMN fire_epoch bigint NOT NULL DEFAULT 0;

-- The bump lives in the database, not in application code, so an older binary's re-arm
-- of a one-time row still starts a new occurrence. INSERTs never fire it (a new row
-- starts at epoch 0). The WHEN predicate below is the whole policy; this body only bumps.
-- +goose StatementBegin
CREATE FUNCTION run_schedules_bump_fire_epoch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.fire_epoch := OLD.fire_epoch + 1;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER run_schedules_fire_epoch BEFORE UPDATE ON run_schedules
FOR EACH ROW
WHEN ((OLD.timing = 'once' OR NEW.timing = 'once') AND (
       (OLD.status IS DISTINCT FROM 'active' AND NEW.status = 'active')
    OR NEW.run_at  IS DISTINCT FROM OLD.run_at
    OR NEW.timing  IS DISTINCT FROM OLD.timing
    OR NEW.target  IS DISTINCT FROM OLD.target
    OR NEW.repo_id IS DISTINCT FROM OLD.repo_id))
EXECUTE FUNCTION run_schedules_bump_fire_epoch();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER run_schedules_fire_epoch ON run_schedules;
DROP FUNCTION run_schedules_bump_fire_epoch();
ALTER TABLE run_schedules DROP COLUMN fire_epoch;
