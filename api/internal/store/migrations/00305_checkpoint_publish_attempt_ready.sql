-- +goose Up
-- Deploy before the application. Existing and explicit-column inserts remain pending.
ALTER TABLE checkpoint_publish_attempts
    ADD COLUMN reconcile_ready_at timestamptz DEFAULT NULL;

-- +goose Down
-- Application rollback retains the additive column and recorded evidence.
SELECT 1;
