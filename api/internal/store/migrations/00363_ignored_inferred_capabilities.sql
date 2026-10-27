-- +goose Up
ALTER TABLE repos ADD COLUMN ignored_inferred_capabilities text[] NOT NULL DEFAULT '{}';
-- New runs capture the repo policy in the same INSERT snapshot as the declared hint.
-- Empty defaults preserve existing runs and repo-less run kinds.
ALTER TABLE runs ADD COLUMN ignored_inferred_capabilities text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE runs DROP COLUMN ignored_inferred_capabilities;
ALTER TABLE repos DROP COLUMN ignored_inferred_capabilities;
