-- +goose Up
ALTER TABLE worker_upgrade_reports
    ADD COLUMN pacing jsonb,
    ADD COLUMN pacing_observed_at timestamptz,
    ADD CONSTRAINT worker_upgrade_reports_pacing_bound CHECK (
        pacing IS NULL OR (jsonb_typeof(pacing) = 'object' AND octet_length(pacing::text) <= 4096)
    );

-- +goose Down
ALTER TABLE worker_upgrade_reports
    DROP CONSTRAINT worker_upgrade_reports_pacing_bound,
    DROP COLUMN pacing_observed_at,
    DROP COLUMN pacing;
