-- +goose NO TRANSACTION
-- +goose Up

-- PRD #4324 M1 indexes, built outside any transaction so the scans hold no ACCESS EXCLUSIVE lock.
-- Each is dropped first: retrying an interrupted concurrent build must replace an invalid index
-- rather than silently keep it. The foreign keys of 00527 and 00528 reference
-- model_providers_scope_owner_id_key, so 00527 refuses an invalid one.
--
-- model_providers_owner_id_key and model_providers_owner_name_lower_key (00476) are NOT touched:
-- the first is the parent key of users_codex_provider_fkey (00517).
DROP INDEX CONCURRENTLY IF EXISTS model_providers_scope_owner_id_key;
CREATE UNIQUE INDEX CONCURRENTLY model_providers_scope_owner_id_key
    ON model_providers (scope_owner_id, id);

-- Shared names are unique case-insensitively across the whole shared catalogue.
DROP INDEX CONCURRENTLY IF EXISTS model_providers_shared_name_lower_key;
CREATE UNIQUE INDEX CONCURRENTLY model_providers_shared_name_lower_key
    ON model_providers (lower(name))
    WHERE credential_mode IS NOT NULL;

-- Serves the lookup of users who selected a given Claude definition (shared delete guard and counts).
DROP INDEX CONCURRENTLY IF EXISTS users_claude_provider_id_idx;
CREATE INDEX CONCURRENTLY users_claude_provider_id_idx
    ON users (claude_provider_id)
    WHERE claude_provider_id IS NOT NULL;

-- +goose Down
-- users_claude_provider_id_idx is on a column no earlier migration drops, so it goes here. The other
-- two indexes depend on columns 00524's Down drops (and, for the scope key, on foreign keys the
-- later Downs drop first); dropping them here would leave those references unprotected if an earlier
-- Down then refuses. Same reasoning as the 00490 Down.
DROP INDEX CONCURRENTLY IF EXISTS users_claude_provider_id_idx;
