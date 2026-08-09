-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION fn_worker_can_claim(
    is_docker boolean,
    allowlist uuid[],
    run_repo_id uuid,
    run_kind text
) RETURNS boolean
LANGUAGE sql IMMUTABLE
AS $$
    -- PRD #216 D5: the SINGLE per-(worker,run) claim-eligibility expression,
    -- called twice in ClaimRun (the claiming worker, and each candidate peer).
    -- PRD #84 (capability-aware scheduling) EXTENDS this signature rather than
    -- duplicating the predicate.
    --
    -- Docker-worker repo allowlist (PRD #89 M-allow): a docker worker may claim
    -- ONLY runs whose repo is on the trusted allowlist; repo-less JUDGE runs are
    -- exempt (scoped to kind='judge' so a future repo-less kind fail-closes); an
    -- EMPTY allowlist is fail-closed (= ANY('{}') is false for every repo). A
    -- non-docker worker short-circuits true. NOT declared STRICT deliberately:
    -- run_repo_id is legitimately NULL for judge runs, and a STRICT function
    -- returns NULL WITHOUT running its body, which would silently filter every
    -- judge run out (a docker worker could never claim a judge run).
    SELECT NOT is_docker
        OR (run_repo_id IS NULL AND run_kind = 'judge')
        OR run_repo_id = ANY(allowlist);
$$;
-- +goose StatementEnd

-- PRD #216 D11: no existing index serves "active non-chat runs of worker X"
-- (idx_runs_worker is (worker_id) alone). The peer-load subquery in ClaimRun and
-- the UI's active_runs both scan exactly this set. Partial index on the RUN lane.
CREATE INDEX idx_runs_worker_active ON runs (worker_id)
    WHERE status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
      AND kind <> 'chat';

-- +goose Down
DROP INDEX IF EXISTS idx_runs_worker_active;
DROP FUNCTION IF EXISTS fn_worker_can_claim(boolean, uuid[], uuid, text);
