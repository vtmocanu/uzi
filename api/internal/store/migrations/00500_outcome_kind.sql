-- +goose Up
-- Issue #4577: the run OUTCOME population (finished, completed, cancelled, plan_rejected,
-- base_diverged, failed, fail_origins, last failure, completed-since-last-failure and
-- needs_landing; both windows; self, factory and per-user) is every run kind except chat. The
-- usage run counts keep fn_usage_primary_kind (#4209). The three *RunOutcomes queries call
-- this function.
--
-- Query-only classifier: it must never back an index, CHECK constraint or generated column.
-- Redefining the predicate later would leave such stored dependents stale. It is deliberately
-- not STRICT and not SECURITY DEFINER and carries no SET clause, so PostgreSQL inlines it into
-- the calling query.
-- +goose StatementBegin
CREATE FUNCTION fn_outcome_kind(run_kind text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT run_kind <> 'chat'
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS fn_outcome_kind(text);
