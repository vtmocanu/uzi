-- +goose Up

-- Single-source the "effective worker capabilities" docker fold (issue #512 M5).
-- Before this migration the fold `capabilities ∪ {docker if docker_enabled}` was
-- inlined in four places (fn_worker_can_claim here, CountOnlineWorkersSatisfyingCaps
-- in queries/runtime.sql, effectiveOwningWorkerCaps in Go, and the RunView worker-caps
-- memo in TS). Three languages cannot literally share, so this collapses the two SQL
-- copies into ONE fn_effective_worker_caps; the Go and TS copies become one named
-- helper each (capability.EffectiveWorkerCaps / effectiveWorkerCaps), all cross-
-- referencing this function by name. This is a BEHAVIOR-PRESERVING cleanup.
--
-- NOTE: goose migration numbers are DRAFT until this branch lands — the lead renumbers
-- above the live head at merge if 00151 has been taken. Nothing cites the number.

-- The fold, extracted verbatim from fn_worker_can_claim (migration 00142): the worker's
-- caps unioned with ARRAY['docker'] when it is docker-enabled. IMMUTABLE so the planner
-- may fold it into the callers' expressions exactly as the inline form was. It COALESCEs
-- worker_caps to '{}' so a NULL array is the empty set; is_docker is always a non-null
-- boolean at both call sites (00142 passes an already-COALESCE'd value, runtime.sql passes
-- COALESCE(w.docker_enabled, false)), so this is BYTE-EQUIVALENT to both inline folds.
-- +goose StatementBegin
CREATE FUNCTION fn_effective_worker_caps(worker_caps text[], is_docker boolean)
RETURNS text[]
LANGUAGE sql IMMUTABLE
AS $$
    SELECT COALESCE(worker_caps, '{}') || CASE WHEN is_docker THEN ARRAY['docker'] ELSE ARRAY[]::text[] END;
$$;
-- +goose StatementEnd

-- Re-create fn_worker_can_claim with the SAME 7-arg signature and body as migration 00142,
-- the ONLY change being the fold sub-expression delegating to fn_effective_worker_caps.
-- The allowlist clause and the COALESCE(run_required_capabilities, '{}') <@ … match 00142
-- verbatim; CREATE OR REPLACE keeps the signature so every caller (ClaimRun, the peer
-- clause) is unaffected.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_worker_can_claim(
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
            OR COALESCE(run_required_capabilities, '{}') <@ fn_effective_worker_caps(worker_caps, is_docker));
$$;
-- +goose StatementEnd

-- +goose Down

-- Restore the INLINE-fold body from migration 00142 verbatim, so rollback re-inlines the
-- fold and no longer references the helper. This runs FIRST (replace the caller before
-- dropping the callee), then the helper is dropped.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_worker_can_claim(
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

DROP FUNCTION IF EXISTS fn_effective_worker_caps(text[], boolean);
