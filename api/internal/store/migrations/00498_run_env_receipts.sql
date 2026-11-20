-- +goose Up

-- This migration takes ACCESS EXCLUSIVE on run_env_vars (a new column with a CHECK, a trigger),
-- SHARE ROW EXCLUSIVE on runs (the receipts foreign key) and builds indexes on tables that M1
-- created a moment ago. lock_timeout bounds only the wait for a lock, not how long it is held:
-- fail fast instead of queueing behind a long transaction (the 00402 pattern).
SET LOCAL lock_timeout = '5s';

-- PRD #4416 M2: delivery receipts, variable identity, AAD version, the run-env lane predicate.

-- One row per (run, variable version) a claim delivered, plain and secret alike: the run page
-- lists injected names from every receipt, retention reads the secret ones only. first/last
-- claim generation record when the version was first delivered and last re-delivered. A receipt
-- carries no value and no hash, and names no run_env_vars row: the version it describes may
-- be retired, pending deletion or already gone, and the receipt must outlive it.
CREATE TABLE run_env_receipts (
    run_id                 uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    user_id                uuid        NOT NULL,
    var_id                 uuid        NOT NULL,
    rev                    integer     NOT NULL,
    scope                  text        NOT NULL CHECK (scope IN ('repo', 'user')),
    name                   text        NOT NULL,
    secret                 boolean     NOT NULL,
    first_claim_generation bigint      NOT NULL,
    last_claim_generation  bigint      NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, var_id, rev)
);
-- Retention asks "does any run hold a secret receipt for this version" and "which runs".
CREATE INDEX run_env_receipts_secret_version_idx
    ON run_env_receipts (var_id, rev, run_id) WHERE secret;

-- The identity of every variable id ever minted. run_env_vars rows are dropped when a repo is
-- deleted or a version is replaced, so a var_id could otherwise be reused for another name or
-- another repo and an old receipt would then describe the wrong variable. repo_id has no foreign
-- key on purpose: the identity must survive its repo.
CREATE TABLE run_env_var_ids (
    var_id  uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id uuid,
    scope   text NOT NULL CHECK (scope IN ('repo', 'user')),
    name    text NOT NULL
);

INSERT INTO run_env_var_ids (var_id, user_id, repo_id, scope, name)
SELECT DISTINCT ON (var_id) var_id, user_id, repo_id,
       CASE WHEN repo_id IS NULL THEN 'user' ELSE 'repo' END, name
FROM run_env_vars
ORDER BY var_id, rev;

-- Records the identity on the first version of a variable and refuses any later row that
-- claims another one; refuses an UPDATE that moves a row to another variable, user, repo or
-- name. SQLSTATE UE001 is distinctive so tests can tell it from any other failure.
-- +goose StatementBegin
CREATE FUNCTION run_env_vars_identity_guard() RETURNS trigger AS $$
DECLARE
    stored run_env_var_ids%ROWTYPE;
    new_scope text;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.var_id IS DISTINCT FROM OLD.var_id
           OR NEW.user_id IS DISTINCT FROM OLD.user_id
           OR NEW.repo_id IS DISTINCT FROM OLD.repo_id
           OR NEW.name IS DISTINCT FROM OLD.name THEN
            RAISE EXCEPTION 'run_env_vars: var_id, user_id, repo_id and name never change (var_id %)', OLD.var_id
                USING ERRCODE = 'UE001';
        END IF;
        RETURN NEW;
    END IF;
    new_scope := CASE WHEN NEW.repo_id IS NULL THEN 'user' ELSE 'repo' END;
    INSERT INTO run_env_var_ids (var_id, user_id, repo_id, scope, name)
    VALUES (NEW.var_id, NEW.user_id, NEW.repo_id, new_scope, NEW.name)
    ON CONFLICT (var_id) DO NOTHING;
    SELECT * INTO stored FROM run_env_var_ids WHERE var_id = NEW.var_id;
    IF stored.user_id IS DISTINCT FROM NEW.user_id
       OR stored.repo_id IS DISTINCT FROM NEW.repo_id
       OR stored.scope IS DISTINCT FROM new_scope
       OR stored.name IS DISTINCT FROM NEW.name THEN
        RAISE EXCEPTION 'run_env_vars: var_id % was minted for another user, repo, scope or name', NEW.var_id
            USING ERRCODE = 'UE001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER run_env_vars_identity_guard
    BEFORE INSERT OR UPDATE ON run_env_vars
    FOR EACH ROW EXECUTE FUNCTION run_env_vars_identity_guard();

-- Which sealing context a row was written under. M1 rows are 1; every row written from M2 on is
-- 2, whose context also binds var_id and rev (internal/runenv AADFor).
ALTER TABLE run_env_vars
    ADD COLUMN aad_version smallint NOT NULL DEFAULT 1 CHECK (aad_version IN (1, 2));

-- Retention and the history budget read the retired and pending-deletion rows of one user.
CREATE INDEX run_env_vars_retained_user_idx ON run_env_vars (user_id)
    WHERE retired_at IS NOT NULL OR pending_delete_at IS NOT NULL;

-- The run-env lane: the runs that receive variables. Mirrors leadcfg.LeadLane (kinds issue,
-- ci_fix, prompt, self_improve, mr_rework and task, excluding a review run and an isolated-lane
-- run) and the write fence of InsertRunLeadConfigFenced in queries/lead_configs.sql, which is
-- pinned against it. The claim gate and run health read this one function.
-- +goose StatementBegin
CREATE FUNCTION fn_run_env_lane(kind text, review_target_run_id uuid, egress_profile_id uuid)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT coalesce(kind IN ('issue', 'ci_fix', 'prompt', 'self_improve', 'mr_rework', 'task'), false)
       AND review_target_run_id IS NULL
       AND egress_profile_id IS NULL
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP FUNCTION fn_run_env_lane(text, uuid, uuid);
DROP INDEX run_env_vars_retained_user_idx;
ALTER TABLE run_env_vars DROP COLUMN aad_version;
DROP TRIGGER run_env_vars_identity_guard ON run_env_vars;
DROP FUNCTION run_env_vars_identity_guard();
DROP TABLE run_env_var_ids;
DROP TABLE run_env_receipts;
