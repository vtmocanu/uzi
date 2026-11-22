-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Locks runs and users, then run_env_vars, before the backfill and the identity trigger.
LOCK TABLE runs, users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE run_env_vars IN ACCESS EXCLUSIVE MODE;

-- PRD #4416 M2: delivery receipts, the variable identity table, the version-2 AAD
-- discriminator and the lead-lane predicate.
--
-- run_env_receipts: one row per (run, variable version) a claim delivered, plain and
-- secret alike (the run page names what ran). Retention, the retire/pending-delete
-- decisions and the finish gate read the SECRET receipts only (the partial index).
-- There is no foreign key to run_env_vars, and no value or hash is ever stored.
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
-- Looks receipts up by var_id, plain and secret alike, for the sweeper's orphan-identity checks.
CREATE INDEX run_env_receipts_var_idx ON run_env_receipts (var_id);

-- run_env_var_ids: the identity of a variable. A var_id is bound to one (user, repo, scope,
-- name) while its identity row exists; the sweep deletes an identity that no run_env_vars row
-- and no receipt references. repo_id has no foreign key.
CREATE TABLE run_env_var_ids (
    var_id  uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id uuid,
    scope   text NOT NULL CHECK (scope IN ('repo', 'user')),
    name    text NOT NULL
);
-- The sweeper's orphan-identity scan and the user cascade read by owner.
CREATE INDEX run_env_var_ids_user_idx ON run_env_var_ids (user_id);

-- Fails when a var_id already names more than one (user, repo, name).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM run_env_vars GROUP BY var_id
        HAVING count(DISTINCT user_id) > 1
            OR count(DISTINCT name) > 1
            OR count(DISTINCT repo_id) > 1
            OR (count(*) FILTER (WHERE repo_id IS NULL) > 0 AND count(repo_id) > 0)
    ) THEN
        RAISE EXCEPTION 'run_env_vars: a var_id maps to more than one (user_id, repo_id, name); resolve the rows before this migration';
    END IF;
END
$$;
-- +goose StatementEnd

INSERT INTO run_env_var_ids (var_id, user_id, repo_id, scope, name)
SELECT DISTINCT ON (var_id)
       var_id, user_id, repo_id, CASE WHEN repo_id IS NULL THEN 'user' ELSE 'repo' END, name
FROM run_env_vars
ORDER BY var_id, rev DESC;

-- The identity guard. On INSERT the identity row is created if absent and the stored
-- (user, repo, scope, name) must equal the new row's, which refuses a reused var_id
-- under another name, repo, scope or owner while the identity row exists. On
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
-- (scope, repo, secret flag, name), 2 adds the owner, var_id and rev. Existing rows stay 1; every new seal writes 2.
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

-- fn_run_env_run_held: whether a run is still held, so a secret receipt of it keeps the version it
-- names sealed. Held is: not terminal, or terminal for less than 24 hours, or holding recovery
-- custody in any form (an open custody hold, a live capture, a live held-work publication, a
-- terminal preservation that may own a ref, pending or promoted salvage, a prerequisite closure
-- that may own a pin, an active publication-resume enrollment, a checkpoint retention that still
-- owes a delete, or a checkpoint push whose outcome is not accounted for). The retired-version
-- sweeper and the history-full answer both read this one definition.
-- +goose StatementBegin
CREATE FUNCTION fn_run_env_run_held(p_run_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM runs r WHERE r.id = p_run_id
                     AND (r.status NOT IN ('completed', 'failed', 'cancelled')
                          OR COALESCE(r.finished_at, r.updated_at) > now() - interval '24 hours'))
        OR EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = p_run_id AND h.state = 'open')
        OR EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = p_run_id AND c.state NOT IN ('expired', 'discarded'))
        OR EXISTS (SELECT 1 FROM run_held_publications p WHERE p.run_id = p_run_id AND p.state NOT IN ('refused', 'deleted', 'abandoned'))
        OR EXISTS (SELECT 1 FROM run_terminal_preservations t WHERE t.run_id = p_run_id AND t.state IN ('bound', 'publishing', 'live', 'retiring'))
        OR EXISTS (SELECT 1 FROM run_salvage sv WHERE sv.run_id = p_run_id AND sv.state IN ('pending', 'promoted'))
        OR EXISTS (SELECT 1 FROM recovery_prerequisite_closures pc WHERE pc.run_id = p_run_id AND pc.state NOT IN ('refused', 'foreign', 'lost', 'deleted'))
        OR EXISTS (SELECT 1 FROM run_publication_resume pr WHERE pr.run_id = p_run_id AND pr.state = 'active')
        OR EXISTS (SELECT 1 FROM checkpoint_retentions cr WHERE cr.run_id = p_run_id AND cr.state IN ('retained', 'superseding', 'superseded', 'settling'))
        OR EXISTS (SELECT 1 FROM checkpoint_publish_attempts a WHERE a.run_id = p_run_id)
$$;
-- +goose StatementEnd

-- fn_run_env_publication_pending: whether a run still has recovery custody or a publication in
-- flight: an open custody hold, a capture that is preparing or uploading, a held-work publication
-- that is prepared, invoked or of unknown outcome, or a terminal preservation that is bound or
-- publishing. A settled state (a capture that is available or needs action, a created held
-- publication, a live terminal preservation) does not hold. The run-env redact route stays open
-- for a terminal run while this holds.
-- +goose StatementBegin
CREATE FUNCTION fn_run_env_publication_pending(p_run_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = p_run_id AND h.state = 'open')
        OR EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = p_run_id AND c.state IN ('preparing', 'uploading'))
        OR EXISTS (SELECT 1 FROM run_held_publications p WHERE p.run_id = p_run_id AND p.state IN ('prepared', 'invoked', 'create_unknown'))
        OR EXISTS (SELECT 1 FROM run_terminal_preservations t WHERE t.run_id = p_run_id AND t.state IN ('bound', 'publishing'))
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Fails when any version-2 row or receipt exists.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM run_env_vars WHERE aad_version = 2) OR EXISTS (SELECT 1 FROM run_env_receipts) THEN
        RAISE EXCEPTION 'cannot roll back run_env_receipts: version-2 sealed variables or delivery receipts exist, and removing them would leave values unopenable or unmasked';
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION fn_run_env_publication_pending(uuid);
DROP FUNCTION fn_run_env_run_held(uuid);
DROP FUNCTION fn_run_env_lane(text, uuid, uuid);
DROP INDEX run_env_vars_sweep_idx;
ALTER TABLE run_env_vars
    DROP CONSTRAINT run_env_vars_aad_version_check,
    DROP COLUMN aad_version;
DROP TRIGGER run_env_vars_identity_guard ON run_env_vars;
DROP FUNCTION run_env_vars_identity_guard();
DROP INDEX run_env_var_ids_user_idx;
DROP TABLE run_env_var_ids;
DROP TABLE run_env_receipts;
