-- +goose Up
ALTER TABLE worker_upgrade_reports ADD COLUMN blocking_kind text;
COMMENT ON COLUMN worker_upgrade_reports.blocking_kind IS 'Display-only roll blocking classification; does not control scheduling.';

-- +goose Down
ALTER TABLE worker_upgrade_reports DROP COLUMN blocking_kind;
