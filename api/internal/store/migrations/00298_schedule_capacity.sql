-- +goose Up
ALTER TABLE run_schedules
 ADD COLUMN capacity_limit integer NULL,
 ADD COLUMN capacity_room_needed integer NULL,
 ADD CONSTRAINT run_schedules_capacity_check CHECK ((capacity_limit IS NULL AND capacity_room_needed IS NULL) OR (capacity_limit IS NOT NULL AND capacity_room_needed IS NOT NULL AND capacity_room_needed>=1 AND capacity_room_needed<=capacity_limit AND capacity_limit<=50));

-- +goose Down
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_capacity_check,
 DROP COLUMN capacity_room_needed, DROP COLUMN capacity_limit;
