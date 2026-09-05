-- +goose Up

-- Per-schedule output mode (PRD #929 M1): the delivery mechanism a proposal-shaped
-- prompt schedule uses — 'mr' (write an idea file and open a merge request, today's
-- behavior) or 'issues' (file the proposal as a forge issue server-side). Nullable, and
-- NULL is meaningful: it means "use the catalog/job default" (resolved in Go at fire time
-- from the frontmatter `output:` key, default 'mr'), so an existing enablement is
-- unaffected on upgrade and never has an mode invented at create time. The setting is
-- honored only for prompt-target schedules; sweep/issue targets reject it at the handler
-- (their output is a run on an existing issue, not a proposal), and this column stays NULL
-- for them. The CHECK enforces the enum while still admitting NULL.
--
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the next free
-- number above the live head if it drifts, per the CLAUDE.md convention.
ALTER TABLE run_schedules ADD COLUMN output_mode text;
-- Added NOT VALID: the CHECK enforces the enum on all NEW/updated rows immediately, but
-- skips the validating table scan (and the ACCESS EXCLUSIVE lock it holds) at add time. A
-- separate later migration (00194) runs VALIDATE CONSTRAINT under a write-compatible lock.
-- (Existing rows are all NULL here — the column is added in this same migration — so the
-- validation is trivially satisfied; the two-step is the safe pattern regardless.)
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_output_mode_check CHECK (output_mode IS NULL OR output_mode IN ('mr','issues')) NOT VALID;

-- +goose Down
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_output_mode_check;
ALTER TABLE run_schedules DROP COLUMN output_mode;
