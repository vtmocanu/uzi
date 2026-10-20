-- +goose Up
ALTER TABLE runs ADD COLUMN completion_non_closing_reason text;

-- +goose Down
ALTER TABLE runs DROP COLUMN completion_non_closing_reason;
