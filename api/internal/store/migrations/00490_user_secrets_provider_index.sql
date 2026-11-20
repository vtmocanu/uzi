-- +goose NO TRANSACTION
-- +goose Up

-- One gateway key per (owner, provider). Even though existing provider_id values
-- are NULL, building this partial index scans user_secrets. Run it outside the
-- column-add transaction so the scan holds no ACCESS EXCLUSIVE lock. Drop our
-- owned index first: retrying an interrupted concurrent build must replace an
-- invalid index rather than silently keeping it with CREATE IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS user_secrets_one_provider_key;
CREATE UNIQUE INDEX CONCURRENTLY user_secrets_one_provider_key
    ON user_secrets (user_id, provider_id)
    WHERE provider_id IS NOT NULL;

-- +goose Down
-- Retain uniqueness until the guarded provider_id column Down succeeds. That
-- column drop removes this dependent index automatically. Dropping it here
-- would leave keys unprotected if the earlier Down then refuses live keys.
SELECT 1;
