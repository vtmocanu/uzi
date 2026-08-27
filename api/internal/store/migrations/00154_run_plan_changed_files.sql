-- +goose Up
ALTER TABLE runs ADD COLUMN plan_changed_files text[];
-- +goose Down
ALTER TABLE runs DROP COLUMN plan_changed_files;
