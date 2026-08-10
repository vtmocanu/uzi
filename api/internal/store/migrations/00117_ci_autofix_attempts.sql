-- +goose Up

-- PRD #71 M4 (ci-autofix loop guard). The per-(repo, ref) ledger the M6 poller
-- detector reads/writes to bound how many AUTOMATIC ci_fix attempts a failing
-- agent MR branch may spend, dedup pipelines already evaluated, and latch the
-- comment-once halt notice. It is INDEPENDENT of the runs table (a ci_fix run is
-- terminal-and-evicted long before the next failure on the same ref), so a durable
-- "attempts spent on this ref" guarantee cannot live on the runs rows and lives
-- here instead. Reset-on-green (DeleteCIAutofixAttempt) and reconcile eviction
-- (DeleteCIAutofixAttemptsNotIn) keep it from outliving the ref it guards, so a
-- reused agent/issue-N branch never inherits a stale count.
CREATE TABLE ci_autofix_attempts (
    repo_id          uuid   NOT NULL REFERENCES repos ON DELETE CASCADE,
    ref              text   NOT NULL,
    attempt_count    int    NOT NULL DEFAULT 0,   -- AUTO attempts only (cap default 2)
    last_signature   text,                         -- failure signature the last attempt targeted
    last_pipeline_id bigint,                        -- dedup: pipeline already evaluated
    halt_notified    boolean NOT NULL DEFAULT false, -- comment-once latch after the cap/no-progress halt
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, ref)
);

-- +goose Down
DROP TABLE ci_autofix_attempts;
