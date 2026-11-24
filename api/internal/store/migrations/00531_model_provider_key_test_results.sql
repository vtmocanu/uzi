-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4324 M1: Test evidence belongs to the tested credential, not the provider. One row per
-- (provider, user) records the exact tuple a probe ran against: the provider revision, the key row
-- and its enablement revision. A reader shows it only while that tuple is still current, and every
-- foreign key cascades, so deleting the provider, the user, the revision or the key removes it.
-- (user_id, key_secret_id) is backed by user_secrets_user_id_id_key (00077).
CREATE TABLE model_provider_key_test_results (
    provider_id        uuid        NOT NULL,
    user_id            uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    revision           integer     NOT NULL,
    key_secret_id      uuid        NOT NULL,
    key_enablement_rev bigint      NOT NULL,
    auth_result        text        NOT NULL,
    tool_result        text,
    probed_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, user_id),
    FOREIGN KEY (provider_id, revision) REFERENCES model_provider_revisions (provider_id, revision) ON DELETE CASCADE,
    FOREIGN KEY (user_id, key_secret_id) REFERENCES user_secrets (user_id, id) ON DELETE CASCADE
);
-- Serves the cascade from a deleted or rotated key.
CREATE INDEX model_provider_key_test_results_key_idx ON model_provider_key_test_results (user_id, key_secret_id);

-- Discard the provider-wide cache: it is not evidence about any one user's key, so existing
-- personal definitions start as "not tested". The legacy table keeps its shape and its
-- (provider_id, revision) conflict target for older API images.
DELETE FROM model_provider_probe_results;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE model_provider_key_test_results;
