-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #1570: the owner's per-harness model for the optional handoff review pass (`uzi handoff
-- --review`). NULL = the built-in review model (Claude: the SDK default; Codex: gpt-6-sol).
-- Nullable text with no CHECK, index or backfill (the 00285 convention): the PUT validates and
-- trims, the claim re-validates, and old binaries ignore the columns.
ALTER TABLE users
    ADD COLUMN task_review_model text,
    ADD COLUMN codex_task_review_model text;

-- +goose Down
-- Down discards the stored review models; reviews fall back to the built-in model.
SET LOCAL lock_timeout = '5s';
ALTER TABLE users
    DROP COLUMN IF EXISTS codex_task_review_model,
    DROP COLUMN IF EXISTS task_review_model;
