-- +goose Up
SET LOCAL lock_timeout = '5s';

-- users.sidebar_motion: the per-user opt-in for animated sidebar icons (issue #4510).
-- NULLABLE with no default: NULL means "not chosen", which resolves to the instance
-- fallback (off today); true/false is an explicit choice.
ALTER TABLE users ADD COLUMN sidebar_motion boolean;

-- +goose Down
ALTER TABLE users DROP COLUMN sidebar_motion;
