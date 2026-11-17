-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4399: durable first-import status for a repo. Enabling a repo records a
-- 'waiting' import for the poller to run (it no longer runs on the HTTP request).
-- A NULL state is a legacy repo (enabled before this migration or seeded): the
-- poller never claims it and the board reports no import status for it.
-- initial_import_generation bumps on each (re-)enable that owes an import, so a
-- stale claim or finish from an older generation matches zero rows.
-- repos is small; the CHECKs scan it once under the same short lock.
ALTER TABLE repos
    ADD COLUMN initial_import_state text,
    ADD COLUMN initial_import_generation bigint NOT NULL DEFAULT 0,
    ADD COLUMN initial_import_error_class text,
    ADD COLUMN initial_import_updated_at timestamptz,
    ADD CONSTRAINT repos_initial_import_state_check
        CHECK (initial_import_state IN ('waiting','running','succeeded','failed')),
    ADD CONSTRAINT repos_initial_import_generation_check
        CHECK (initial_import_generation >= 0),
    ADD CONSTRAINT repos_initial_import_error_class_check
        CHECK (initial_import_error_class IN ('timeout','auth','server_error','rate_limited','other')),
    ADD CONSTRAINT repos_initial_import_error_pairing_check
        CHECK ((initial_import_error_class IS NOT NULL) = (initial_import_state IS NOT DISTINCT FROM 'failed'));

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE repos
    DROP CONSTRAINT repos_initial_import_error_pairing_check,
    DROP CONSTRAINT repos_initial_import_error_class_check,
    DROP CONSTRAINT repos_initial_import_generation_check,
    DROP CONSTRAINT repos_initial_import_state_check,
    DROP COLUMN initial_import_updated_at,
    DROP COLUMN initial_import_error_class,
    DROP COLUMN initial_import_generation,
    DROP COLUMN initial_import_state;
