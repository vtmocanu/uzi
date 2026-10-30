-- Write-ahead publication intent (issue #1324). A run that parks on a forge outage during
-- publication keeps a server-side intent so the next claim resumes publication only. The intent
-- lives in a side table so runs gains a single column (recovery_wait_phase).

-- +goose Up
CREATE TABLE run_publication_resume (
    run_id           uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    enrollment_seq   int         NOT NULL CHECK (enrollment_seq > 0),
    claim_generation bigint      NOT NULL,
    worker_id        uuid,
    intent           jsonb       NOT NULL CHECK (octet_length(intent::text) <= 65536),
    candidate_seq    int         NOT NULL DEFAULT 0 CHECK (candidate_seq >= 0),
    state            text        NOT NULL CHECK (state IN ('active', 'retired', 'completed')),
    retired_reason   text,
    wait_started_at  timestamptz,
    first_wait_at    timestamptz,
    park_total       int         NOT NULL DEFAULT 0,
    episode          int         NOT NULL DEFAULT 0,
    hold_reason      text        CHECK (hold_reason IN (
        'publication_retry_exhausted', 'publication_model_required',
        'publication_source_unavailable', 'publication_intent_mismatch',
        'publication_custodian_unavailable')),
    handoff          jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, enrollment_seq)
);
CREATE UNIQUE INDEX run_publication_resume_active_idx ON run_publication_resume (run_id) WHERE state = 'active';

ALTER TABLE runs ADD COLUMN recovery_wait_phase text;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_phase_check
    CHECK (recovery_wait_phase IS NULL OR recovery_wait_phase = 'publication') NOT VALID;
ALTER TABLE runs VALIDATE CONSTRAINT runs_recovery_wait_phase_check;

-- Authority mirrors guard_checkpoint_coordinates (minus its enrollment leg): a run with an
-- active intent may change owner, generation or claim state only with BOTH the persisted worker
-- capability and the updated-API opt-in. The exception rolls back the surrounding savepoint.
-- +goose StatementBegin
CREATE FUNCTION guard_publication_resume() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    authority boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN RETURN NEW; END IF;
    authority := (NEW.worker_id IS NOT NULL AND NEW.worker_id IS DISTINCT FROM OLD.worker_id)
        OR NEW.claim_generation IS DISTINCT FROM OLD.claim_generation
        OR (OLD.claim_released_at IS NOT NULL AND NEW.claim_released_at IS NULL)
        OR (NEW.status IS DISTINCT FROM OLD.status AND OLD.status NOT IN ('claimed','running') AND NEW.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup'));
    IF NOT authority THEN RETURN NEW; END IF;
    IF NOT EXISTS (SELECT 1 FROM run_publication_resume p WHERE p.run_id = NEW.id AND p.state = 'active') THEN
        RETURN NEW;
    END IF;
    IF current_setting('uzi.publication_resume_api', true) IS DISTINCT FROM 'v1'
       OR NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id AND w.user_id = NEW.user_id
                      AND 'publication_resume_v1' = ANY(w.protocol_capabilities))
    THEN
        RAISE EXCEPTION 'publication resume requires capable worker and updated API' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_publication_resume_trg BEFORE INSERT OR UPDATE ON runs
FOR EACH ROW EXECUTE FUNCTION guard_publication_resume();
-- +goose StatementEnd

-- Persisted meaning of the phase and hold reason is bounded by the park they describe. These
-- run in the database so an older API writer that leaves recovery_wait also clears them.
-- +goose StatementBegin
CREATE FUNCTION normalize_recovery_wait_phase() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status <> 'recovery_wait' OR NEW.recovery_wait_cause IS DISTINCT FROM 'forge_unreachable' THEN
        NEW.recovery_wait_phase := NULL;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER normalize_recovery_wait_phase_trg BEFORE UPDATE ON runs
FOR EACH ROW WHEN (NEW.recovery_wait_phase IS NOT NULL)
EXECUTE FUNCTION normalize_recovery_wait_phase();

CREATE FUNCTION clear_publication_hold_reason() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE run_publication_resume p SET hold_reason = NULL
    WHERE p.run_id = NEW.id AND p.state = 'active' AND p.hold_reason IS NOT NULL;
    RETURN NULL;
