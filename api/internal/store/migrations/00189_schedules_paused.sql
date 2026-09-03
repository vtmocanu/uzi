-- +goose Up

-- User-level "pause all schedules" kill switch (PRD #1093): two additive columns on
-- users. schedules_paused is the switch; schedules_paused_until is the optional
-- auto-resume instant. "Paused" means schedules_paused AND (schedules_paused_until IS
-- NULL OR schedules_paused_until > now()). A NULL until means "until I resume"; an
-- expired until reads as not paused everywhere (expiry is computed on read and at fire
-- time, in Go, so no background job or cron clears it). Additive, no backfill: every
-- existing user defaults to not paused.
--
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the next
-- free number above the live head if it drifts, per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN schedules_paused boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN schedules_paused_until timestamptz;

-- +goose Down
ALTER TABLE users DROP COLUMN schedules_paused_until;
ALTER TABLE users DROP COLUMN schedules_paused;
