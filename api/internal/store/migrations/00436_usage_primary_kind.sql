-- +goose Up
-- Issue #4209: the Overview usage and outcome counts measure PRIMARY work runs (issue, task,
-- prompt, job, self_improve), not helper runs (chat, judge, cross_check, mr_rework, ci_fix).
-- This is the single definition of that population; the SelfUsage / AdminUsageTotals /
-- AdminUsagePerUser run counts and the three *RunOutcomes queries call it.
--
-- Query-only classifier: it must never back an index, CHECK constraint or generated column.
-- Redefining the list later would leave such stored dependents stale. It is deliberately not
-- STRICT and not SECURITY DEFINER and carries no SET clause, so PostgreSQL inlines it into
-- the calling query and the planner sees a plain `kind = ANY (...)` qual.
-- +goose StatementBegin
CREATE FUNCTION fn_usage_primary_kind(run_kind text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT run_kind IN ('issue', 'task', 'prompt', 'job', 'self_improve')
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS fn_usage_primary_kind(text);
