-- +goose Up
ALTER TABLE run_schedules ADD COLUMN remove_label_on_dispatch boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE run_schedules DROP COLUMN remove_label_on_dispatch;
