-- +goose Up

-- PRD #1733: a provider-transient recovery park is typed, counted and bounded. The worker reports
-- a park whose cause is a provider error as recovery_cause 'provider_outage' (already in
-- runs_recovery_wait_cause_check since 00232, so no CHECK change), and the server tracks an
-- unbroken STREAK of those parks in two columns on runs:
--
--   * provider_park_count            - parks in the open streak (the park that opened it is 1).
--                                      NOT NULL DEFAULT 0, so every existing row has no streak.
--   * provider_park_streak_started_at - when the open streak began; NULL when none is open.
--
-- A streak is opened by the first provider park, restarted when the parking flight reports
-- productive turns, and cleared by an owner action (resume of a provider hold, an early promote,
-- a credential switch). Past RUN_PROVIDER_PARK_MAX_DURATION the next provider park becomes a
-- paused hold with hold_reason 'provider_unavailable' (hold_reason is free text, so no CHECK
-- change). Distinct from recovery_wait_count, which only shapes the backoff for every cause.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
ALTER TABLE runs
    ADD COLUMN provider_park_count integer NOT NULL DEFAULT 0,
    ADD COLUMN provider_park_streak_started_at timestamptz NULL;

-- +goose Down
ALTER TABLE runs
    DROP COLUMN provider_park_streak_started_at,
    DROP COLUMN provider_park_count;
