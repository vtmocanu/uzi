-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4784: per-user JSON documents for runtime web modules. One document per
-- (user, module). Revisions come from a single sequence and are never reused, so a
-- stale writer can never match a later document. Rolling back deletes every stored
-- document and resets the sequence.
CREATE SEQUENCE module_user_data_revision_seq AS bigint NO CYCLE;

CREATE TABLE module_user_data (
    user_id    uuid        NOT NULL REFERENCES users ON DELETE CASCADE,
    module_id  text        NOT NULL CHECK (module_id ~ '^[a-z][a-z0-9-]{0,31}$'),
    data       jsonb       NOT NULL CHECK (jsonb_typeof(data) <> 'null'),
    revision   bigint      NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, module_id)
);

-- +goose Down
DROP TABLE module_user_data;
DROP SEQUENCE module_user_data_revision_seq;
