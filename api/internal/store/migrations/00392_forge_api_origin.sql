-- Direct publication/API failures feed the same advisory origin cooldown as clone/fetch failures
-- (issue #1324 D9). The API origin is derived by the server from the authorized repo mapping, never
-- from a worker-sent URL. Equal canonical API and Git origins share one cooldown row, and the two
-- evidence surfaces keep independent flags on it so one surface's success cannot clear the other.

-- +goose Up
-- The first statement: the ALTERs below wait on forge_origin_cooldowns and run_forge_origins locks.
-- A lock wait past the bound fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';
-- The authorized API origin of a repo, mirroring agent/src/forge.ts githubRepoParts / gitlabBaseUrl /
-- forgejoRepoParts. GitHub's API lives on a different host (api.github.com, or api.<host> with a
-- leading www. dropped, port kept); GitLab and Forgejo serve the API from the web host, so their
-- identity is the Git origin (a Forgejo subpath is not part of an origin). NULL when the web URL has
-- no canonical origin. fixtures/forge-api-origin/cases.json pins this against the TS derivation.
-- +goose StatementBegin
CREATE FUNCTION canonical_forge_api_origin(forge_type text, web_url text) RETURNS text
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE git_origin text; host text;
BEGIN
    git_origin := canonical_forge_origin(web_url || '.git');
    IF git_origin IS NULL OR forge_type IS DISTINCT FROM 'github' THEN RETURN git_origin; END IF;
    host := substr(git_origin, length('https://') + 1);
    IF host = 'github.com' THEN RETURN 'https://api.github.com'; END IF;
    RETURN canonical_forge_origin('https://api.' || regexp_replace(host, '^www\.', ''));
END $$;
-- +goose StatementEnd

-- One row per repo/connection stays true: the new column is appended last.
CREATE OR REPLACE VIEW authorized_forge_origins AS
SELECT rp.id AS repo_id, c.user_id, c.id AS connection_id,
       canonical_forge_origin(rp.web_url || '.git') AS origin,
       canonical_forge_api_origin(c.forge_type, rp.web_url) AS api_origin
FROM repos rp JOIN forge_connections c ON c.id = rp.connection_id;

-- Existing rows are Git evidence, so the Git flag defaults true. A row exists only while at least
-- one surface is unverified.
ALTER TABLE forge_origin_cooldowns
    ADD COLUMN git_unverified boolean NOT NULL DEFAULT true,
    ADD COLUMN api_unverified boolean NOT NULL DEFAULT false;
ALTER TABLE forge_origin_cooldowns DROP CONSTRAINT forge_origin_cooldowns_last_reason_check;
ALTER TABLE forge_origin_cooldowns ADD CONSTRAINT forge_origin_cooldowns_last_reason_check
    CHECK (last_reason IN ('transient_clone_fetch_exhausted', 'transient_publication_exhausted'));
ALTER TABLE forge_origin_cooldowns ADD CONSTRAINT forge_origin_cooldowns_unverified_check
    CHECK (git_unverified OR api_unverified);

-- The claim snapshot also records the API origin and the API reservation it captured. When the
-- API origin equals the Git origin the reservation columns copy the shared row's reservation.
ALTER TABLE run_forge_origins
    ADD COLUMN api_origin text,
    ADD COLUMN api_reservation_incarnation uuid,
    ADD COLUMN api_reservation_version bigint;
ALTER TABLE run_forge_origins ADD CONSTRAINT run_forge_origins_api_reservation_check
    CHECK ((api_reservation_incarnation IS NULL) = (api_reservation_version IS NULL));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forge_origin_claim_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    admitted_connection uuid;
    admitted_origin text;
    admitted_api text;