END;
$$;
CREATE TRIGGER clear_publication_hold_reason_trg AFTER UPDATE ON runs
FOR EACH ROW
WHEN (NEW.status <> 'recovery_wait' OR NEW.recovery_wait_cause IS DISTINCT FROM 'forge_unreachable')
EXECUTE FUNCTION clear_publication_hold_reason();
-- +goose StatementEnd

-- Identity, state and candidate writes need the API opt-in. The hold reason, wait clock, counters
-- and handoff are exempt so the normaliser above never raises under an older writer.
-- +goose StatementBegin
CREATE FUNCTION guard_publication_resume_row() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    opted boolean := current_setting('uzi.publication_resume_api', true) IS NOT DISTINCT FROM 'v1';
    field text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT opted OR NEW.state <> 'active' THEN
            RAISE EXCEPTION 'publication resume enrollment requires updated API' USING ERRCODE = '23514';
        END IF;
        IF NEW.intent->>'run_id' IS DISTINCT FROM NEW.run_id::text THEN
            RAISE EXCEPTION 'publication intent run mismatch' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.run_id <> OLD.run_id OR NEW.enrollment_seq <> OLD.enrollment_seq THEN
        RAISE EXCEPTION 'publication resume key is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.claim_generation IS DISTINCT FROM OLD.claim_generation THEN
        IF NOT opted THEN
            RAISE EXCEPTION 'publication resume identity requires updated API' USING ERRCODE = '23514';
        END IF;
    END IF;
    IF NEW.state IS DISTINCT FROM OLD.state THEN
        IF NOT opted OR OLD.state <> 'active' OR NEW.state NOT IN ('retired', 'completed') THEN
            RAISE EXCEPTION 'publication resume state change refused' USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.state <> 'active'
          AND (NEW.intent IS DISTINCT FROM OLD.intent OR NEW.candidate_seq IS DISTINCT FROM OLD.candidate_seq
               OR NEW.claim_generation IS DISTINCT FROM OLD.claim_generation) THEN
        RAISE EXCEPTION 'inactive publication resume is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.intent IS DISTINCT FROM OLD.intent OR NEW.candidate_seq IS DISTINCT FROM OLD.candidate_seq THEN
        IF NOT opted THEN
            RAISE EXCEPTION 'publication candidate update requires updated API' USING ERRCODE = '23514';
        END IF;
        IF NEW.state = OLD.state AND NEW.candidate_seq <= OLD.candidate_seq THEN
            RAISE EXCEPTION 'publication candidate_seq must increase' USING ERRCODE = '23514';
        END IF;
        FOREACH field IN ARRAY ARRAY['run_id', 'repo_id', 'branch', 'claim_generation', 'original_head', 'floor'] LOOP
            IF NEW.intent->field IS DISTINCT FROM OLD.intent->field THEN
                RAISE EXCEPTION 'publication intent identity is immutable' USING ERRCODE = '23514';
            END IF;
        END LOOP;
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_publication_resume_row_trg BEFORE INSERT OR UPDATE ON run_publication_resume
FOR EACH ROW EXECUTE FUNCTION guard_publication_resume_row();
-- +goose StatementEnd

-- +goose Down
-- Exclude concurrent enrollment before checking for enrolled runs. The next statement takes a
-- fresh READ COMMITTED snapshot after this lock is acquired.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
    -- A stricter isolation level could retain a snapshot from before the lock.
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'publication resume schema downgrade requires READ COMMITTED';
    END IF;
    IF EXISTS (SELECT 1 FROM run_publication_resume p JOIN runs r ON r.id = p.run_id
               WHERE p.state = 'active' OR r.status NOT IN ('completed', 'failed', 'cancelled')) THEN
        RAISE EXCEPTION 'cannot downgrade with enrolled publication resume';
    END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER clear_publication_hold_reason_trg ON runs;
DROP TRIGGER normalize_recovery_wait_phase_trg ON runs;
DROP TRIGGER guard_publication_resume_trg ON runs;
DROP FUNCTION clear_publication_hold_reason();
DROP FUNCTION normalize_recovery_wait_phase();
DROP FUNCTION guard_publication_resume();
DROP TABLE run_publication_resume;
DROP FUNCTION guard_publication_resume_row();
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_phase_check;
ALTER TABLE runs DROP COLUMN recovery_wait_phase;
