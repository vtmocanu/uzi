-- +goose Up
-- Generic per-user secret store, kind-keyed so future per-user secrets need no
-- new table shape (a new kind is one ALTER-CHECK migration). ciphertext holds
-- secretbox.Seal(secret) — a DB dump alone never reveals a plaintext secret.
CREATE TABLE user_secrets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('anthropic_token')),
    ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, kind)
);

-- +goose Down
DROP TABLE user_secrets;
