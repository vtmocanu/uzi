-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4320 M1: the per-engine provider source setting on users (model_provider selection;
-- the setter ships in a later milestone). NULL keeps the whole existing Claude ladder. The
-- composite foreign key pins the selected provider to the same owner, and ON DELETE RESTRICT
-- means deleting a selected provider is refused (the handler maps it to provider_in_use_as_source);
-- a source is never silently nulled (contrast 00078/00079).
ALTER TABLE users ADD COLUMN claude_provider_id uuid;
ALTER TABLE users ADD CONSTRAINT users_claude_provider_fkey
    FOREIGN KEY (id, claude_provider_id) REFERENCES model_providers (owner_user_id, id) ON DELETE RESTRICT NOT VALID;
-- Validated in 00470.

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE users DROP CONSTRAINT users_claude_provider_fkey;
ALTER TABLE users DROP COLUMN claude_provider_id;
