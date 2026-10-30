-- +goose Up
ALTER TABLE users
    ADD COLUMN vault_auto_reunlock boolean NOT NULL DEFAULT true,
    ADD COLUMN vault_slot_generation bigint NOT NULL DEFAULT 0 CHECK (vault_slot_generation >= 0);

CREATE TABLE vault_browser_slots (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    browser_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    token_version integer NOT NULL,
    vault_epoch bigint NOT NULL CHECK (vault_epoch >= 0),
    generation bigint NOT NULL CHECK (generation >= 0),
    UNIQUE (user_id, browser_id)
);
CREATE INDEX vault_browser_slots_expiry_idx ON vault_browser_slots (expires_at, id);

-- Storage callers retain their existing query contracts. Revocation runs within
-- their statement, including direct password changes, not only HTTP callers.
-- +goose StatementBegin
CREATE FUNCTION fence_vault_browser_slots() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.token_version IS DISTINCT FROM OLD.token_version
       OR NEW.password_hash IS DISTINCT FROM OLD.password_hash
       OR (OLD.is_active AND NOT NEW.is_active)
       OR NEW.vault_mode IS DISTINCT FROM OLD.vault_mode
       OR NEW.vault_epoch IS DISTINCT FROM OLD.vault_epoch
       OR (OLD.vault_auto_reunlock AND NOT NEW.vault_auto_reunlock) THEN
        NEW.vault_slot_generation := OLD.vault_slot_generation + 1;
    END IF;
    IF NEW.vault_slot_generation < OLD.vault_slot_generation THEN
        RAISE EXCEPTION 'vault slot generation cannot decrease';
    END IF;
    IF NEW.vault_slot_generation IS DISTINCT FROM OLD.vault_slot_generation THEN
        DELETE FROM vault_browser_slots WHERE user_id = OLD.id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER users_vault_browser_fence BEFORE UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION fence_vault_browser_slots();

-- +goose Down
DROP TRIGGER users_vault_browser_fence ON users;
DROP FUNCTION fence_vault_browser_slots();
DROP TABLE vault_browser_slots;
ALTER TABLE users DROP COLUMN vault_auto_reunlock, DROP COLUMN vault_slot_generation;
