-- +goose Up
ALTER TABLE pr_description_versions ADD COLUMN region_has_diagram boolean;

-- +goose Down
ALTER TABLE pr_description_versions DROP COLUMN region_has_diagram;
