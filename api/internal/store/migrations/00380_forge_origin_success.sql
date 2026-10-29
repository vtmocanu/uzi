-- +goose Up
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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forge_origin_reservation_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
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
