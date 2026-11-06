-- +goose Up
-- First statement: bound any lock wait below. The table is new and empty, so the DDL is
-- instant; a wait past the bound fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';
-- Issue #3251: the Codex catalog's default reasoning effort, as last reported by a worker, per
-- user. context_id is an opaque SHA-256 hash of (user, secret id, auth mode, material revision,
-- account key); it carries no credential material and appears in no read DTO. Retention (at most
-- 32 rows per user, none older than 30 days) is enforced by the single writer under a per-user
-- advisory lock, not by this schema.
CREATE TABLE codex_catalog_defaults (
  user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  context_id      text NOT NULL CONSTRAINT codex_catalog_defaults_context_check CHECK (context_id ~ '^[0-9a-f]{64}$'),
  model           text NOT NULL CONSTRAINT codex_catalog_defaults_model_check CHECK (octet_length(model) BETWEEN 1 AND 100),
  runtime_version text NOT NULL CONSTRAINT codex_catalog_defaults_version_check CHECK (octet_length(runtime_version) BETWEEN 1 AND 32),
  default_effort  text NOT NULL CONSTRAINT codex_catalog_defaults_effort_check
                  CHECK (default_effort IN ('none','minimal','low','medium','high','xhigh','max','ultra')),
  reported_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT codex_catalog_defaults_pkey PRIMARY KEY (user_id, context_id, model, runtime_version)
);
CREATE INDEX codex_catalog_defaults_user_recent_idx ON codex_catalog_defaults (user_id, reported_at DESC);

-- +goose Down
DROP TABLE codex_catalog_defaults;
