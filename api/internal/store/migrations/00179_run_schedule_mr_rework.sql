-- +goose Up

-- runs.mr_rework_enabled and run_schedules.mr_rework_enabled: the per-run and
-- per-schedule override layers for the MR review watcher (PRD #841 M1, Decision D1).
-- Both mirror the existing users.mr_rework_enabled (00165) shape rather than
-- wait_on_limit's NOT NULL DEFAULT: each column is NULLABLE with no default, tri-state
-- (NULL = inherit, true/false = explicit override). Eligibility resolves LIVE at read
-- time in ListMRReworkCandidates via COALESCE(run, owner) IS NOT FALSE, so there is no
-- owner-default snapshot at creation (D1 live-inherit) — a run/schedule left NULL simply
-- follows the owner's default, exactly today's behaviour.
--
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the next
-- free number above the live head if it drifts, per the CLAUDE.md convention.
ALTER TABLE runs ADD COLUMN mr_rework_enabled boolean;
ALTER TABLE run_schedules ADD COLUMN mr_rework_enabled boolean;

-- +goose Down
ALTER TABLE runs DROP COLUMN mr_rework_enabled;
ALTER TABLE run_schedules DROP COLUMN mr_rework_enabled;
