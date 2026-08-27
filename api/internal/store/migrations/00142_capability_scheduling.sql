-- +goose Up

-- Run requirement set (PRD #84 M2). The NON-provisionable capabilities a run needs
-- a worker to already have (today only {docker}; {jvm} is exercised but its full
-- retrofit is M5). NOT NULL DEFAULT '{}' so the empty set means "no requirement" —
-- an unrequired run claims anywhere — and existing rows need NO backfill. The
-- provisionable toolchain (required_tools) is a separate M4 column, not added here.
ALTER TABLE runs ADD COLUMN required_capabilities text[] NOT NULL DEFAULT '{}';

-- Static per-repo capability hint (PRD #84 M2), the pre-claim declaration source
-- (Decision 2). Known before a plan exists, so the enqueue seam (Service.createRun)
-- copies it onto every new issue run's required_capabilities. NOT NULL DEFAULT '{}'
-- for the same no-backfill reason. Every value is Filter-ed against the server-owned
-- vocabulary at the write path so an unknown/spoofed name is never persisted.
ALTER TABLE repos ADD COLUMN required_capabilities text[] NOT NULL DEFAULT '{}';

-- Extend fn_worker_can_claim (PRD #84 M2) — its own comment (migration 00113) reserves
-- this for #84 rather than a fork. The arg list changes (three trailing args:
-- worker_caps, run_required_capabilities, capability_aware), so the function is DROPped
-- then re-CREATEd with the new signature. The existing docker-worker→allowlist clause
-- is copied VERBATIM and a new capability-match clause is AND-ed onto it.
--
-- The `<@` "contained-by" gives `required ⊆ effective_worker_caps`, so an empty
-- required set is a subset of everything and an unrequired run claims anywhere. The
-- effective set folds is_docker INTO the worker's caps (ARRAY['docker'] when the
-- worker is docker-enabled), so the hosted docker_enabled source and the self-reported
-- `docker` capability source can never disagree at claim time. capability_aware=false
-- makes the whole new clause trivially true, so the kill-switch reverts ONLY this
-- addition while the docker→allowlist clause stays AND-ed and always enforced
-- (PRD #84 Decision 13). NOT declared STRICT (run_repo_id is legitimately NULL for a
-- judge run — see 00113). The COALESCEs guard against any NULL array slipping in.
DROP FUNCTION IF EXISTS fn_worker_can_claim(boolean, uuid[], uuid, text);
-- +goose StatementBegin
CREATE FUNCTION fn_worker_can_claim(
    is_docker boolean,
    allowlist uuid[],
    run_repo_id uuid,
    run_kind text,
    worker_caps text[],
    run_required_capabilities text[],
    capability_aware boolean
) RETURNS boolean
LANGUAGE sql IMMUTABLE
AS $$
    SELECT (NOT is_docker
            OR (run_repo_id IS NULL AND run_kind = 'judge')
            OR run_repo_id = ANY(allowlist))
       AND (NOT capability_aware
            OR COALESCE(run_required_capabilities, '{}') <@
               (COALESCE(worker_caps, '{}') || CASE WHEN is_docker THEN ARRAY['docker'] ELSE ARRAY[]::text[] END));
$$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE runs DROP COLUMN required_capabilities;
ALTER TABLE repos DROP COLUMN required_capabilities;

-- Restore the exact 4-arg body from migration 00113 (copied verbatim) so rollback is
-- clean. 00113's own Down still DROPs the 4-arg signature; only this 00142 Up/Down
-- manages the 7-arg⇄4-arg transition.
DROP FUNCTION IF EXISTS fn_worker_can_claim(boolean, uuid[], uuid, text, text[], text[], boolean);
-- +goose StatementBegin
CREATE FUNCTION fn_worker_can_claim(
    is_docker boolean,
    allowlist uuid[],
    run_repo_id uuid,
    run_kind text
) RETURNS boolean
LANGUAGE sql IMMUTABLE
AS $$
    SELECT NOT is_docker
        OR (run_repo_id IS NULL AND run_kind = 'judge')
        OR run_repo_id = ANY(allowlist);
$$;
-- +goose StatementEnd
