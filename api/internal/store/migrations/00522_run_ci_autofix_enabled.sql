-- +goose Up

-- runs.ci_autofix_enabled: the per-run override for automatic CI-autofix (issue #4617).
-- Mirrors runs.mr_rework_enabled (00179): NULLABLE with no default, tri-state
-- (NULL = inherit the owner default users.ci_autofix_enabled, true/false = explicit
-- override). Eligibility resolves LIVE at read time in ListCIAutofixCandidateRefs via
-- COALESCE(run, owner) IS NOT FALSE, so there is no owner-default snapshot at creation:
-- a run left NULL simply follows the owner's default, exactly today's behaviour.
--
-- Additive nullable column, no table rewrite. The ALTER takes a brief lock on runs, so the
-- statement times out rather than queue behind a long transaction and stall run writes.
-- Rollback drops the column, which resets every override to inherit.
--
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the next
-- free number above the live head if it drifts, per the CLAUDE.md convention.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs ADD COLUMN ci_autofix_enabled boolean;

-- +goose Down
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs DROP COLUMN ci_autofix_enabled;
