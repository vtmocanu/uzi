-- +goose Up

-- Worker-advertised concurrency cap (PRD #42 Decisions 3 & 10): a worker reports
-- WORKER_MAX_CONCURRENT_RUNS at registration and the server records it HERE for
-- observability only. It is never enforced server-side — no claim-SQL change, no
-- constraint (ADR-42 Options B/A' rejected). NULL when the worker advertises no
-- cap (an older image, or M3a before the M2 agent starts sending it). Renumbered
-- to the next free slot above the live head at landing time (repo convention: the
-- number written here is a parallel-PRD draft, not the final version).
ALTER TABLE workers ADD COLUMN max_concurrent_runs int;

-- +goose Down
ALTER TABLE workers DROP COLUMN max_concurrent_runs;
