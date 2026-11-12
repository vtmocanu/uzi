-- +goose Up

-- A user's connection to an external issue tracker (PRD #4173) via their own
-- permanent token. The token is stored encrypted (AES-256-GCM, with the user,
-- kind and base_url bound in as additional authenticated data); a DB dump
-- alone cannot recover it. One connection per (user, kind, base_url).
CREATE TABLE issue_tracker_connections (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    kind             text NOT NULL CHECK (kind IN ('youtrack')),
    base_url         text NOT NULL CHECK (base_url LIKE 'https://%'),  -- must be in ISSUE_TRACKER_ALLOWED_BASE_URLS
    account_id       text NOT NULL,
    account_login    text NOT NULL,
    token_ciphertext bytea NOT NULL,       -- AES-256-GCM(token), AAD-bound
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_verified_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, kind, base_url)
);

-- +goose Down
DROP TABLE issue_tracker_connections;
