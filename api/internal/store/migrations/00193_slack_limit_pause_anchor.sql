-- +goose Up
-- limit_paused_at: additive column on slack_run_messages holding WHEN the pending usage-limit
-- park began (PRD #1116). NULL when no park is awaiting its resume line. It is both the
-- dedupe marker and the pause start (copied from runs.status_since at the park), consumed
-- (set back to NULL) by a compare-and-swap on the next transition. Additive, no backfill.
-- Its own column, not reusing an existing anchor field, for the same reason gate_generation
-- and milestones_notified_completed are their own columns: distinct dedupe fact.
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the next free
-- number above the live head if it drifts, per the CLAUDE.md convention.
ALTER TABLE slack_run_messages ADD COLUMN limit_paused_at timestamptz;

-- +goose Down
ALTER TABLE slack_run_messages DROP COLUMN limit_paused_at;
