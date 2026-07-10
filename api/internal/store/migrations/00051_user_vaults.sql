-- +goose Up
-- Per-user vault (PRD #32): the wrapped Data Encryption Key that seals a user's
-- personal secrets. wrapped_dek = secretbox(KEK, DEK) where KEK = Argon2id(login
-- password, kek_salt). The plaintext DEK is NEVER stored — it is reconstructed in
-- API memory at unlock and discarded on lock/restart. kek_salt is a fresh random
-- 16-byte salt, independent of the users.password_hash Argon2 salt: same salt +
-- params would make this row's KEK equal to the stored password hash, i.e. the DB
-- would contain the KEK. Distinct salt ⇒ the KEK never lands at rest anywhere.
CREATE TABLE user_vaults (
    user_id     UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    kek_salt    BYTEA NOT NULL,          -- 16 bytes, random, never reused
    wrapped_dek BYTEA NOT NULL,          -- secretbox(KEK, DEK)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Which key seals each user_secrets row. 'master' = the legacy UZI_SECRET_KEY box
-- (all rows before this PRD, and any written while the vault is unwired); 'dek' =
-- the per-user vault DEK. Existing rows default to 'master' and are lazily
-- rewrapped to 'dek' on the owner's first unlock. New saves are born 'dek'.
ALTER TABLE user_secrets ADD COLUMN sealed_with TEXT NOT NULL DEFAULT 'master'
    CHECK (sealed_with IN ('master', 'dek'));

-- +goose Down
ALTER TABLE user_secrets DROP COLUMN sealed_with;
DROP TABLE user_vaults;
