-- +goose Up

-- PRD #2006: ephemeral worker lease. A finished ephemeral (run-bound) worker may stay warm for a
-- bounded lease and take a same-owner, same-repository, same-branch follow-up run instead of being
-- torn down at once. The lease lives on the workers row, so lease entry, claim-and-rebind, expiry,
-- eviction, drain, re-register and teardown all write or delete that one row under its row lock.
--
-- The migration number is a draft: it is renumbered above the live head at landing.

-- 1. The lease. lease_since is the instant the lease started; the lease is live at time t when
-- lease_since + <the operator's lease interval> > t AND the worker is not draining (the interval is
-- the api's UZI_EPHEMERAL_LEASE, passed to every predicate, never stored). lease_repo_id and
-- lease_branch record WHAT the lease reuses for: the last bound run's repository and effective
-- branch identity (fn_run_lease_branch below). lease_repo_id deliberately has no FK: the lease is a
-- scheduling hint that expires on its own, and a hard-deleted repository must not be blocked by,
-- or cascade into, a worker row.
ALTER TABLE workers
    ADD COLUMN lease_since   timestamptz,
    ADD COLUMN lease_repo_id uuid,
    ADD COLUMN lease_branch  text;

-- All three are set together or none are, and only an ephemeral worker outside the isolated lane
-- may hold one (the lane's one-run-per-worker invariant stays: no file survives into another run).
ALTER TABLE workers ADD CONSTRAINT ck_workers_ephemeral_lease CHECK (
    ((lease_since IS NULL) = (lease_repo_id IS NULL))
    AND ((lease_since IS NULL) = (lease_branch IS NULL))
    AND (lease_branch IS NULL OR lease_branch <> '')
    AND (lease_since IS NULL OR (ephemeral AND NOT isolated_lane))
);

-- 2. The effective branch identity of a run: the non-empty branch a lease binds to and a follow-up
-- must match. NULL means "no derivable identity", and NULL never equals NULL, so a run with none is
-- never lease-eligible (fail closed).
--   issue      always the canonical 'agent/issue-<iid>' for a positive issue_iid, DERIVED from the
--              server-owned issue_iid and never read from runs.branch. runs.branch of an issue run
--              is written from the worker's own terminal report (an untrusted field), so it is
--              accepted only as a no-op: NULL or empty (a queued run) or exactly the canonical value.
--              Any other recorded branch, or a non-positive issue_iid, gives NULL: a worker cannot
--              choose its lease branch, so it cannot steer the lease onto another issue's run.
--              Mirrors agentIssueBranch in api/internal/workersvc/ci_fix.go.
--   mr_rework  pipeline_ref (its runs.branch is NULL), as claim_assembly.go sources the claim's
--              Branch.
--   ci_fix     the failure snapshot's string 'ref', the field claimPipelineFromSnapshot decodes.
--   any other kind has no identity.
-- The Go sources are pinned to this function by TestLeaseBranchParityLiveDB in
-- api/internal/workersvc; change either side and that test goes red.
-- +goose StatementBegin
CREATE FUNCTION fn_run_lease_branch(
    run_kind text,
    run_branch text,
    run_pipeline_ref text,
    run_issue_iid bigint,
    run_failure_snapshot jsonb
) RETURNS text
LANGUAGE sql IMMUTABLE
AS $$
    SELECT CASE run_kind
        WHEN 'issue' THEN CASE
            WHEN run_issue_iid > 0
                 AND (NULLIF(run_branch, '') IS NULL
                      OR run_branch = 'agent/issue-' || run_issue_iid::text)
            THEN 'agent/issue-' || run_issue_iid::text
        END
        WHEN 'mr_rework' THEN NULLIF(run_pipeline_ref, '')
        WHEN 'ci_fix' THEN CASE WHEN jsonb_typeof(run_failure_snapshot -> 'ref') = 'string'
                                THEN NULLIF(run_failure_snapshot ->> 'ref', '') END
        ELSE NULL
    END;
$$;
-- +goose StatementEnd

-- 3. The single lease-admission predicate every claim and placement site shares, so the claimant
-- clause, the spread-peer mirror, CountOnlineWorkersClaimableForRun and the provisioning queries
-- cannot drift apart. TRUE only when the lease is live at `at`, the worker is not draining, the run
-- is an ordinary repo-backed run (no egress profile), and its repository and effective branch
-- identity equal the lease's. `at` is explicit: an authoritative site passes a fresh clock reading
-- taken after every lock it depends on, an advisory mirror passes now(). The owner match is not
-- here: every call site already scopes the worker and the run to one user. COALESCEd to FALSE so a
-- NULL anywhere (no lease, no identity, an unset interval) can never read as admission. STABLE,
-- not IMMUTABLE: timestamptz + interval depends on the session time zone.
-- +goose StatementBegin
CREATE FUNCTION fn_ephemeral_lease_admits(
    w_lease_since timestamptz,
    w_lease_repo_id uuid,
    w_lease_branch text,
    w_is_draining boolean,
    lease interval,
    at timestamptz,
    run_repo_id uuid,
    run_kind text,
    run_branch text,
    run_pipeline_ref text,
    run_issue_iid bigint,
    run_failure_snapshot jsonb,
    run_egress_profile_id uuid
) RETURNS boolean
LANGUAGE sql STABLE
AS $$
    SELECT COALESCE(
        w_lease_since IS NOT NULL
        AND NOT w_is_draining
        AND w_lease_since + lease > at
        AND run_egress_profile_id IS NULL
        AND run_repo_id IS NOT NULL
        AND run_repo_id = w_lease_repo_id
        AND fn_run_lease_branch(run_kind, run_branch, run_pipeline_ref, run_issue_iid, run_failure_snapshot)
            = w_lease_branch,
        false);
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS fn_ephemeral_lease_admits(timestamptz, uuid, text, boolean, interval, timestamptz, uuid, text, text, text, bigint, jsonb, uuid);
DROP FUNCTION IF EXISTS fn_run_lease_branch(text, text, text, bigint, jsonb);
ALTER TABLE workers DROP CONSTRAINT IF EXISTS ck_workers_ephemeral_lease;
ALTER TABLE workers
    DROP COLUMN IF EXISTS lease_branch,
    DROP COLUMN IF EXISTS lease_repo_id,
    DROP COLUMN IF EXISTS lease_since;
