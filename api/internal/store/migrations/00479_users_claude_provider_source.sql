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
-- Validated in 00480.

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently clear a user's provider source (the 00198 convention): dropping the
-- column would turn every selected source into a vendor fallback, which D12 forbids. The table is
-- locked BEFORE the guard so a source selected concurrently cannot land between the check and the
-- column drop. Take the final ALTER lock up front so a reader that later writes cannot
-- deadlock against a lock upgrade after the guard.
LOCK TABLE users IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users WHERE claude_provider_id IS NOT NULL) THEN
        RAISE EXCEPTION 'refusing to roll back 00479: user(s) still have a Claude provider source selected; clear it explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE users DROP CONSTRAINT users_claude_provider_fkey;
ALTER TABLE users DROP COLUMN claude_provider_id;
