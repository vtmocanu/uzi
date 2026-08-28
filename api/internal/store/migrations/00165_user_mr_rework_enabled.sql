-- +goose Up

-- users.mr_rework_enabled: the per-user opt-in to the MR review watcher (PRD #700
-- M5, Decision 5). When on, a completed run's MR that gains new review comments on a
-- green pipeline is auto-reworked. Unlike the judge/autopilot opt-ins (00061/00037,
-- which are NOT NULL DEFAULT false, opt-IN), this feature ships ON: the column is
-- NULLABLE with no default, and a NULL/absent value is READ AS ENABLED. So an
-- existing row (NULL after this ALTER) is opted in, and a user opts OUT by setting
-- the flag explicitly to false. The global kill-switch lives in app_settings
-- (mr_rework_enabled); a rework fires only when BOTH the global toggle is on AND the
-- run owner has not opted out here.
--
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the next
-- free number above the live head if it drifts, per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN mr_rework_enabled boolean;

-- +goose Down
ALTER TABLE users DROP COLUMN mr_rework_enabled;
