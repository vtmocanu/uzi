-- +goose Up
-- Separate transaction from 00349: release its ACCESS EXCLUSIVE lock before scanning.
SET LOCAL lock_timeout = '2s';
ALTER TABLE runs VALIDATE CONSTRAINT runs_source_publication_intent_check;

-- +goose Down
-- Keeping the validated constraint preserves the same write contract on rollback.
SELECT 1;
