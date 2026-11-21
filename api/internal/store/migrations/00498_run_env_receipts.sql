-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4416 M2: delivery receipts, the never-reused variable identity, the version-2 AAD
-- discriminator and the lead-lane predicate.
--
-- run_env_receipts: one row per (run, variable version) a claim delivered, plain and
-- secret alike (the run page names what ran). Retention, the retire/pending-delete
-- decisions and the finish gate read the SECRET receipts only (the partial index).
-- There is no foreign key to run_env_vars (a receipt outlives the version it names, so a
-- deleted or purged row stays visible as missing) and no value or hash is ever stored.
-- user_id is the run owner, carried so the receipt can be matched to the owner's rows
-- without a join. first/last_claim_generation bound which claims delivered the version.
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
CREATE INDEX run_env_receipts_secret_version_idx
    ON run_env_receipts (var_id, rev, run_id) WHERE secret;

-- run_env_var_ids: the identity of every variable ever created, kept after its rows and
-- its repo are gone. A var_id is bound to one (user, repo, scope, name) for good, so a
-- receipt that names a var_id can never be satisfied by a row of another variable. repo_id
-- has no foreign key on purpose: the identity must survive the repo.
CREATE TABLE run_env_var_ids (
    var_id  uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id uuid,
    scope   text NOT NULL CHECK (scope IN ('repo', 'user')),
    name    text NOT NULL
);

INSERT INTO run_env_var_ids (var_id, user_id, repo_id, scope, name)
SELECT DISTINCT ON (var_id)
       var_id, user_id, repo_id, CASE WHEN repo_id IS NULL THEN 'user' ELSE 'repo' END, name
FROM run_env_vars
ORDER BY var_id, rev DESC;

-- The identity guard. On INSERT the identity row is created if absent and the stored
-- (user, repo, scope, name) must equal the new row's, which refuses a reused var_id
-- under another name, repo, scope or owner even after every row of it was deleted. On
-- UPDATE the identity columns may not change. check_violation (23514) is raised.
-- +goose StatementBegin
CREATE FUNCTION run_env_vars_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    ident run_env_var_ids%ROWTYPE;
    new_scope text := CASE WHEN NEW.repo_id IS NULL THEN 'user' ELSE 'repo' END;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.var_id IS DISTINCT FROM OLD.var_id
           OR NEW.user_id IS DISTINCT FROM OLD.user_id
           OR NEW.repo_id IS DISTINCT FROM OLD.repo_id
           OR NEW.name IS DISTINCT FROM OLD.name THEN
            RAISE EXCEPTION 'run_env_vars: the identity of variable % cannot change', OLD.var_id
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    INSERT INTO run_env_var_ids (var_id, user_id, repo_id, scope, name)
    VALUES (NEW.var_id, NEW.user_id, NEW.repo_id, new_scope, NEW.name)
    ON CONFLICT (var_id) DO NOTHING;
    SELECT * INTO ident FROM run_env_var_ids WHERE var_id = NEW.var_id;
    IF ident.user_id IS DISTINCT FROM NEW.user_id
       OR ident.repo_id IS DISTINCT FROM NEW.repo_id
       OR ident.scope IS DISTINCT FROM new_scope
       OR ident.name IS DISTINCT FROM NEW.name THEN
        RAISE EXCEPTION 'run_env_vars: variable id % is already bound to another identity', NEW.var_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER run_env_vars_identity_guard
    BEFORE INSERT OR UPDATE ON run_env_vars
    FOR EACH ROW EXECUTE FUNCTION run_env_vars_identity_guard();

-- aad_version names the sealing context a row was sealed under: 1 is the M1 context
-- (scope, repo, secret flag, name), 2 adds the owner, var_id and rev so a ciphertext cannot
-- be moved between versions. Existing rows stay 1; every new seal writes 2.
ALTER TABLE run_env_vars
    ADD COLUMN aad_version smallint NOT NULL DEFAULT 1,
    ADD CONSTRAINT run_env_vars_aad_version_check CHECK (aad_version IN (1, 2));

-- The retention sweeper's scan: only the owners that have a retired or pending-deletion row.
CREATE INDEX run_env_vars_sweep_idx ON run_env_vars (user_id)
    WHERE retired_at IS NOT NULL OR pending_delete_at IS NOT NULL;

-- Mirrors Go leadcfg.LeadLane (and the fence of InsertRunLeadConfigFenced): a run has a
-- lead agent unless it is a review run or is bound to an egress profile, and only for
-- the listed kinds. Run environment variables are delivered to lead-lane runs only.
-- +goose StatementBegin
CREATE FUNCTION fn_run_env_lane(kind text, review_target_run_id uuid, egress_profile_id uuid)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
    SELECT review_target_run_id IS NULL
       AND egress_profile_id IS NULL
       AND COALESCE(kind IN ('issue', 'ci_fix', 'prompt', 'self_improve', 'mr_rework', 'task'), false)
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP FUNCTION fn_run_env_lane(text, uuid, uuid);
DROP INDEX run_env_vars_sweep_idx;
ALTER TABLE run_env_vars
    DROP CONSTRAINT run_env_vars_aad_version_check,
    DROP COLUMN aad_version;
DROP TRIGGER run_env_vars_identity_guard ON run_env_vars;
DROP FUNCTION run_env_vars_identity_guard();
DROP TABLE run_env_var_ids;
DROP TABLE run_env_receipts;
