-- +goose NO TRANSACTION
-- +goose Up

-- PRD #3989 M3: the credential promoter's worklist (ListCredentialDisabledUsers,
-- ListCredentialDisabledRuns) now also reads the credentials_reset hold. The 00258 partial index
-- covers only credential_disabled, so this one covers both reasons. The 00258 index stays.
-- Build concurrently so the runs table stays writable.
-- A failed concurrent build can leave an invalid index; drop this owned index
-- before retrying rather than silently accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS runs_credential_hold_promoter_idx;
CREATE INDEX CONCURRENTLY runs_credential_hold_promoter_idx
    ON runs (user_id, status_since, id)
    WHERE status = 'paused' AND hold_reason IN ('credential_disabled', 'credentials_reset');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_credential_hold_promoter_idx;
