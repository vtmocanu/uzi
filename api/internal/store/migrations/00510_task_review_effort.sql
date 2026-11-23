-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #1570: the owner's per-harness reasoning effort for the optional handoff review pass (`uzi
-- handoff --review`). NULL = inherit the owner's ordinary worker effort for that harness. Stored
-- values are the five levels or `runtime` (no level sent). Nullable text with no CHECK, index or
-- backfill (the 00285 and 00509 convention): the PUT validates and trims, the claim re-resolves,
-- and old binaries ignore the columns.
ALTER TABLE users
    ADD COLUMN task_review_effort text,
    ADD COLUMN codex_task_review_effort text;

-- +goose Down
-- Down discards the stored review efforts; reviews inherit the worker effort again.
SET LOCAL lock_timeout = '5s';
ALTER TABLE users
    DROP COLUMN IF EXISTS codex_task_review_effort,
    DROP COLUMN IF EXISTS task_review_effort;
