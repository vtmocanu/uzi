-- +goose Up

-- PRD #890 M1: the dedup key for the "your vault is locked" Slack notice.
--
-- Episode / dedup model: NULL means "not yet notified for the current lock-episode".
-- The reconciler atomically claims a user by setting lock_notified_at = now() (only the
-- row that comes back is DM'd), so N api pods booting together send ONE DM per user, not
-- N. The next successful vault unlock clears it back to NULL, opening a fresh episode — so
-- a later deploy that locks the user again notifies afresh, while a user who never unlocks
-- stays marked and is never re-notified. Nullable, default NULL: every existing row is
-- unmarked, so the first deploy after this ships notifies every eligible user once.
ALTER TABLE user_vaults ADD COLUMN lock_notified_at timestamptz;

-- +goose Down
ALTER TABLE user_vaults DROP COLUMN lock_notified_at;
