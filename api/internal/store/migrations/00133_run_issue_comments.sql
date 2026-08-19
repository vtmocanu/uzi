-- +goose Up
-- PRD #381 D7: structured snapshot of the issue's human comments at run creation.
-- Nullable: every existing run and every non-issue kind reads NULL.
ALTER TABLE runs ADD COLUMN issue_comments jsonb;

-- +goose Down
ALTER TABLE runs DROP COLUMN issue_comments;
