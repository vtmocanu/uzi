-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #4507: episode-keyed marker for the health detector's quiet Codex provenance hold. It
-- holds the health_since of the episode in which the detector observed a quiet named Codex
-- provenance hold while the draining/maintenance owner hold was in force. It is valid only while
-- it equals the row's current health_since, so a new episode (new health_since) invalidates it
-- without any clearing write. Nullable, no default, no backfill: a legacy row has no marker.
ALTER TABLE runs ADD COLUMN health_owner_hold_since timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs DROP COLUMN IF EXISTS health_owner_hold_since;
