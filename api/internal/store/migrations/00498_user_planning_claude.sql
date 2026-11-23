-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #3252: the owner's Claude planning model and effort, used only while a lead-lane Claude run
-- plans. NULL = "Same as the run" (the plan phase uses the same pair as implementation). Nullable
-- text with no CHECK, index or backfill (the 00285 convention): the PUT validates and trims, and old
-- binaries ignore the columns.
ALTER TABLE users
    ADD COLUMN planning_claude_model text,
    ADD COLUMN planning_claude_effort text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE users
    DROP COLUMN IF EXISTS planning_claude_effort,
    DROP COLUMN IF EXISTS planning_claude_model;
