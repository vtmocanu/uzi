-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4416 M1: per-repo (and, from M4, per-user) environment variables for runs.
-- One row per VERSION of a variable. var_id is minted when a variable is created and
-- never reused (a delete then re-create is a different variable); rev counts up per
-- var_id. A row is live when neither retired_at nor pending_delete_at is set; a
-- disabled row (disabled_at) is still live. repo_id NULL is the user-level scope.
--
-- Every value, plain or secret, is sealed through the vault seam (sealed_with is the
-- same discriminator user_secrets carries, minus the nil-AAD legacy 'master': a new table
-- has no legacy rows, and a nil-AAD seal would not bind the value to its name), bound by an AAD that names the scope, repo,
-- secret flag and variable name (internal/runenv AADKind). value_bytes is the plaintext
-- length, kept so the per-scope and retired-history byte limits need no decryption.
CREATE TABLE run_env_vars (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    var_id            uuid NOT NULL,
    user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id           uuid REFERENCES repos(id) ON DELETE CASCADE,
    name              text NOT NULL CHECK (name ~ '^[A-Z_][A-Z0-9_]{0,127}$'),
    secret            boolean NOT NULL,
    description       text NOT NULL DEFAULT '' CHECK (octet_length(description) <= 1024),
    ciphertext        bytea NOT NULL,
    sealed_with       text NOT NULL CHECK (sealed_with IN ('master_aad', 'dek')),
    value_bytes       integer NOT NULL CHECK (value_bytes BETWEEN 0 AND 65536),
    rev               integer NOT NULL CHECK (rev >= 1),
    retired_at        timestamptz,
    disabled_at       timestamptz,
    pending_delete_at timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- Only a secret version is ever retired or kept pending deletion (for masking).
    CONSTRAINT run_env_vars_retained_secret_check
        CHECK ((retired_at IS NULL AND pending_delete_at IS NULL) OR secret),
    CONSTRAINT run_env_vars_var_rev_key UNIQUE (var_id, rev)
);

-- A name is unique among the LIVE rows of its scope.
CREATE UNIQUE INDEX run_env_vars_live_repo_name_key
    ON run_env_vars (user_id, repo_id, name)
    WHERE repo_id IS NOT NULL AND retired_at IS NULL AND pending_delete_at IS NULL;
CREATE UNIQUE INDEX run_env_vars_live_user_name_key
    ON run_env_vars (user_id, name)
    WHERE repo_id IS NULL AND retired_at IS NULL AND pending_delete_at IS NULL;
-- A variable has at most one live version.
CREATE UNIQUE INDEX run_env_vars_live_var_key
    ON run_env_vars (var_id)
    WHERE retired_at IS NULL AND pending_delete_at IS NULL;
-- Cascade and per-scope reads.
CREATE INDEX run_env_vars_repo_idx ON run_env_vars (repo_id) WHERE repo_id IS NOT NULL;
CREATE INDEX run_env_vars_user_idx ON run_env_vars (user_id);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE run_env_vars;
