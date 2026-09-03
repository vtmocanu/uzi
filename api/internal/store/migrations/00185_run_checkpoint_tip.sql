-- +goose Up
ALTER TABLE runs ADD COLUMN checkpoint_tip TEXT;
-- +goose Down
ALTER TABLE runs DROP COLUMN checkpoint_tip;
