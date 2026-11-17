-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4320 M1: user-defined model providers (Anthropic-compatible gateways). These are new
-- tables only; they lock no busy table. A revision is immutable (an edit inserts a row; there is
-- no UPDATE statement on model_provider_revisions). UNIQUE (owner_user_id, id) is the parent key of
-- the composite foreign keys that bind a provider key (user_secrets) and a selected source
-- (users.claude_provider_id) to the SAME owner (migrations that follow).
CREATE TABLE model_providers (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name             text        NOT NULL,
    engine           text        NOT NULL CHECK (engine IN ('claude')),
    current_revision integer     NOT NULL,
    retired_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT model_providers_owner_id_key UNIQUE (owner_user_id, id)
);
-- Names are unique per owner case-insensitively ("Corp" and "corp" collide).
CREATE UNIQUE INDEX model_providers_owner_name_lower_key ON model_providers (owner_user_id, lower(name));

CREATE TABLE model_provider_revisions (
    provider_id uuid        NOT NULL REFERENCES model_providers(id) ON DELETE CASCADE,
    revision    integer     NOT NULL,
    protocol    text        NOT NULL CHECK (protocol IN ('anthropic_messages')),
    base_url    text        NOT NULL,
    auth_style  text        NOT NULL CHECK (auth_style IN ('bearer', 'x_api_key')),
    model_map   jsonb       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, revision)
);

-- The latest probe result per revision (the probe lands in a later milestone of the PRD).
CREATE TABLE model_provider_probe_results (
    provider_id uuid        NOT NULL,
    revision    integer     NOT NULL,
    auth_result text        NOT NULL,
    tool_result text,
    probed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, revision),
    FOREIGN KEY (provider_id, revision) REFERENCES model_provider_revisions(provider_id, revision) ON DELETE CASCADE
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy providers, revisions and their keys (the 00198 convention).
-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM model_providers;
    IF n > 0 THEN
        RAISE EXCEPTION 'refusing to roll back 00466: % model provider row(s) still exist; delete them explicitly before downgrading', n;
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE model_provider_probe_results;
DROP TABLE model_provider_revisions;
DROP TABLE model_providers;
