-- +goose Up

-- CI-fix runs (PRD #6 Phase 2). A run is now one of two kinds: an issue run (the
-- existing path — an agent works a PRD issue) or a ci_fix run (an agent diagnoses
-- + fixes a failed pipeline). A ci_fix run has no issue, so issue_iid becomes
-- nullable and a CHECK pins the per-kind shape; the failed pipeline it targets is
-- snapshotted onto the run so it stays self-contained (the pipeline_statuses cache
-- row is overwritten by newer syncs). fix_verdict is stamped by the pipeline sync
-- (verified/fix_failed) or reported by the worker (not_code).
ALTER TABLE runs
    ADD COLUMN kind text NOT NULL DEFAULT 'issue' CHECK (kind IN ('issue', 'ci_fix')),
    ALTER COLUMN issue_iid DROP NOT NULL,
    ADD COLUMN pipeline_id bigint,
    ADD COLUMN pipeline_ref text,                 -- the failed ref (default branch or an agent branch)
    ADD COLUMN failure_snapshot jsonb,            -- failed jobs + truncated log tails, frozen at queue time
    ADD COLUMN fix_verdict text
        CHECK (fix_verdict IS NULL OR fix_verdict IN ('verified', 'fix_failed', 'not_code')),
    ADD CONSTRAINT runs_kind_shape CHECK (
        (kind = 'issue'  AND issue_iid IS NOT NULL)
     OR (kind = 'ci_fix' AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL));

-- Re-scope the existing one-active-run-per-issue index to issue runs only. This is
-- defensive, not load-bearing: ci_fix rows carry NULL issue_iid and Postgres treats
-- NULLs as distinct in a unique index, so they could never have collided anyway —
-- but the explicit predicate documents that this index is about issue runs.
DROP INDEX uq_runs_one_active_per_issue;
CREATE UNIQUE INDEX uq_runs_one_active_per_issue
    ON runs (repo_id, issue_iid)
    WHERE kind = 'issue' AND status NOT IN ('completed', 'failed', 'cancelled');

-- One active ci_fix per failing ref (mirrors the per-issue index): a second Fix CI
-- on a ref that already has an active fix run is rejected (23505 → 409).
CREATE UNIQUE INDEX uq_runs_one_active_ci_fix
    ON runs (repo_id, pipeline_ref)
    WHERE kind = 'ci_fix' AND status NOT IN ('completed', 'failed', 'cancelled');

-- +goose Down
DROP INDEX uq_runs_one_active_ci_fix;
DROP INDEX uq_runs_one_active_per_issue;
CREATE UNIQUE INDEX uq_runs_one_active_per_issue
    ON runs (repo_id, issue_iid)
    WHERE status NOT IN ('completed', 'failed', 'cancelled');
ALTER TABLE runs
    DROP CONSTRAINT runs_kind_shape,
    DROP COLUMN fix_verdict,
    DROP COLUMN failure_snapshot,
    DROP COLUMN pipeline_ref,
    DROP COLUMN pipeline_id,
    ALTER COLUMN issue_iid SET NOT NULL,
    DROP COLUMN kind;
