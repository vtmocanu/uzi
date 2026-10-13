-- +goose Up
ALTER TABLE workers ADD COLUMN registered_pod_uid uuid;
ALTER TABLE workers ADD COLUMN roll_heartbeat_at timestamptz;
ALTER TABLE workers ADD COLUMN draining_reason text CHECK (draining_reason IS NULL OR draining_reason = 'image_roll');

-- +goose Down
ALTER TABLE workers DROP COLUMN draining_reason;
ALTER TABLE workers DROP COLUMN roll_heartbeat_at;
ALTER TABLE workers DROP COLUMN registered_pod_uid;
