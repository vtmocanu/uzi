-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #4476: Codex claim provenance. A Codex-indicating run that has been claimed at least once
-- (claim_generation >= 1) may be claimed again only by the worker recorded as its last successful
-- Codex claimant for the CURRENT generation. The pair (worker, generation) is nullable with no
-- default, no FK and no backfill: a legacy row has NULL provenance and is therefore held.
ALTER TABLE runs
    ADD COLUMN claim_provenance_worker_id uuid,
    ADD COLUMN claim_provenance_generation bigint;

-- The admission predicate shared by ClaimRun and its mirrors. A plain $$ string body (not
-- BEGIN ATOMIC, no argument defaults): the fresh-schema rule in the intake-pause rowtype test
-- rejects parsed bodies. claimant NULL models a worker that does not exist yet (a new ephemeral
-- worker), which is never admitted for a held run.
-- +goose StatementBegin
CREATE FUNCTION fn_codex_claim_provenance_admits(r runs, claimant uuid)
RETURNS boolean LANGUAGE sql STABLE PARALLEL SAFE AS $$
SELECT NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
    OR r.claim_generation < 1
    OR COALESCE(r.claim_provenance_worker_id = claimant
                AND r.claim_provenance_generation = r.claim_generation, false)
$$;
-- +goose StatementEnd

-- Provenance changes only through an admitted claimant's +1 claim (ClaimRun sets worker_id,
-- claim_generation and the pair in one UPDATE). Any write not shaped like an admitted +1 claim is
-- rejected here, so no park, Resume, promoter, account-unavailable, delete or backfill path can
-- write it.
-- +goose StatementBegin
CREATE FUNCTION runs_claim_provenance_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.claim_provenance_worker_id IS NOT NULL OR NEW.claim_provenance_generation IS NOT NULL THEN
            RAISE EXCEPTION 'claim provenance cannot be set on insert' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    -- IS NOT TRUE, not NOT (...): a NULL conjunct (e.g. NEW.worker_id NULL) must raise too.
    IF (
        NEW.claim_generation = OLD.claim_generation + 1
        AND ((OLD.claim_generation < 1
              AND OLD.claim_provenance_worker_id IS NULL AND OLD.claim_provenance_generation IS NULL)
             OR (OLD.claim_generation >= 1
                 AND COALESCE(OLD.claim_provenance_worker_id = NEW.worker_id
                         AND OLD.claim_provenance_generation = OLD.claim_generation, false))
            )
        AND (OLD.claim_provenance_worker_id IS NULL) = (OLD.claim_provenance_generation IS NULL)
        AND (NEW.claim_provenance_worker_id IS NULL) = (NEW.claim_provenance_generation IS NULL)
        AND NEW.claim_provenance_worker_id IS NOT NULL
        AND NEW.claim_provenance_worker_id = NEW.worker_id
        AND NEW.claim_provenance_generation = NEW.claim_generation
    ) IS NOT TRUE THEN
        RAISE EXCEPTION 'claim provenance may change only through an admitted claim of run %', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_runs_claim_provenance_guard BEFORE UPDATE ON runs
    FOR EACH ROW
    WHEN (OLD.claim_provenance_worker_id IS DISTINCT FROM NEW.claim_provenance_worker_id
          OR OLD.claim_provenance_generation IS DISTINCT FROM NEW.claim_provenance_generation)
    EXECUTE FUNCTION runs_claim_provenance_guard();

CREATE TRIGGER trg_runs_claim_provenance_guard_insert BEFORE INSERT ON runs
    FOR EACH ROW
    WHEN (NEW.claim_provenance_worker_id IS NOT NULL OR NEW.claim_provenance_generation IS NOT NULL)
    EXECUTE FUNCTION runs_claim_provenance_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS trg_runs_claim_provenance_guard_insert ON runs;
DROP TRIGGER IF EXISTS trg_runs_claim_provenance_guard ON runs;
DROP FUNCTION IF EXISTS runs_claim_provenance_guard();
DROP FUNCTION IF EXISTS fn_codex_claim_provenance_admits(runs, uuid);
ALTER TABLE runs
    DROP COLUMN IF EXISTS claim_provenance_generation,
    DROP COLUMN IF EXISTS claim_provenance_worker_id;
