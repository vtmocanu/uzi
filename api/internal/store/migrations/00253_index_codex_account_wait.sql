-- +goose NO TRANSACTION
-- +goose Up

-- PRD #1590 M2 (D2): keep the growing runs table writable while building the
-- two partial indexes behind the Codex account hold. The number is a draft,
-- renumbered above the live head at landing (task migration:renumber).
-- idx_runs_codex_sub_queued bounds the park_codex_account_unavailable sweeper
-- page: ParkQueuedCodexAccountUnavailablePage walks queued Codex subscription
-- runs by id (a keyset cursor), so a tick examines at most one page of them.
-- idx_runs_codex_account_wait covers the held set that the account-driven
-- promoter (M3) scans.
-- A failed concurrent build can leave an invalid index. Drop that exact owned
-- index before retrying rather than silently accepting it with IF NOT EXISTS.
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_codex_sub_queued;
CREATE INDEX CONCURRENTLY idx_runs_codex_sub_queued
    ON runs (id)
    WHERE status = 'queued' AND harness = 'codex' AND codex_auth_mode = 'subscription';

DROP INDEX CONCURRENTLY IF EXISTS idx_runs_codex_account_wait;
CREATE INDEX CONCURRENTLY idx_runs_codex_account_wait
    ON runs (id)
    WHERE status = 'recovery_wait' AND recovery_wait_cause = 'codex_account_unavailable';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_codex_account_wait;
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_codex_sub_queued;
