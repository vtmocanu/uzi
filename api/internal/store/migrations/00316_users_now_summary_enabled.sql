-- +goose Up

-- users.now_summary_enabled: the per-user switch for the model-written "Now" line
-- on the run progress card (PRD #2603). Like users.mr_rework_enabled (00165) the
-- column is NULLABLE with no default and a NULL/absent value is READ AS ENABLED, so
-- an existing row is opted in and a user opts OUT by setting it explicitly to false.
-- The instance kill-switch lives in app_settings (now_summary_enabled); a summary is
-- written only when BOTH the instance switch is on AND the run owner has not opted
-- out here.
--
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the next
-- free number above the live head if it drifts, per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN now_summary_enabled boolean;

-- +goose Down
ALTER TABLE users DROP COLUMN now_summary_enabled;