BEGIN
    IF NEW.worker_id IS NULL OR NEW.repo_id IS NULL OR NEW.claim_generation <= 0 THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM
           (OLD.worker_id, OLD.claim_generation) THEN RETURN NEW; END IF;
    END IF;
    IF NEW.status NOT IN ('claimed', 'running') THEN RETURN NEW; END IF;
    IF NEW.forge_admission->>'generation' = NEW.claim_generation::text
       AND NEW.forge_admission->>'worker_id' = NEW.worker_id::text THEN
        admitted_connection := (NEW.forge_admission->>'connection_id')::uuid;
        admitted_origin := NEW.forge_admission->>'origin';
        admitted_api := NEW.forge_admission->>'api_origin';
    ELSE
        -- Historical/non-claim writers retain M1's exact-generation snapshot.
        SELECT a.connection_id, a.origin, a.api_origin INTO admitted_connection, admitted_origin, admitted_api
        FROM authorized_forge_origins a WHERE a.repo_id = NEW.repo_id AND a.user_id = NEW.user_id;
    END IF;
    DELETE FROM run_forge_origins WHERE run_id = NEW.id;
    IF admitted_connection IS NULL THEN RETURN NEW; END IF;
    INSERT INTO run_forge_origins
        (run_id, user_id, worker_id, generation, connection_id, origin,
         reservation_incarnation, reservation_version,
         api_origin, api_reservation_incarnation, api_reservation_version)
    SELECT NEW.id, NEW.user_id, NEW.worker_id, NEW.claim_generation, admitted_connection, admitted_origin,
           c.reservation_incarnation, c.reservation_version,
           admitted_api,
           CASE WHEN admitted_api = admitted_origin THEN c.reservation_incarnation ELSE ca.reservation_incarnation END,
           CASE WHEN admitted_api = admitted_origin THEN c.reservation_version ELSE ca.reservation_version END
    FROM workers w LEFT JOIN forge_origin_cooldowns c
      ON c.user_id = NEW.user_id AND c.worker_id = NEW.worker_id AND c.origin = admitted_origin
        AND c.reservation_run_id = NEW.id AND c.reservation_generation = NEW.claim_generation
        AND c.reservation_incarnation = c.incarnation
    LEFT JOIN forge_origin_cooldowns ca
      ON ca.user_id = NEW.user_id AND ca.worker_id = NEW.worker_id AND ca.origin = admitted_api
        AND admitted_api <> admitted_origin
        AND ca.reservation_run_id = NEW.id AND ca.reservation_generation = NEW.claim_generation
        AND ca.reservation_incarnation = ca.incarnation
    WHERE w.id = NEW.worker_id AND w.user_id = NEW.user_id;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- Completion is Git evidence only: it clears the Git flag and deletes the row only when no API
