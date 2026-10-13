-- +goose Up
ALTER TABLE workers DROP CONSTRAINT workers_draining_reason_check;
ALTER TABLE workers ADD CONSTRAINT workers_draining_reason_check
    CHECK (draining_reason IS NULL OR draining_reason IN ('image_roll', 'pressure_recycle'));

-- +goose Down
-- Retain the scheduling fence while relinquishing only pressure-recycle ownership.
UPDATE workers SET draining_reason = NULL WHERE draining_reason = 'pressure_recycle';
ALTER TABLE workers DROP CONSTRAINT workers_draining_reason_check;
ALTER TABLE workers ADD CONSTRAINT workers_draining_reason_check
    CHECK (draining_reason IS NULL OR draining_reason = 'image_roll');
