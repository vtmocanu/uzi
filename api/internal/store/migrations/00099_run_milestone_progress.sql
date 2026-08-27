-- +goose Up

-- Milestone progress + scaled budget (PRD #122 M2). Four columns on `runs`, two
-- pairs with distinct lifecycles:
--
--   milestones_completed    the UNIONED (monotone, dedup) set of frozen milestone
--                           ids the lead has reported done (Decision 3). Grows only —
--                           a milestone never un-completes.
--   milestones_in_progress  the OVERWRITTEN snapshot of ids currently being worked
--                           (Decision 3). Replaced wholesale on each report, since a
--                           snapshot has no monotonicity to preserve.
--
-- Both hold a jsonb ARRAY OF IDS (not {id,title} objects — the titles live on
-- milestones_frozen, 00098; these reference into it), and NULL is NOT the same as
-- '[]' (as in 00098):
--   NULL         nothing was ever reported for this column — a run whose lead never
--                reported progress, which behaves exactly as a pre-feature run.
--   '[]'::jsonb  a progress report arrived and the set was empty.
-- Anything that collapses those two (COALESCE to '[]', NOT NULL DEFAULT '[]') would
-- make every historical run claim it had reported progress. Don't.
--
--   budget_max_iterations   the effective per-run turn ceiling derived SERVER-SIDE
--                           from the frozen milestone count at freeze (Decision
--                           5/5b/12), written IMMUTABLY.
--   budget_wall_seconds     the effective per-run wall clock, likewise derived at
--                           freeze and honoured by SweepRunningTimeout + the run-health
--                           slow clamp.
-- NULL means "use the global default" (RUN_MAX_ITERATIONS / RUN_TIMEOUT) — so a zero-
-- or one-milestone run is byte-for-byte a pre-feature run. A CHECK keeps a persisted
-- budget positive: a stored value is always a real ceiling, never 0/negative.
ALTER TABLE runs
    ADD COLUMN milestones_completed   jsonb
        CHECK (milestones_completed IS NULL OR jsonb_typeof(milestones_completed) = 'array'),
    ADD COLUMN milestones_in_progress jsonb
        CHECK (milestones_in_progress IS NULL OR jsonb_typeof(milestones_in_progress) = 'array'),
    ADD COLUMN budget_max_iterations  int
        CHECK (budget_max_iterations IS NULL OR budget_max_iterations > 0),
    ADD COLUMN budget_wall_seconds    int
        CHECK (budget_wall_seconds IS NULL OR budget_wall_seconds > 0);

-- +goose Down
ALTER TABLE runs
    DROP COLUMN milestones_completed,
    DROP COLUMN milestones_in_progress,
    DROP COLUMN budget_max_iterations,
    DROP COLUMN budget_wall_seconds;
