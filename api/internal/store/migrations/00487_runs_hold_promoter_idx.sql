-- +goose NO TRANSACTION
-- +goose Up

-- PRD #4320 module 5: the credential promoter's worklist (ListCredentialDisabledUsers,
-- ListCredentialDisabledRuns) now also reads the provider_destination_not_allowed hold, the one
-- provider refusal the promoter re-queues once the operator allowlists the URL again. The 00414
-- partial index covers two reasons, so a three-reason predicate would not use it. This one covers
-- all three, and serves the old two-reason predicate too (IN (a, b) implies IN (a, b, c)), so the
-- 00414 index is dropped after this one exists, never leaving the worklist without an index.
-- Build concurrently so the runs table stays writable. A failed concurrent build can leave an
-- invalid index; drop this owned index before retrying rather than accepting it with IF NOT EXISTS.
-- The migration number is a draft: it is renumbered above the live head at landing.
DROP INDEX CONCURRENTLY IF EXISTS runs_hold_promoter_idx;
CREATE INDEX CONCURRENTLY runs_hold_promoter_idx
    ON runs (user_id, status_since, id)
    WHERE status = 'paused' AND hold_reason IN ('credential_disabled', 'credentials_reset', 'provider_destination_not_allowed');
DROP INDEX CONCURRENTLY IF EXISTS runs_credential_hold_promoter_idx;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_credential_hold_promoter_idx;
CREATE INDEX CONCURRENTLY runs_credential_hold_promoter_idx
    ON runs (user_id, status_since, id)
    WHERE status = 'paused' AND hold_reason IN ('credential_disabled', 'credentials_reset');
DROP INDEX CONCURRENTLY IF EXISTS runs_hold_promoter_idx;
