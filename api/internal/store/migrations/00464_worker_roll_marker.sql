-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Display-only controller roll-marker summary (issue #4385). Nothing in claim,
-- heartbeat, register or scheduling reads these columns.
ALTER TABLE worker_upgrade_reports
    ADD COLUMN roll_marker jsonb,
    ADD COLUMN roll_marker_observed_at timestamptz,
    ADD CONSTRAINT worker_upgrade_reports_roll_marker_bound CHECK (
        roll_marker IS NULL OR (jsonb_typeof(roll_marker) = 'object' AND octet_length(roll_marker::text) <= 1024)
    );

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE worker_upgrade_reports
    DROP CONSTRAINT worker_upgrade_reports_roll_marker_bound,
    DROP COLUMN roll_marker_observed_at,
    DROP COLUMN roll_marker;
