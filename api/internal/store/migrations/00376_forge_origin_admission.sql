-- +goose Up
-- Advisory snapshot only. Mirrors may use this predicate without taking locks.
-- The claimant must still conditionally reserve an existing due row under its
-- worker and target-run locks. Absence never takes an origin or synthetic lock.
-- +goose StatementBegin
CREATE FUNCTION fn_forge_origin_eligible(w workers, r runs, evaluated_at timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT w.user_id = r.user_id AND (
        r.repo_id IS NULL OR r.kind = 'chat' OR r.egress_profile_id IS NOT NULL
        OR EXISTS (
            SELECT 1 FROM authorized_forge_origins a
            WHERE a.repo_id = r.repo_id AND a.user_id = r.user_id
              AND a.origin IS NOT NULL
              AND NOT EXISTS (
                  SELECT 1 FROM forge_origin_cooldowns c
                  WHERE c.user_id = a.user_id AND c.worker_id = w.id AND c.origin = a.origin
                    AND (c.cooled_until > COALESCE(evaluated_at, statement_timestamp()) OR c.reservation_run_id IS NOT NULL)
              )
        )
    )
$$;
-- +goose StatementEnd

-- Cleanup's trigger sorts before z_forge_origin_claim_stamp, so generation
-- replacement still sees OLD's exact reservation metadata. Stamp only after
-- the claim generation advances, including the reservation made by ClaimRun.
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
        (run_id, user_id, worker_id, generation, connection_id, origin,
         reservation_incarnation, reservation_version)
    SELECT NEW.id, NEW.user_id, NEW.worker_id, NEW.claim_generation, a.connection_id, a.origin,
           c.reservation_incarnation, c.reservation_version
    FROM authorized_forge_origins a
    JOIN workers w ON w.id = NEW.worker_id AND w.user_id = NEW.user_id
    LEFT JOIN forge_origin_cooldowns c
      ON c.user_id = NEW.user_id AND c.worker_id = NEW.worker_id AND c.origin = a.origin
        AND c.reservation_run_id = NEW.id AND c.reservation_generation = NEW.claim_generation
        AND c.reservation_incarnation = c.incarnation
    WHERE a.repo_id = NEW.repo_id AND a.user_id = NEW.user_id;
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
    INSERT INTO run_forge_origins (run_id, user_id, worker_id, generation, connection_id, origin)
    SELECT NEW.id, NEW.user_id, NEW.worker_id, NEW.claim_generation, a.connection_id, a.origin
    FROM authorized_forge_origins a JOIN workers w ON w.id = NEW.worker_id AND w.user_id = NEW.user_id
    WHERE a.repo_id = NEW.repo_id AND a.user_id = NEW.user_id;
    RETURN NEW;
END $$;
-- +goose StatementEnd
DROP FUNCTION fn_forge_origin_eligible(workers, runs, timestamptz);
