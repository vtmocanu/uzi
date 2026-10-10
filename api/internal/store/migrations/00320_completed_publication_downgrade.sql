-- +goose Up
-- A capability downgrade completes normally without stamping publication evidence.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION completed_publication_run_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.completion_final_head IS NOT NULL THEN RETURN NULL; END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'completed' AND NEW.status = 'completed'
        AND NEW.completion_final_head IS DISTINCT FROM OLD.completion_final_head THEN RETURN NULL; END IF;
    IF NEW.status <> 'completed' THEN NEW.completion_final_head := NULL; END IF;
    IF NEW.completion_final_head IS NOT NULL AND OLD.status <> 'completed' THEN
        IF NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id AND w.user_id = NEW.user_id
            AND 'recovery_completed_publication_v1' = ANY(w.protocol_capabilities)) THEN
            NEW.completion_final_head := NULL;
            RETURN NEW;
        END IF;
        IF NEW.completion_contract_version IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM run_completion_permits p WHERE p.run_id = NEW.id
                AND p.issued_by_worker_id = NEW.worker_id AND p.head = NEW.completion_final_head
                AND p.consumed_at IS NOT NULL AND p.contract_revision = NEW.contract_revision
        ) THEN RETURN NULL; END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Restore the guard installed by migration 00318; existing evidence is unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION completed_publication_run_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.completion_final_head IS NOT NULL THEN RETURN NULL; END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'completed' AND NEW.status = 'completed'
        AND NEW.completion_final_head IS DISTINCT FROM OLD.completion_final_head THEN RETURN NULL; END IF;
    IF NEW.status <> 'completed' THEN NEW.completion_final_head := NULL; END IF;
    IF NEW.completion_final_head IS NOT NULL AND OLD.status <> 'completed' THEN
        IF NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id AND w.user_id = NEW.user_id
            AND 'recovery_completed_publication_v1' = ANY(w.protocol_capabilities)) THEN
            NEW.completion_final_head := NULL;
        END IF;
        IF NEW.completion_contract_version IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM run_completion_permits p WHERE p.run_id = NEW.id
                AND p.issued_by_worker_id = NEW.worker_id AND p.head = NEW.completion_final_head
                AND p.consumed_at IS NOT NULL AND p.contract_revision = NEW.contract_revision
        ) THEN RETURN NULL; END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
