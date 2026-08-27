-- +goose Up

-- Default scheduled jobs (PRD #589), idempotent-enable half. A default schedule is
-- identified by (user_id, repo_id, catalog_slug); enabling the same catalog job twice on
-- the same repo for the same owner must be a no-op that returns the existing row, not a
-- second duplicate. This partial unique index enforces that at the DB level so the
-- CreateDefaultSchedule INSERT can use ON CONFLICT ... DO NOTHING. It covers ONLY
-- origin='default' rows, so it never constrains user-authored schedules (which carry a
-- NULL catalog_slug and may freely repeat a repo).
CREATE UNIQUE INDEX uq_run_schedules_default_per_repo
    ON run_schedules (user_id, repo_id, catalog_slug)
    WHERE origin = 'default';

-- +goose Down

DROP INDEX IF EXISTS uq_run_schedules_default_per_repo;
