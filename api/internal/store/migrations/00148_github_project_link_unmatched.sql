-- +goose Up

-- PRD #576 M3: persist the unmatched-columns advisory on the link row. On Adopt
-- each uzi board column is exact-matched to a Status option by name; columns with
-- no matching option are skipped (their items never sync). Before M3 this set was
-- only logged and returned as a note — nothing stored it, so the sync panel could
-- not surface it. This column records the skipped column labels so ProjectSyncStatus
-- (a pure store read, no forge call — Decision D5) can return them to the panel.
--
-- NOT NULL DEFAULT '{}' so the empty set means "every column matched" — existing
-- rows need NO backfill — and the write path never has to distinguish NULL from
-- empty (a nil Go []string bound to this column is COALESCE'd to '{}' in the upsert).
ALTER TABLE github_project_links
    ADD COLUMN unmatched_columns text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE github_project_links DROP COLUMN unmatched_columns;
