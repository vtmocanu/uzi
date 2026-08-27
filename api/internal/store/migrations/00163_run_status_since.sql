-- +goose Up
-- runs.status_since: WHEN this run entered its current status. runs.updated_at conflates
-- "row changed at all" with "gate episode began", so an incidental writer (a move-pending
-- flag, an MR-state stamp) that touches the row without changing runs.status was resetting
-- the approval/queued health clocks. status_since is stamped ONLY by statements that assign
-- runs.status, so the health pass reads a clock that advances on real status transitions and
-- nothing else.
--
-- The three Up statements are ordered for correctness: add the column nullable with no
-- default (fast, no table rewrite); backfill EVERY row with no WHERE clause; then install
-- NOT NULL and DEFAULT now(). NOT NULL comes AFTER the backfill so a missed row would fail
-- the migration rather than smuggle a NULL past it, and DEFAULT now() auto-stamps every
-- future INSERT.
--
-- The backfill uses updated_at because for the vast majority of rows updated_at IS the last
-- status change; for a row that an incidental writer had already corrupted, seeding from
-- updated_at reproduces today's behaviour for that one row rather than worsening it, and the
-- next real transition re-stamps status_since correctly.
ALTER TABLE runs ADD COLUMN status_since timestamptz;
UPDATE runs SET status_since = updated_at;
ALTER TABLE runs ALTER COLUMN status_since SET NOT NULL, ALTER COLUMN status_since SET DEFAULT now();

-- +goose Down
ALTER TABLE runs DROP COLUMN status_since;