-- failure remains. It never clears an API failure. Every reservation, API one included, is
-- released with the claim.
-- Lock this run's reserved rows in origin order, the order ClaimRun's forge_locked CTE takes them.
-- Only rows this run still reserves are locked (the reservation filter matches every later write),
-- because run_forge_origins survives a release and an unreserved row must not be locked by an
-- unrelated write to the run. The statements after it touch the Git row and then the API row;
-- without the ordered lock, a claim racing a release or completion deadlocks on
-- forge_origin_cooldowns (SQLSTATE 40P01).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forge_origin_reservation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        -- completed_publication_stamp runs first. Its admitted worker identity,
        -- not a later broker/forge receipt, is the direct-success evidence.
        IF OLD.status <> 'completed' AND NEW.status = 'completed'
           AND NEW.kind IN ('issue', 'mr_rework', 'self_improve')
           AND NEW.completion_final_head IS NOT NULL AND NEW.claim_released_at IS NULL
           AND (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM (OLD.worker_id, OLD.claim_generation) THEN
            PERFORM 1 FROM forge_origin_cooldowns c
            JOIN run_forge_origins m ON m.run_id = OLD.id AND m.worker_id = OLD.worker_id
                AND m.generation = OLD.claim_generation
            WHERE c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin IN (m.origin, m.api_origin)
              AND c.reservation_run_id = OLD.id AND c.reservation_generation = OLD.claim_generation
            ORDER BY c.origin
            FOR UPDATE OF c;
            DELETE FROM forge_origin_cooldowns c USING run_forge_origins m,
                recovery_custody_holds h, repos rp, forge_connections fc
            WHERE m.run_id = NEW.id AND m.user_id = NEW.user_id
              AND m.worker_id = NEW.worker_id AND m.generation = NEW.claim_generation
              AND rp.id = NEW.repo_id AND fc.id = rp.connection_id AND fc.user_id = NEW.user_id
              AND m.connection_id = fc.id AND m.origin = canonical_forge_origin(rp.web_url || '.git')
              AND c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
              AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
              AND c.incarnation = m.reservation_incarnation AND c.reservation_incarnation = m.reservation_incarnation
              AND c.reservation_version = m.reservation_version
              AND c.observation_version = c.reservation_observation_version
              AND c.git_unverified AND NOT c.api_unverified
              AND h.run_id = NEW.id AND h.user_id = NEW.user_id AND h.repo_id = NEW.repo_id
              AND h.original_worker_id = NEW.worker_id AND h.generation = NEW.claim_generation
              AND h.inventory_guarded AND h.state = 'open'
              AND h.live_worker_id = NEW.worker_id AND h.live_run_id = NEW.id
              AND rp.forge_project_id > 0 AND NEW.mr_iid > 0
              AND h.completion_identity = jsonb_build_object(
                'hold_id', h.id, 'run_id', NEW.id, 'owner_id', NEW.user_id,
                'worker_id', NEW.worker_id, 'generation', NEW.claim_generation,
                'final_head', NEW.completion_final_head, 'repo_id', rp.id,
                'connection_id', fc.id, 'project_id', rp.forge_project_id,
                'forge_type', fc.forge_type, 'base_url', fc.base_url,
                'branch', CASE WHEN NEW.kind = 'issue' THEN 'agent/issue-' || NEW.issue_iid::text
                    WHEN NEW.kind = 'self_improve' THEN 'uzi/self-improve/' || NEW.id::text ELSE NEW.pipeline_ref END,
                'mr_iid', NEW.mr_iid);
            UPDATE forge_origin_cooldowns c SET git_unverified = false
            FROM run_forge_origins m,
                recovery_custody_holds h, repos rp, forge_connections fc
            WHERE m.run_id = NEW.id AND m.user_id = NEW.user_id
              AND m.worker_id = NEW.worker_id AND m.generation = NEW.claim_generation
              AND rp.id = NEW.repo_id AND fc.id = rp.connection_id AND fc.user_id = NEW.user_id
              AND m.connection_id = fc.id AND m.origin = canonical_forge_origin(rp.web_url || '.git')
              AND c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
              AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
              AND c.incarnation = m.reservation_incarnation AND c.reservation_incarnation = m.reservation_incarnation
              AND c.reservation_version = m.reservation_version
              AND c.observation_version = c.reservation_observation_version
              AND c.git_unverified AND c.api_unverified
              AND h.run_id = NEW.id AND h.user_id = NEW.user_id AND h.repo_id = NEW.repo_id
              AND h.original_worker_id = NEW.worker_id AND h.generation = NEW.claim_generation
              AND h.inventory_guarded AND h.state = 'open'
              AND h.live_worker_id = NEW.worker_id AND h.live_run_id = NEW.id
              AND rp.forge_project_id > 0 AND NEW.mr_iid > 0
              AND h.completion_identity = jsonb_build_object(
                'hold_id', h.id, 'run_id', NEW.id, 'owner_id', NEW.user_id,
                'worker_id', NEW.worker_id, 'generation', NEW.claim_generation,
                'final_head', NEW.completion_final_head, 'repo_id', rp.id,
                'connection_id', fc.id, 'project_id', rp.forge_project_id,
                'forge_type', fc.forge_type, 'base_url', fc.base_url,
                'branch', CASE WHEN NEW.kind = 'issue' THEN 'agent/issue-' || NEW.issue_iid::text
                    WHEN NEW.kind = 'self_improve' THEN 'uzi/self-improve/' || NEW.id::text ELSE NEW.pipeline_ref END,
                'mr_iid', NEW.mr_iid);
        END IF;
        IF NEW.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
           AND NEW.claim_released_at IS NULL AND NEW.stop_kind IS NULL
           AND (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM (OLD.worker_id, OLD.claim_generation)
           THEN RETURN NEW; END IF;
    END IF;
    PERFORM 1 FROM forge_origin_cooldowns c
    JOIN run_forge_origins m ON m.run_id = OLD.id AND m.worker_id = OLD.worker_id
        AND m.generation = OLD.claim_generation
    WHERE c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin IN (m.origin, m.api_origin)
      AND c.reservation_run_id = OLD.id AND c.reservation_generation = OLD.claim_generation
    ORDER BY c.origin
    FOR UPDATE OF c;
    UPDATE forge_origin_cooldowns c SET
        reservation_run_id = NULL, reservation_generation = NULL,
        reservation_incarnation = NULL, reservation_version = NULL,
        reservation_observation_version = NULL
    FROM run_forge_origins m
    WHERE m.run_id = OLD.id AND m.worker_id = OLD.worker_id AND m.generation = OLD.claim_generation
      AND c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
      AND c.reservation_run_id = OLD.id AND c.reservation_generation = OLD.claim_generation
      AND c.reservation_incarnation = m.reservation_incarnation
      AND c.reservation_version = m.reservation_version;
    UPDATE forge_origin_cooldowns c SET
        reservation_run_id = NULL, reservation_generation = NULL,
        reservation_incarnation = NULL, reservation_version = NULL,
        reservation_observation_version = NULL
    FROM run_forge_origins m
    WHERE m.run_id = OLD.id AND m.worker_id = OLD.worker_id AND m.generation = OLD.claim_generation
      AND m.api_origin IS NOT NULL AND m.api_origin <> m.origin
      AND c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.api_origin
      AND c.reservation_run_id = OLD.id AND c.reservation_generation = OLD.claim_generation
      AND c.reservation_incarnation = m.api_reservation_incarnation
      AND c.reservation_version = m.api_reservation_version;
    RETURN OLD;
END $$;
-- +goose StatementEnd

-- Repo/connection lifecycle prunes keys that no authorized Git or API origin names any more.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prune_forge_origin_cooldowns() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Lock the doomed rows in ClaimRun's forge_locked order first: a set-based DELETE locks in scan
    -- order and can deadlock with a claim (SQLSTATE 40P01).
    PERFORM 1 FROM forge_origin_cooldowns c
    WHERE NOT EXISTS (SELECT 1 FROM authorized_forge_origins a
                      WHERE a.user_id = c.user_id AND (a.origin = c.origin OR a.api_origin = c.origin))
    ORDER BY c.user_id, c.worker_id, c.origin
    FOR UPDATE OF c;
    DELETE FROM forge_origin_cooldowns c
    WHERE NOT EXISTS (SELECT 1 FROM authorized_forge_origins a
                      WHERE a.user_id = c.user_id AND (a.origin = c.origin OR a.api_origin = c.origin));
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- A worker is ineligible while either the Git or the API origin of the run is cooled or reserved.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_forge_origin_eligible(w workers, r runs, evaluated_at timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT w.user_id = r.user_id AND (
        r.repo_id IS NULL OR r.kind = 'chat' OR r.egress_profile_id IS NOT NULL
        -- A code checker reads the lead's local snapshot and never contacts the forge.
        OR (r.kind = 'cross_check' AND EXISTS (
            SELECT 1 FROM cross_checks cc
            WHERE cc.lead_run_id = r.target_run_id AND cc.stage = 'code' AND cc.checker_run_id = r.id))
        -- Only a cooled or reserved authorized origin excludes this worker. A run with
        -- no usable mapping (unmappable URL, owner mismatch) stays claimable so the
        -- claim snapshot check refuses it visibly instead of queueing it everywhere.
        OR NOT EXISTS (
            SELECT 1 FROM authorized_forge_origins a
            JOIN forge_origin_cooldowns c
              ON c.user_id = a.user_id AND c.worker_id = w.id AND c.origin IN (a.origin, a.api_origin)
            WHERE a.repo_id = r.repo_id AND a.user_id = r.user_id
              AND (c.cooled_until > COALESCE(evaluated_at, statement_timestamp()) OR c.reservation_run_id IS NOT NULL)
        )
    )
$$;
-- +goose StatementEnd

-- fn_forge_origin_owner_pins is deliberately unchanged: it keys the Git origin only, so an API-only
-- row never releases an owner's source pin.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_forge_origin_provision_allowed(r runs, evaluated_at timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
SELECT r.repo_id IS NULL OR r.kind = 'chat' OR r.egress_profile_id IS NOT NULL
    OR NOT EXISTS (
        SELECT 1 FROM authorized_forge_origins a
        JOIN forge_origin_cooldowns c ON c.user_id = a.user_id AND c.origin IN (a.origin, a.api_origin)
        JOIN workers w ON w.id = c.worker_id AND w.user_id = a.user_id
        WHERE a.repo_id = r.repo_id AND a.user_id = r.user_id
          AND NOT fn_forge_origin_eligible(w,r,evaluated_at)
    )
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Restore 00380 to 00385 behaviour. API-only evidence has no representation there and is dropped.
UPDATE forge_origin_cooldowns SET last_reason = 'transient_clone_fetch_exhausted'
WHERE last_reason = 'transient_publication_exhausted';
DELETE FROM forge_origin_cooldowns WHERE api_unverified AND NOT git_unverified;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_forge_origin_provision_allowed(r runs, evaluated_at timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
SELECT r.repo_id IS NULL OR r.kind = 'chat' OR r.egress_profile_id IS NOT NULL
    OR NOT EXISTS (
        SELECT 1 FROM authorized_forge_origins a
        JOIN forge_origin_cooldowns c ON c.user_id = a.user_id AND c.origin = a.origin
        JOIN workers w ON w.id = c.worker_id AND w.user_id = a.user_id
        WHERE a.repo_id = r.repo_id AND a.user_id = r.user_id
          AND NOT fn_forge_origin_eligible(w,r,evaluated_at)
    )
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_forge_origin_eligible(w workers, r runs, evaluated_at timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT w.user_id = r.user_id AND (
        r.repo_id IS NULL OR r.kind = 'chat' OR r.egress_profile_id IS NOT NULL
        -- A code checker reads the lead's local snapshot and never contacts the forge.
        OR (r.kind = 'cross_check' AND EXISTS (
            SELECT 1 FROM cross_checks cc
            WHERE cc.lead_run_id = r.target_run_id AND cc.stage = 'code' AND cc.checker_run_id = r.id))
        -- Only a cooled or reserved authorized origin excludes this worker. A run with
        -- no usable mapping (unmappable URL, owner mismatch) stays claimable so the
        -- claim snapshot check refuses it visibly instead of queueing it everywhere.
        OR NOT EXISTS (
            SELECT 1 FROM authorized_forge_origins a
            JOIN forge_origin_cooldowns c
              ON c.user_id = a.user_id AND c.worker_id = w.id AND c.origin = a.origin
            WHERE a.repo_id = r.repo_id AND a.user_id = r.user_id
              AND (c.cooled_until > COALESCE(evaluated_at, statement_timestamp()) OR c.reservation_run_id IS NOT NULL)
        )
    )
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prune_forge_origin_cooldowns() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM forge_origin_cooldowns c
    WHERE NOT EXISTS (SELECT 1 FROM authorized_forge_origins a
                      WHERE a.user_id = c.user_id AND a.origin = c.origin);
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forge_origin_reservation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        -- completed_publication_stamp runs first. Its admitted worker identity,
        -- not a later broker/forge receipt, is the direct-success evidence.
        IF OLD.status <> 'completed' AND NEW.status = 'completed'
           AND NEW.kind IN ('issue', 'mr_rework', 'self_improve')
           AND NEW.completion_final_head IS NOT NULL AND NEW.claim_released_at IS NULL
           AND (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM (OLD.worker_id, OLD.claim_generation) THEN
            DELETE FROM forge_origin_cooldowns c USING run_forge_origins m,
                recovery_custody_holds h, repos rp, forge_connections fc
            WHERE m.run_id = NEW.id AND m.user_id = NEW.user_id
              AND m.worker_id = NEW.worker_id AND m.generation = NEW.claim_generation
              AND rp.id = NEW.repo_id AND fc.id = rp.connection_id AND fc.user_id = NEW.user_id
              AND m.connection_id = fc.id AND m.origin = canonical_forge_origin(rp.web_url || '.git')
              AND c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
              AND c.reservation_run_id = m.run_id AND c.reservation_generation = m.generation
              AND c.incarnation = m.reservation_incarnation AND c.reservation_incarnation = m.reservation_incarnation
              AND c.reservation_version = m.reservation_version
              AND c.observation_version = c.reservation_observation_version
              AND h.run_id = NEW.id AND h.user_id = NEW.user_id AND h.repo_id = NEW.repo_id
              AND h.original_worker_id = NEW.worker_id AND h.generation = NEW.claim_generation
              AND h.inventory_guarded AND h.state = 'open'
              AND h.live_worker_id = NEW.worker_id AND h.live_run_id = NEW.id
              AND rp.forge_project_id > 0 AND NEW.mr_iid > 0
              AND h.completion_identity = jsonb_build_object(
                'hold_id', h.id, 'run_id', NEW.id, 'owner_id', NEW.user_id,
                'worker_id', NEW.worker_id, 'generation', NEW.claim_generation,
                'final_head', NEW.completion_final_head, 'repo_id', rp.id,
                'connection_id', fc.id, 'project_id', rp.forge_project_id,
                'forge_type', fc.forge_type, 'base_url', fc.base_url,
                'branch', CASE WHEN NEW.kind = 'issue' THEN 'agent/issue-' || NEW.issue_iid::text
                    WHEN NEW.kind = 'self_improve' THEN 'uzi/self-improve/' || NEW.id::text ELSE NEW.pipeline_ref END,
                'mr_iid', NEW.mr_iid);
        END IF;
        IF NEW.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
           AND NEW.claim_released_at IS NULL AND NEW.stop_kind IS NULL
           AND (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM (OLD.worker_id, OLD.claim_generation)
           THEN RETURN NEW; END IF;
    END IF;
    UPDATE forge_origin_cooldowns c SET
        reservation_run_id = NULL, reservation_generation = NULL,
        reservation_incarnation = NULL, reservation_version = NULL,
        reservation_observation_version = NULL
    FROM run_forge_origins m
    WHERE m.run_id = OLD.id AND m.worker_id = OLD.worker_id AND m.generation = OLD.claim_generation
      AND c.user_id = m.user_id AND c.worker_id = m.worker_id AND c.origin = m.origin
      AND c.reservation_run_id = OLD.id AND c.reservation_generation = OLD.claim_generation
      AND c.reservation_incarnation = m.reservation_incarnation
      AND c.reservation_version = m.reservation_version;
    RETURN OLD;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forge_origin_claim_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    admitted_connection uuid;
    admitted_origin text;
BEGIN
    IF NEW.worker_id IS NULL OR NEW.repo_id IS NULL OR NEW.claim_generation <= 0 THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM
           (OLD.worker_id, OLD.claim_generation) THEN RETURN NEW; END IF;
    END IF;
    IF NEW.status NOT IN ('claimed', 'running') THEN RETURN NEW; END IF;
    IF NEW.forge_admission->>'generation' = NEW.claim_generation::text
       AND NEW.forge_admission->>'worker_id' = NEW.worker_id::text THEN
        admitted_connection := (NEW.forge_admission->>'connection_id')::uuid;
        admitted_origin := NEW.forge_admission->>'origin';
    ELSE
        -- Historical/non-claim writers retain M1's exact-generation snapshot.
        SELECT a.connection_id, a.origin INTO admitted_connection, admitted_origin
        FROM authorized_forge_origins a WHERE a.repo_id = NEW.repo_id AND a.user_id = NEW.user_id;
    END IF;
    DELETE FROM run_forge_origins WHERE run_id = NEW.id;
    IF admitted_connection IS NULL THEN RETURN NEW; END IF;
    INSERT INTO run_forge_origins
        (run_id, user_id, worker_id, generation, connection_id, origin,
         reservation_incarnation, reservation_version)
    SELECT NEW.id, NEW.user_id, NEW.worker_id, NEW.claim_generation, admitted_connection, admitted_origin,
           c.reservation_incarnation, c.reservation_version
    FROM workers w LEFT JOIN forge_origin_cooldowns c
      ON c.user_id = NEW.user_id AND c.worker_id = NEW.worker_id AND c.origin = admitted_origin
        AND c.reservation_run_id = NEW.id AND c.reservation_generation = NEW.claim_generation
        AND c.reservation_incarnation = c.incarnation
    WHERE w.id = NEW.worker_id AND w.user_id = NEW.user_id;
    RETURN NEW;
END $$;
-- +goose StatementEnd

ALTER TABLE run_forge_origins
    DROP CONSTRAINT run_forge_origins_api_reservation_check,
    DROP COLUMN api_reservation_version,
    DROP COLUMN api_reservation_incarnation,
    DROP COLUMN api_origin;
ALTER TABLE forge_origin_cooldowns DROP CONSTRAINT forge_origin_cooldowns_unverified_check;
ALTER TABLE forge_origin_cooldowns DROP CONSTRAINT forge_origin_cooldowns_last_reason_check;
ALTER TABLE forge_origin_cooldowns ADD CONSTRAINT forge_origin_cooldowns_last_reason_check
    CHECK (last_reason = 'transient_clone_fetch_exhausted');
ALTER TABLE forge_origin_cooldowns DROP COLUMN api_unverified, DROP COLUMN git_unverified;
-- A view cannot lose a column through CREATE OR REPLACE.
DROP VIEW authorized_forge_origins;
CREATE VIEW authorized_forge_origins AS
SELECT rp.id AS repo_id, c.user_id, c.id AS connection_id,
       canonical_forge_origin(rp.web_url || '.git') AS origin
FROM repos rp JOIN forge_connections c ON c.id = rp.connection_id;
DROP FUNCTION canonical_forge_api_origin(text, text);
