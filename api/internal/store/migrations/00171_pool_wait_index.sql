-- +goose Up

-- PRD #754 M5: a partial index backing the reactive-resume worklist (ListPoolWaitRuns,
-- run every sweeper tick, ~15s). Without it that query is a sequential scan of the whole
-- runs table — which grows without bound as history accumulates — to find the handful of
-- rows currently held. The partial predicate keeps the index itself tiny (only pool_wait
-- rows are in it, and the set empties as M5 resumes them), and status_since is its sort
-- key so the oldest-first ORDER BY is index-ordered. Mirrors idx_runs_limit_wait_retry,
-- the equivalent index for the usage-limit park's promotion pass (00091).
CREATE INDEX idx_runs_pool_wait ON runs (status_since) WHERE status = 'pool_wait';

-- +goose Down

DROP INDEX idx_runs_pool_wait;
