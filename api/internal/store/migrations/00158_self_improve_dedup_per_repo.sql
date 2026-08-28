-- Self-improvement dedup, per-repo (PRD #590 M1). The original
-- uq_runs_one_active_self_improve (00058_run_judge_self_improve_kinds.sql) admits at
-- most ONE non-terminal self_improve run INSTANCE-WIDE — correct when a single bespoke
-- engine drove one fixed uzi repo, but too coarse once a self_improve schedule can be
-- enabled per repo. Re-scope the partial index to (repo_id) so each repo gets its own
-- one-active guard. runs_kind_shape already forces a self_improve row to carry a NOT NULL
-- repo_id, so the partial index is well-defined for every row it covers.

-- +goose Up
DROP INDEX IF EXISTS uq_runs_one_active_self_improve;

CREATE UNIQUE INDEX uq_runs_one_active_self_improve
    ON runs (repo_id)
    WHERE kind = 'self_improve' AND status NOT IN ('completed', 'failed', 'cancelled');

-- +goose Down
-- The up newly permits one active self_improve run PER REPO; the historical instance-wide
-- UNIQUE(kind) index below admits only one total, so a faithful rollback must first collapse
-- any surplus. Cancel every non-terminal self_improve run except the oldest so the unique
-- index can be rebuilt deterministically (down-migrations are manual and this intentionally
-- reverts to the stricter instance-wide guard).
DROP INDEX IF EXISTS uq_runs_one_active_self_improve;

UPDATE runs SET status = 'cancelled'
 WHERE kind = 'self_improve'
   AND status NOT IN ('completed', 'failed', 'cancelled')
   AND id <> (
       SELECT id FROM runs
        WHERE kind = 'self_improve'
          AND status NOT IN ('completed', 'failed', 'cancelled')
        ORDER BY created_at ASC, id ASC
        LIMIT 1
   );

CREATE UNIQUE INDEX uq_runs_one_active_self_improve
    ON runs (kind)
    WHERE kind = 'self_improve' AND status NOT IN ('completed', 'failed', 'cancelled');
