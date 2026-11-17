-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4320: validates users_claude_provider_fkey, added NOT VALID in 00469 (users.claude_provider_id
-- is NULL on every existing row, so the scan is trivial; it is its own file so the validation never
-- shares a transaction with another table's lock).
ALTER TABLE users VALIDATE CONSTRAINT users_claude_provider_fkey;

-- +goose Down
-- Validation has no data or schema to undo; 00469 owns the drop.
SELECT 1;
