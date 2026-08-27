-- +goose Up

-- A user's connection to a forge via their own bot PAT. The token is stored
-- encrypted (AES-256-GCM); a DB dump alone cannot recover it. One connection
-- per (user, forge_type, base_url).
CREATE TABLE forge_connections (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    forge_type        text NOT NULL CHECK (forge_type IN ('gitlab')),  -- 'forgejo' later
    base_url          text NOT NULL,       -- must be in FORGE_ALLOWED_BASE_URLS
    bot_username      text NOT NULL,
    bot_forge_user_id bigint NOT NULL,
    token_ciphertext  bytea NOT NULL,      -- AES-256-GCM(PAT)
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_verified_at  timestamptz,
    UNIQUE (user_id, forge_type, base_url)
);

-- Repos discovered via the bot's membership list. Rows are upserted with
-- enabled=false whenever the membership list is fetched, so enable/disable
-- always has a stable id to target. Keyed by the forge's numeric project id
-- (paths get renamed; ids do not).
CREATE TABLE repos (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id       uuid NOT NULL REFERENCES forge_connections ON DELETE CASCADE,
    forge_project_id    bigint NOT NULL,
    path_with_namespace text NOT NULL,
    web_url             text NOT NULL,
    default_branch      text,
    enabled             boolean NOT NULL DEFAULT false,
    UNIQUE (connection_id, forge_project_id)
);

-- Ordered kanban columns for a repo. Each column is a forge label; the implicit
-- Open (no column label) and Closed (issue state) columns are not stored.
CREATE TABLE board_columns (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id    uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
    label_name text NOT NULL,
    position   int NOT NULL,
    UNIQUE (repo_id, label_name)
);

-- Cache of forge issue truth. Never authoritative: the forge is the source of
-- truth and the sync engine reconciles this table against it. The description
-- is not stored — only the computed has_prd_link boolean.
CREATE TABLE issues (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id          uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
    forge_issue_iid  bigint NOT NULL,
    title            text NOT NULL,
    state            text NOT NULL,                    -- opened|closed
    labels           jsonb NOT NULL DEFAULT '[]',
    web_url          text NOT NULL,
    author           text,
    has_prd_link     boolean NOT NULL DEFAULT false,
    forge_updated_at timestamptz NOT NULL,
    synced_at        timestamptz NOT NULL,
    UNIQUE (repo_id, forge_issue_iid)
);

CREATE INDEX idx_repos_connection ON repos (connection_id);
CREATE INDEX idx_board_columns_repo ON board_columns (repo_id, position);
CREATE INDEX idx_issues_repo ON issues (repo_id);

-- +goose Down
DROP TABLE issues;
DROP TABLE board_columns;
DROP TABLE repos;
DROP TABLE forge_connections;
