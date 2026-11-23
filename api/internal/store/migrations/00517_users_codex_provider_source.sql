-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE users ADD COLUMN codex_provider_id uuid;
ALTER TABLE users ADD CONSTRAINT users_codex_provider_fkey
    FOREIGN KEY (id, codex_provider_id) REFERENCES model_providers (owner_user_id, id) ON DELETE RESTRICT NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE users IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users WHERE codex_provider_id IS NOT NULL) THEN
        RAISE EXCEPTION 'refusing to roll back 00517: Codex sources still selected';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE users DROP CONSTRAINT users_codex_provider_fkey;
ALTER TABLE users DROP COLUMN codex_provider_id;
