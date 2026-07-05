-- +goose Up

-- Board–run lifecycle integration (PRD #12 M1): the run row carries the facts the
-- server-driven column automation needs.
--
--   origin_column      the issue's column at run creation, snapshotted so a
--                      failed/cancelled run can be restored to where it started
--                      (NOT hardcoded Open). "" means the run started in the
--                      implicit Open column; NULL means unknown (pre-migration
--                      rows) — the two are deliberately distinct, and a NULL
--                      origin never restores (never strip a human's label on a
--                      guess).
--   board_column       the column the automation last SUCCESSFULLY applied for
--                      this run (NULL until the first successful move). The
--                      manual-drag guard compares the issue's current column
--                      against COALESCE(board_column, origin_column): anything
--                      else means a human placed the card, so the move is skipped.
--   move_pending_since the same-transaction marker that a column move is owed.
--                      Stamped in the SAME statement as the status write for the
--                      four statuses the automation reacts to (queued, completed,
--                      failed, cancelled), which closes the crash window between
--                      persisting the status and performing the forge move.
--                      Cleared by a successful move or a detected manual drag;
--                      retried by the reconcile loop within a 30-minute window.
ALTER TABLE runs
    ADD COLUMN origin_column      text,
    ADD COLUMN board_column       text,
    ADD COLUMN move_pending_since timestamptz;

-- The "latest run per issue" query (PRD #12 M2 — a DISTINCT ON (issue_iid)
-- ORDER BY created_at DESC, not a LATERAL join; see the M2 sqlc-nullability
-- decision) and the per-issue run history both scan runs by (repo, issue)
-- newest-first; only idx_runs_repo exists today.
CREATE INDEX idx_runs_issue_history ON runs (repo_id, issue_iid, created_at DESC);

-- Reconcile-loop scan: pending moves, oldest first.
CREATE INDEX idx_runs_move_pending ON runs (move_pending_since) WHERE move_pending_since IS NOT NULL;

-- +goose Down
DROP INDEX idx_runs_move_pending;
DROP INDEX idx_runs_issue_history;
ALTER TABLE runs
    DROP COLUMN move_pending_since,
    DROP COLUMN board_column,
    DROP COLUMN origin_column;
