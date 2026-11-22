-- +goose Up
ALTER TABLE users
    ADD COLUMN planning_codex_model text,
    ADD COLUMN planning_codex_effort text;

-- +goose Down
ALTER TABLE users
    DROP COLUMN planning_codex_model,
    DROP COLUMN planning_codex_effort;
