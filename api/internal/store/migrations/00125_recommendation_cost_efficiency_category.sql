-- +goose Up

-- Widen the review_recommendations.category domain with a seventh value,
-- `cost_efficiency` (issue: add a cost-efficiency judge recommendation category). The
-- retrospective judge now surfaces quality-first cost-efficiency findings — ways a run
-- could have spent fewer tokens/turns/agents WITHOUT reducing correctness, verification
-- depth, or code quality (agent/src/judge-runner.ts JUDGE_SYSTEM_PROMPT). This category
-- must be accepted by the ingest validator (workersvc.RecommendationCategories) and the
-- DB CHECK that backstops it.
--
-- The category CHECK was created inline-unnamed in 00059, so Postgres auto-named it
-- `review_recommendations_category_check` (single-column CHECK → <table>_<column>_check);
-- it has never been altered since (verified: no prior ALTER touches it). Drop and re-add
-- it with the widened set — the six existing values carried verbatim from 00059 plus
-- `cost_efficiency`.
--
-- Constraint-only change: no column added/typed, no new `-- name:` query, so this needs
-- NO sqlc regen (sqlc reads the schema for column types, which a CHECK does not affect).
--
-- NOTE (goose numbering): drafted as 00125 — the next free number above the live head
-- (00124) at drafting time — and renumbered at the landing merge if another migration
-- lands first, per the CLAUDE.md convention (goose is strict, no allow-missing).
ALTER TABLE review_recommendations DROP CONSTRAINT review_recommendations_category_check;
ALTER TABLE review_recommendations ADD CONSTRAINT review_recommendations_category_check
    CHECK (category IN (
        'enable_tool', 'install_worker_tool', 'adjust_template',
        'improve_agent', 'add_agent', 'improve_uzi', 'cost_efficiency'));

-- +goose Down

-- Narrow back to the original six-value set (00059). Any `cost_efficiency` rows written
-- while this migration was applied would violate the narrower CHECK, so drop them first —
-- a down-migration undoing this feature cannot keep values the restored domain forbids.
DELETE FROM review_recommendations WHERE category = 'cost_efficiency';
ALTER TABLE review_recommendations DROP CONSTRAINT review_recommendations_category_check;
ALTER TABLE review_recommendations ADD CONSTRAINT review_recommendations_category_check
    CHECK (category IN (
        'enable_tool', 'install_worker_tool', 'adjust_template',
        'improve_agent', 'add_agent', 'improve_uzi'));
