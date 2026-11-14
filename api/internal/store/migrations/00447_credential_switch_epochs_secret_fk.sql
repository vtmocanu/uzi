-- +goose Up
-- Issue #4201: the run_credential_switch_epochs.secret_id foreign key, apart from 00446 so that no
-- migration holds a lock on runs while it waits on user_secrets (see 00446). The table is new and
-- empty, so the constraint validates instantly; this locks only that table and user_secrets.
SET LOCAL lock_timeout = '5s';
ALTER TABLE run_credential_switch_epochs
    ADD CONSTRAINT run_credential_switch_epochs_secret_id_fkey
    FOREIGN KEY (secret_id) REFERENCES user_secrets(id) ON DELETE SET NULL;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE run_credential_switch_epochs DROP CONSTRAINT IF EXISTS run_credential_switch_epochs_secret_id_fkey;
