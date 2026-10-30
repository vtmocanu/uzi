-- +goose Up
-- Identity only: this is not an endpoint or allowlist authority.
-- +goose StatementBegin
CREATE FUNCTION canonical_forge_origin(raw text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE authority text; host text; port text; parts text[];
BEGIN
    IF raw ~ '[[:space:][:cntrl:]\\]' THEN RETURN NULL; END IF;
    parts := regexp_match(raw, '(?i)^https://([^/?#]+)');
    IF parts IS NULL THEN RETURN NULL; END IF;
    authority := regexp_replace(parts[1], '^.*@', '');
    IF authority LIKE '[%' THEN
        parts := regexp_match(authority, '^\[([0-9A-Fa-f:.]+)\](:([0-9]+))?$');
        IF parts IS NULL THEN RETURN NULL; END IF;
        BEGIN
            IF family(parts[1]::inet) <> 6 THEN RETURN NULL; END IF;
        EXCEPTION WHEN invalid_text_representation THEN RETURN NULL;
        END;
        host := '[' || host(parts[1]::inet) || ']';
        port := parts[3];
    ELSE
        parts := regexp_match(authority, '^([A-Za-z0-9._-]+)(:([0-9]+))?$');
        IF parts IS NULL THEN RETURN NULL; END IF;
        host := lower(regexp_replace(parts[1], '\.$', ''));
        IF host = '' OR host LIKE '.%' OR host LIKE '%..%' THEN RETURN NULL; END IF;
        port := parts[3];
    END IF;
    IF port IS NOT NULL THEN
        port := ltrim(port, '0');
        IF port = '' OR length(port) > 5 THEN RETURN NULL; END IF;
        IF port::integer > 65535 THEN RETURN NULL; END IF;
        port := (port::integer)::text;
        -- Cooldown identity groups numeric aliases; outbound authority is unchanged.
        IF port = '443' THEN port := NULL; END IF;
    END IF;
    RETURN 'https://' || host || CASE WHEN port IS NULL THEN '' ELSE ':' || port END;
END $$;
-- +goose StatementEnd

CREATE VIEW authorized_forge_origins AS
SELECT rp.id AS repo_id, c.user_id, c.id AS connection_id,
       canonical_forge_origin(rp.web_url || '.git') AS origin
FROM repos rp JOIN forge_connections c ON c.id = rp.connection_id;

ALTER TABLE workers ADD CONSTRAINT workers_forge_origin_owner_key UNIQUE (user_id, id);

CREATE TABLE forge_origin_cooldowns (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    worker_id uuid NOT NULL,
    origin text NOT NULL CHECK (canonical_forge_origin(origin) IS NOT NULL AND canonical_forge_origin(origin) = origin),
    incarnation uuid NOT NULL DEFAULT gen_random_uuid(),
    observation_version bigint NOT NULL DEFAULT 1 CHECK (observation_version > 0),
    failure_step integer NOT NULL DEFAULT 0 CHECK (failure_step BETWEEN 0 AND 30),
    cooled_until timestamptz NOT NULL,
    last_reason text NOT NULL CHECK (last_reason = 'transient_clone_fetch_exhausted'),
    observed_at timestamptz NOT NULL,
    reservation_run_id uuid,
    reservation_generation bigint,
    reservation_incarnation uuid,
    reservation_version bigint,
    reservation_observation_version bigint,
    PRIMARY KEY (user_id, worker_id, origin),
    FOREIGN KEY (user_id, worker_id) REFERENCES workers(user_id, id) ON DELETE CASCADE,
    CHECK (reservation_incarnation IS NULL OR reservation_incarnation = incarnation),
    CHECK ((reservation_run_id IS NULL AND reservation_generation IS NULL
            AND reservation_incarnation IS NULL AND reservation_version IS NULL
            AND reservation_observation_version IS NULL)
        OR (reservation_run_id IS NOT NULL AND reservation_generation IS NOT NULL
            AND reservation_incarnation IS NOT NULL AND reservation_version IS NOT NULL
            AND reservation_observation_version IS NOT NULL))
);

-- One current claim snapshot, not a claim/event history. Reservation binding is
-- filled by M2 while holding claimant worker and run locks, before cooldown locks.
CREATE TABLE run_forge_origins (
    run_id uuid PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    worker_id uuid NOT NULL,
    generation bigint NOT NULL,
    connection_id uuid NOT NULL,
    origin text,
    reservation_incarnation uuid,
    reservation_version bigint,
    FOREIGN KEY (user_id, worker_id) REFERENCES workers(user_id, id) ON DELETE CASCADE,
    CHECK ((reservation_incarnation IS NULL) = (reservation_version IS NULL))
);

-- +goose StatementBegin
CREATE FUNCTION forge_origin_claim_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id IS NULL OR NEW.repo_id IS NULL OR NEW.claim_generation <= 0 THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM
           (OLD.worker_id, OLD.claim_generation) THEN RETURN NEW; END IF;
    END IF;
    IF NEW.status NOT IN ('claimed', 'running') THEN RETURN NEW; END IF;
    DELETE FROM run_forge_origins WHERE run_id = NEW.id;
    INSERT INTO run_forge_origins (run_id, user_id, worker_id, generation, connection_id, origin)
    SELECT NEW.id, NEW.user_id, NEW.worker_id, NEW.claim_generation, a.connection_id, a.origin
    FROM authorized_forge_origins a JOIN workers w ON w.id = NEW.worker_id AND w.user_id = NEW.user_id
    WHERE a.repo_id = NEW.repo_id AND a.user_id = NEW.user_id;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- Runs are already locked by their writers. Never acquire a run lock from cooldown.
-- Alphabetically after completed_publication_stamp; M4 adds direct-success clearing here.
-- +goose StatementBegin
CREATE FUNCTION forge_origin_reservation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
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
-- BEFORE DELETE keeps the claim snapshot available before FK cascade deletion.
CREATE TRIGGER forge_origin_reservation_delete BEFORE DELETE ON runs
FOR EACH ROW EXECUTE FUNCTION forge_origin_reservation_lifecycle();
CREATE TRIGGER forge_origin_reservation_lifecycle AFTER UPDATE ON runs
FOR EACH ROW EXECUTE FUNCTION forge_origin_reservation_lifecycle();
-- z sorts after reservation cleanup so generation replacement still sees OLD metadata.
CREATE TRIGGER z_forge_origin_claim_stamp AFTER INSERT OR UPDATE ON runs
FOR EACH ROW EXECUTE FUNCTION forge_origin_claim_stamp();

-- Repo/connection lifecycle alone prunes obsolete keys; expiry never deletes state.
-- Each invocation makes one bounded SQL deletion over current keys; no retries.
-- +goose StatementBegin
CREATE FUNCTION prune_forge_origin_cooldowns() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM forge_origin_cooldowns c
    WHERE NOT EXISTS (SELECT 1 FROM authorized_forge_origins a
                      WHERE a.user_id = c.user_id AND a.origin = c.origin);
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER prune_forge_origin_repo AFTER DELETE OR UPDATE OF web_url, connection_id ON repos
FOR EACH STATEMENT EXECUTE FUNCTION prune_forge_origin_cooldowns();
CREATE TRIGGER prune_forge_origin_connection AFTER DELETE OR UPDATE OF user_id ON forge_connections
FOR EACH STATEMENT EXECUTE FUNCTION prune_forge_origin_cooldowns();

-- +goose Down
DROP TRIGGER prune_forge_origin_connection ON forge_connections;
DROP TRIGGER prune_forge_origin_repo ON repos;
DROP FUNCTION prune_forge_origin_cooldowns();
DROP TRIGGER z_forge_origin_claim_stamp ON runs;
DROP TRIGGER forge_origin_reservation_lifecycle ON runs;
DROP TRIGGER forge_origin_reservation_delete ON runs;
DROP FUNCTION forge_origin_reservation_lifecycle();
DROP FUNCTION forge_origin_claim_stamp();
DROP TABLE run_forge_origins;
DROP TABLE forge_origin_cooldowns;
ALTER TABLE workers DROP CONSTRAINT workers_forge_origin_owner_key;
DROP VIEW authorized_forge_origins;
DROP FUNCTION canonical_forge_origin(text);
