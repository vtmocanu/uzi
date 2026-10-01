-- +goose Up

-- Independent Codex effort. Keep explicit shared preferences verbatim during the
-- split; NULL/blank preferences inherit the medium product default at claim time.
-- Validation stays at write surfaces, like the existing default_effort column.
ALTER TABLE users ADD COLUMN default_codex_effort text;
UPDATE users SET default_codex_effort = default_effort
WHERE default_effort IS NOT NULL;

-- +goose Down

-- The original shared/Claude preference is retained. An independently edited
-- Codex preference is lost on schema rollback.
ALTER TABLE users DROP COLUMN default_codex_effort;
