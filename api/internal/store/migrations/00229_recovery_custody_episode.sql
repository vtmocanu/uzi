-- +goose Up

-- PRD #1349 M1 (D7/D10): per-owner custody-episode notify state plus an owner-list index.
-- Purely ADDITIVE — one brand-new table and one new index — so an N-1 worker/api reading the
-- pre-existing surface during a rolling release is unaffected (check-migration-additive scans
-- only destructive Up verbs on worker-facing tables; CREATE TABLE and CREATE INDEX are
-- neither). DRAFT number 00226 above the live head 00225; the lead renumbers at landing.

-- D10: the one-per-episode owner Slack DM dedup slot. A dedicated table (not a column on
-- users) keeps the change isolated to this milestone: the SPA `User` model and every
-- SELECT * FROM users stay byte-identical, so the sqlc regen touches only recovery.sql.go +
-- one new model. Presence of a row means "this owner has already been notified for the
-- CURRENT blocked-custody episode"; ClaimCustodyEpisodeNotice inserts it at-most-once (PK
-- conflict = already notified), ClearCustodyEpisodeNotice deletes it when the episode closes.
-- FK to users with ON DELETE CASCADE: a transient dedup flag, so it may cascade with the
-- owner (unlike a custody hold, which must never silently cascade — it protects a last copy).
CREATE TABLE custody_episode_notices (
    user_id     uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    notified_at timestamptz NOT NULL
);

-- D7: the owner hold-list read (ListCustodyHoldsForOwner) filters by user_id and orders by
-- created_at across ALL states, so the existing partial idx_recovery_custody_holds_owner_open
-- (WHERE state='open') does not cover it. This composite serves the ordered owner listing.
CREATE INDEX IF NOT EXISTS idx_recovery_custody_holds_owner
    ON recovery_custody_holds (user_id, created_at);

-- +goose Down

DROP INDEX IF EXISTS idx_recovery_custody_holds_owner;
DROP TABLE custody_episode_notices;
