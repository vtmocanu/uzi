-- +goose Up
-- One server-owned current-generation tuple, written only by ClaimRun from its
-- admitted target snapshot. A trigger query obtains a fresh SQL snapshot and
-- cannot itself reconstruct which origin the outer statement admitted.
ALTER TABLE runs ADD COLUMN forge_admission jsonb;
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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forge_origin_claim_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id IS NULL OR NEW.repo_id IS NULL OR NEW.claim_generation <= 0 THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.worker_id, NEW.claim_generation) IS NOT DISTINCT FROM
           (OLD.worker_id, OLD.claim_generation) THEN RETURN NEW; END IF;
    END IF;
    IF NEW.status NOT IN ('claimed', 'running') THEN RETURN NEW; END IF;
    DELETE FROM run_forge_origins WHERE run_id = NEW.id;
    INSERT INTO run_forge_origins
        (run_id,user_id,worker_id,generation,connection_id,origin,reservation_incarnation,reservation_version)
    SELECT NEW.id,NEW.user_id,NEW.worker_id,NEW.claim_generation,a.connection_id,a.origin,
           c.reservation_incarnation,c.reservation_version
    FROM authorized_forge_origins a JOIN workers w ON w.id=NEW.worker_id AND w.user_id=NEW.user_id
    LEFT JOIN forge_origin_cooldowns c
      ON c.user_id=NEW.user_id AND c.worker_id=NEW.worker_id AND c.origin=a.origin
        AND c.reservation_run_id=NEW.id AND c.reservation_generation=NEW.claim_generation
        AND c.reservation_incarnation=c.incarnation
    WHERE a.repo_id=NEW.repo_id AND a.user_id=NEW.user_id;
    RETURN NEW;
END $$;
-- +goose StatementEnd
ALTER TABLE runs DROP COLUMN forge_admission;
