-- +goose Up
ALTER TABLE runs ADD COLUMN ci_config_paths text[];
-- +goose Down
ALTER TABLE runs DROP COLUMN ci_config_paths;
