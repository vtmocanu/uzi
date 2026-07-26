-- +goose Up
-- The PRD a run moved, and whether the issue link still needs patching for it
-- (PRD #72 M4 + M5). Both columns land here, in ONE migration: the declared path
-- and the pending-patch marker are the same fact, and splitting them would give
-- M5 a second migration to renumber for no gain.
--
-- prd_done_path is a DECLARATION by the run's lead, forwarded verbatim by an
-- untrusted worker, not something derived from the diff. It is gated on
-- runs.kind = 'issue' and validated by api/internal/prdpath on the one write path
-- (workersvc.clampWirePRDDonePath), which drops a bad value rather than failing
-- the terminal report.
--
-- No CHECK constraint on prd_done_path, deliberately. A file path is not a closed
-- domain the way stop_kind (00082) is, so a CHECK could only restate the grammar
-- in a second, drifting place. The shape is enforced in Go, once.
ALTER TABLE runs ADD COLUMN prd_done_path        text;
ALTER TABLE runs ADD COLUMN prd_patch_settled_at timestamptz;

-- The marker is named for the EDGE BEING CONSUMED, not for a patch happening:
-- three of its four settle reasons involve no forge write at all. Following
-- close_synced_at (00081), which means the same thing. The four reasons:
--   1. the MR merged and the description was rewritten;
--   2. the MR merged but the description no longer contained the old path;
--   3. the MR was closed without merging;
--   4. the run was superseded.
-- A forge error settles nothing and leaves the marker for the next tick.
COMMENT ON COLUMN runs.prd_patch_settled_at IS
    'When the PRD-link patch edge was consumed (patched, no-match, MR closed unmerged, or superseded). NULL with prd_done_path set = still pending.';

-- The M5 watcher's working set. Partial so the index holds only rows that can
-- still fire, copying idx_recommendation_filed_issues_close_pending (00081) for
-- the same reason: the poller runs this candidate query per repo per tick.
CREATE INDEX idx_runs_prd_patch_pending ON runs (repo_id)
    WHERE prd_done_path IS NOT NULL AND prd_patch_settled_at IS NULL;

-- +goose Down
-- Downs are not run in this deployment (the boot runner only migrates up); kept
-- so a local reset works.
DROP INDEX IF EXISTS idx_runs_prd_patch_pending;
ALTER TABLE runs DROP COLUMN IF EXISTS prd_patch_settled_at;
ALTER TABLE runs DROP COLUMN IF EXISTS prd_done_path;
