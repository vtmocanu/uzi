-- +goose Up

-- Seeded plans (PRD #209): where a run's plan_md came from.
--
--   'agent'   the plan was produced INSIDE the worker (or the row predates this
--             column) — a normal issue run that planned in Phase 1, or a resume of
--             one. This is the default, so every historical run backfills to it and
--             the anti-regression criterion (Success Criterion 2) holds by
--             construction: a run created with no seeded plan is 'agent', which is
--             the state the whole codebase already assumed.
--   'seeded'  the plan was supplied at create time by an external planner (a local
--             Claude Code session) over the API, NOT derived from the issue. Such a
--             run skips the Phase-1 planning turn and the approval gate.
--
-- NOT NULL DEFAULT 'agent' is deliberate on both counts (PRD #209 D8):
--   * NOT NULL keeps the plan_approved third disjunct a plain `run.PlanSource ==
--     "seeded"` string compare rather than a pgtype.Text unwrap. sqlc types a
--     nullable text column as pgtype.Text, and the bare `== "seeded"` form would not
--     compile against that; the NOT NULL column reads as a plain Go string.
--   * DEFAULT 'agent' makes the backfill of every existing row safe and total.
--
-- 🔴 plan_source describes the row's BIRTH; plan_md stays MUTABLE. The two can
-- diverge, and that divergence is the D8 safety bug: a seeded run that falls
-- through to the plan gate has SetRunAwaitingApproval rewrite plan_md with the
-- worker's OWN Phase-1 plan while plan_source still reads 'seeded', so a later
-- claim ships plan_approved=true over an unreviewed plan_md. The fix keeps the
-- column tracking provenance rather than birth: SetRunAwaitingApproval sets
-- plan_source = 'agent' in the same UPDATE that rewrites plan_md (see runtime.sql).
-- The column DDL cannot enforce that; it is a query invariant, guarded by a test.
ALTER TABLE runs
    ADD COLUMN plan_source text NOT NULL DEFAULT 'agent'
        CHECK (plan_source IN ('agent', 'seeded'));

-- +goose Down
ALTER TABLE runs
    DROP COLUMN plan_source;
